// Package api implements the protobuffet HTTP server: the publish/read API,
// the package-manager endpoints and a small browsing UI.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/smoreg/protobuffet/internal/buftool"
	"github.com/smoreg/protobuffet/internal/composerrepo"
	"github.com/smoreg/protobuffet/internal/config"
	"github.com/smoreg/protobuffet/internal/gomodproxy"
	"github.com/smoreg/protobuffet/internal/npmrepo"
	"github.com/smoreg/protobuffet/internal/pypirepo"
	"github.com/smoreg/protobuffet/internal/registry"
	"github.com/smoreg/protobuffet/internal/verify"
)

// Server wires the store, the configuration and the HTTP routes together.
type Server struct {
	cfg     config.Server
	catalog *registry.Catalog
	buf     *buftool.Buf
	log     *slog.Logger
	mux     *http.ServeMux
	metrics *metrics
	// publishing bounds concurrent publishes: each one holds archives in
	// memory and runs buf.
	publishing chan struct{}
	audit      *auditLog
	// tokens holds the live credential set, swapped on reload.
	tokens atomic.Pointer[[]config.Token]
}

// New builds a server. In the default "required" mode a missing buf binary is
// fatal: a registry that cannot check compatibility silently stops protecting
// consumers, and that is worse than not starting.
func New(cfg config.Server, catalog *registry.Catalog, log *slog.Logger) (*Server, error) {
	s := &Server{
		cfg: cfg, catalog: catalog, log: log,
		mux: http.NewServeMux(), metrics: newMetrics(),
		publishing: make(chan struct{}, cfg.MaxConcurrentPublishes),
		// The audit log belongs next to the data it describes, so it moves
		// with the volume and cannot drift from the store's location.
		audit: newAuditLog(catalog.Root()),
	}
	tokens := cfg.Tokens
	s.tokens.Store(&tokens)
	if cfg.BreakingCheck.Enabled() {
		b, err := buftool.Find(cfg.BufPath)
		switch {
		case err == nil:
			s.buf = b
			log.Info("compatibility checks enabled", "mode", cfg.BreakingCheck, "buf", b.Path())
		case cfg.BreakingCheck == config.BreakingRequired:
			return nil, fmt.Errorf("breaking_check is %q but buf is unavailable: %w (install buf, set buf_path, or lower breaking_check)",
				cfg.BreakingCheck, err)
		default:
			log.Warn("compatibility checks degraded: buf not available", "mode", cfg.BreakingCheck, "err", err)
		}
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	read := func(h http.Handler) http.Handler { return s.requireScope("read", h) }
	write := func(h http.HandlerFunc) http.Handler { return s.requireScope("write", h) }
	admin := func(h http.HandlerFunc) http.Handler { return s.requireScope("admin", h) }

	s.mux.Handle("GET /go/", read(http.StripPrefix("/go/", gomodproxy.New(s.catalog))))
	s.mux.Handle("GET /composer/", read(http.StripPrefix("/composer", composerrepo.New(s.catalog, s.cfg.BaseURL))))
	s.mux.Handle("GET /npm/", read(npmrepo.New(s.catalog, s.cfg.BaseURL)))
	s.mux.Handle("GET /pypi/", read(pypirepo.New(s.catalog, s.cfg.BaseURL)))

	s.mux.Handle("GET /api/v1/modules", read(http.HandlerFunc(s.listModules)))
	s.mux.Handle("GET /api/v1/modules/{owner}/{name}", read(http.HandlerFunc(s.getModule)))
	s.mux.Handle("GET /api/v1/modules/{owner}/{name}/versions/{version}", read(http.HandlerFunc(s.getVersion)))
	s.mux.Handle("GET /api/v1/modules/{owner}/{name}/versions/{version}/schema.zip", read(http.HandlerFunc(s.getSchema)))
	s.mux.Handle("POST /api/v1/modules/{owner}/{name}/versions", write(s.publish))
	s.mux.Handle("PUT /api/v1/modules/{owner}/{name}/labels/{label}", write(s.setLabel))
	// Yanking hides a bad release without breaking anyone who pinned it, so
	// it is an admin action rather than a publish-time one.
	s.mux.Handle("PUT /api/v1/modules/{owner}/{name}/versions/{version}/yank", admin(s.yank))
	s.mux.Handle("DELETE /api/v1/modules/{owner}/{name}/versions/{version}/yank", admin(s.unyank))

	s.mux.Handle("GET /m/{owner}/{name}", read(http.HandlerFunc(s.uiModule)))
	// Go module paths live under this prefix by convention; discovery for
	// them, and for any other path, is handled by the catch-all below.
	s.mux.Handle("GET /gen/", read(http.HandlerFunc(s.ui)))
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n")) //nolint:errcheck // a failed write means the client is gone
	})
	s.mux.Handle("GET /metrics", read(http.HandlerFunc(s.serveMetrics)))
	s.mux.Handle("GET /api/v1/audit", admin(s.serveAudit))
	// Collecting through the API keeps it in the process that owns the data
	// directory, so the in-memory catalog is refreshed and no second process
	// competes for the lock.
	s.mux.Handle("POST /api/v1/gc", admin(s.collectGarbage))
	s.mux.Handle("GET /", read(http.HandlerFunc(s.ui)))
}

// Handler returns the root handler with request logging.
func (s *Server) Handler() http.Handler {
	return s.logging(s.mux)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Responses carry file contents under a declared content type; tell
		// browsers not to second-guess it.
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// A request id makes one publish traceable across the client log, the
		// server log and whatever sits in front. An inbound id is only
		// honoured behind a trusted proxy: otherwise a client could pick its
		// own and make two unrelated requests look like one in the audit log.
		reqID := ""
		if s.cfg.TrustedProxy {
			reqID = sanitizeHeader(r.Header.Get("X-Request-Id"))
		}
		if reqID == "" {
			reqID = newRequestID()
		}
		w.Header().Set("X-Request-Id", reqID)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), requestIDKey, reqID)))

		elapsed := time.Since(start)
		s.metrics.observe(route(r.URL.Path), rec.status, elapsed)
		s.log.Info("request",
			"request_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", elapsed.Round(time.Millisecond),
			"remote", s.remoteAddr(r),
		)
	})
}

// collectGarbage applies the retention policy in-process.
func (s *Server) collectGarbage(w http.ResponseWriter, r *http.Request) {
	dryRun := r.URL.Query().Get("dry_run") == "1"
	pruned, err := s.catalog.PrunePseudoVersions(registry.PruneOptions{
		KeepNewest: s.cfg.Retention.KeepPseudoVersions,
		MaxAge:     time.Duration(s.cfg.Retention.KeepPseudoDays) * 24 * time.Hour,
		DryRun:     dryRun,
	})
	if err != nil {
		httpError(w, err)
		return
	}
	type entry struct {
		Module  string `json:"module"`
		Version string `json:"version"`
		Reason  string `json:"reason"`
	}
	out := make([]entry, 0, len(pruned))
	for _, p := range pruned {
		out = append(out, entry{Module: p.Module.String(), Version: p.Version, Reason: p.Reason})
	}
	if !dryRun && len(pruned) > 0 {
		s.recordAudit(r, auditEvent{
			Subject: subjectOf(r), Action: "gc", Result: "ok",
			Detail: fmt.Sprintf("pruned %d pseudo-versions", len(pruned)),
		})
	}
	writeJSON(w, http.StatusOK, struct {
		DryRun bool    `json:"dry_run"`
		Pruned []entry `json:"pruned"`
	}{DryRun: dryRun, Pruned: out})
}

// serveAudit returns the tail of the audit log, newest first.
func (s *Server) serveAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	events, err := s.audit.tail(limit)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// recordAudit writes one event, filling in what the request knows.
func (s *Server) recordAudit(r *http.Request, e auditEvent) {
	// The id assigned by the middleware, never the raw header: the audit log
	// answers "who did this, from where", and a client must not author it.
	e.RequestID = requestIDFrom(r)
	e.Remote = s.remoteAddr(r)
	if err := s.audit.record(e); err != nil {
		// The action already happened; losing its audit line is worth a loud
		// log, not a failed request.
		s.log.Error("audit write failed", "action", e.Action, "module", e.Module, "err", err)
	}
}

func subjectOf(r *http.Request) string {
	if tok, ok := tokenFrom(r); ok {
		return tok.Subject
	}
	return ""
}

func yankAction(yanked bool) string {
	if yanked {
		return "yank"
	}
	return "unyank"
}

func auditBreakingDetail(report *BreakingReport) string {
	switch {
	case report == nil:
		return ""
	case report.Overridden != "":
		return "breaking, overridden: " + report.Overridden
	case report.Checked:
		return "checked against " + report.Against
	default:
		return report.Skipped
	}
}

// serveMetrics renders counters plus two gauges taken from the catalog.
func (s *Server) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	var modules, versions int64
	if mods, err := s.catalog.ListModules(); err == nil {
		modules = int64(len(mods))
		for _, m := range mods {
			if idx, err := s.catalog.Index(m); err == nil {
				versions += int64(len(idx.Versions))
			}
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.writeTo(w, modules, versions)
}

// newRequestID returns a short random identifier.
func newRequestID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(buf[:])
}

// remoteAddr reports where a request came from. The forwarded header is only
// read when a proxy is declared in front; taken from an arbitrary client it is
// a free-text field that ends up in the audit log.
func (s *Server) remoteAddr(r *http.Request) string {
	if s.cfg.TrustedProxy {
		if v := sanitizeHeader(r.Header.Get("X-Forwarded-For")); v != "" {
			if i := strings.IndexByte(v, ','); i > 0 {
				return strings.TrimSpace(v[:i])
			}
			return v
		}
	}
	return r.RemoteAddr
}

// sanitizeHeader keeps a header value printable and short, so a forged one
// cannot inject newlines into a log line or bloat the audit file.
func sanitizeHeader(v string) string {
	if len(v) > 200 {
		v = v[:200]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(v))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// listLimit bounds a page. Without it, a registry with thousands of modules
// answers every listing with megabytes of JSON.
const (
	defaultListLimit = 100
	maxListLimit     = 1000
)

func (s *Server) listModules(w http.ResponseWriter, r *http.Request) {
	mods, err := s.catalog.ListModules()
	if err != nil {
		httpError(w, err)
		return
	}
	type entry struct {
		Module   string    `json:"module"`
		Versions int       `json:"versions"`
		Latest   string    `json:"latest,omitempty"`
		Updated  time.Time `json:"updated"`
	}
	limit := listLimit(r)
	cursor := r.URL.Query().Get("cursor")

	out := make([]entry, 0, limit)
	next := ""
	for _, m := range mods {
		// Modules are sorted by name, so the cursor is simply the name to
		// resume after.
		if cursor != "" && m.String() <= cursor {
			continue
		}
		if len(out) == limit {
			next = out[len(out)-1].Module
			break
		}
		idx, err := s.catalog.Index(m)
		if err != nil {
			continue
		}
		e := entry{Module: m.String(), Versions: len(idx.Versions), Updated: idx.Updated}
		if v, ok := idx.Latest(); ok {
			e.Latest = v.Version
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, struct {
		Modules []entry `json:"modules"`
		Next    string  `json:"next,omitempty"`
	}{Modules: out, Next: next})
}

func listLimit(r *http.Request) int {
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return min(n, maxListLimit)
		}
	}
	return defaultListLimit
}

func moduleFrom(r *http.Request) (registry.Module, error) {
	return registry.ParseModule(r.PathValue("owner") + "/" + r.PathValue("name"))
}

func (s *Server) getModule(w http.ResponseWriter, r *http.Request) {
	m, err := moduleFrom(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	idx, err := s.catalog.Index(m)
	if err != nil {
		httpError(w, err)
		return
	}
	// A module with thousands of CI pushes has a large version list; callers
	// that only need the labels can skip it.
	if r.URL.Query().Get("versions") == "none" {
		writeJSON(w, http.StatusOK, struct {
			Module   registry.Module   `json:"module"`
			Labels   map[string]string `json:"labels,omitempty"`
			Updated  time.Time         `json:"updated"`
			Versions int               `json:"version_count"`
		}{Module: idx.Module, Labels: idx.Labels, Updated: idx.Updated, Versions: len(idx.Versions)})
		return
	}
	if limit := listLimit(r); len(idx.Versions) > limit {
		trimmed := *idx
		// Newest versions are what a caller paging through wants first.
		trimmed.Versions = idx.Versions[len(idx.Versions)-limit:]
		writeJSON(w, http.StatusOK, trimmed)
		return
	}
	writeJSON(w, http.StatusOK, idx)
}

func (s *Server) getVersion(w http.ResponseWriter, r *http.Request) {
	m, err := moduleFrom(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v, err := s.catalog.Resolve(m, r.PathValue("version"))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) getSchema(w http.ResponseWriter, r *http.Request) {
	m, err := moduleFrom(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v, err := s.catalog.Resolve(m, r.PathValue("version"))
	if err != nil {
		httpError(w, err)
		return
	}
	f, size, err := s.catalog.Open(m, v.Version, "schema.zip")
	if err != nil {
		httpError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", fmt.Sprint(size))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", m.Name+"-"+v.Version+".zip"))
	http.ServeContent(w, r, "schema.zip", v.CreatedAt, f)
}

func (s *Server) setLabel(w http.ResponseWriter, r *http.Request) {
	m, err := moduleFrom(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.catalog.SetLabel(m, r.PathValue("label"), body.Version); err != nil {
		httpError(w, err)
		return
	}
	s.recordAudit(r, auditEvent{
		Subject: subjectOf(r), Action: "label", Module: m.String(),
		Version: body.Version, Label: r.PathValue("label"), Result: "ok",
	})
	writeJSON(w, http.StatusOK, map[string]string{"label": r.PathValue("label"), "version": body.Version})
}

func (s *Server) yank(w http.ResponseWriter, r *http.Request) {
	s.setYanked(w, r, true)
}

func (s *Server) unyank(w http.ResponseWriter, r *http.Request) {
	s.setYanked(w, r, false)
}

func (s *Server) setYanked(w http.ResponseWriter, r *http.Request, yanked bool) {
	m, err := moduleFrom(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	subject := ""
	if tok, ok := tokenFrom(r); ok {
		subject = tok.Subject
	}
	version := r.PathValue("version")
	if err := s.catalog.SetYanked(m, version, yanked, body.Reason, subject, time.Now()); err != nil {
		httpError(w, err)
		return
	}
	s.recordAudit(r, auditEvent{
		Subject: subject, Action: yankAction(yanked), Module: m.String(),
		Version: version, Result: "ok", Detail: body.Reason,
	})
	s.log.Warn("version yank state changed",
		"module", m.String(), "version", version, "yanked", yanked, "by", subject, "reason", body.Reason)
	writeJSON(w, http.StatusOK, map[string]any{
		"module": m.String(), "version": version, "yanked": yanked, "reason": body.Reason,
	})
}

func (s *Server) publish(w http.ResponseWriter, r *http.Request) {
	m, err := moduleFrom(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	select {
	case s.publishing <- struct{}{}:
		defer func() { <-s.publishing }()
	case <-r.Context().Done():
		http.Error(w, "client went away", http.StatusRequestTimeout)
		return
	case <-time.After(30 * time.Second):
		// Shedding load beats queueing until the node runs out of memory.
		w.Header().Set("Retry-After", "30")
		http.Error(w, "too many concurrent publishes", http.StatusServiceUnavailable)
		s.metrics.publish("throttled")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "invalid multipart body: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll() //nolint:errcheck // temp files are cleaned by the OS if this fails

	var man PublishManifest
	if err := json.Unmarshal([]byte(r.FormValue("manifest")), &man); err != nil {
		http.Error(w, "invalid manifest: "+err.Error(), http.StatusBadRequest)
		return
	}
	if man.Module != m.String() {
		http.Error(w, fmt.Sprintf("manifest module %q does not match path %q", man.Module, m), http.StatusBadRequest)
		return
	}

	files, err := readUploads(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	schema, ok := files["schema.zip"]
	if !ok {
		http.Error(w, "schema.zip is required", http.StatusBadRequest)
		return
	}
	// A version with no SDKs is downloadable by nobody; accepting it burns a
	// version number that can never be republished.
	if len(man.Artifacts) == 0 {
		http.Error(w, "at least one artifact is required", http.StatusBadRequest)
		return
	}
	if man.SchemaSHA256 != "" && man.SchemaSHA256 != registry.SHA256Hex(schema) {
		http.Error(w, "schema.zip digest mismatch", http.StatusBadRequest)
		return
	}

	// Reject a malformed artifact before it becomes an immutable version.
	for _, a := range man.Artifacts {
		body, ok := files[a.File]
		if !ok {
			http.Error(w, fmt.Sprintf("artifact %s: file %q missing from the request", a.Target, a.File),
				http.StatusBadRequest)
			return
		}
		if err := verify.Artifact(a, body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	if man.AllowBreaking {
		tok, ok := tokenFrom(r)
		if !ok || !tok.Can("admin") {
			http.Error(w, "allow_breaking requires an admin token", http.StatusForbidden)
			return
		}
	}

	wanted, err := registry.NormalizeVersion(man.Version)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	report, err := s.checkBreaking(r.Context(), m, wanted, schema, man.AllowBreaking)
	if err != nil {
		// In required mode a check that cannot run is a refusal, not a
		// warning: 503 tells the client to retry once the registry is healthy.
		s.log.Error("compatibility check failed", "module", m.String(), "err", err)
		http.Error(w, "compatibility check could not run: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if report != nil && report.Findings != "" && report.Overridden == "" {
		s.metrics.publish("breaking")
		s.recordAudit(r, auditEvent{
			Subject: subjectOf(r), Action: "publish", Module: m.String(),
			Version: man.Version, Result: "refused-breaking", Detail: report.Against,
		})
		writeJSON(w, http.StatusConflict, PublishResponse{Module: m.String(), Breaking: report})
		return
	}

	subject := ""
	if tok, ok := tokenFrom(r); ok {
		subject = tok.Subject
	}
	version, err := s.catalog.Publish(registry.PublishRequest{
		Module:      m,
		Version:     man.Version,
		SchemaZip:   schema,
		Artifacts:   man.Artifacts,
		Files:       files,
		Labels:      man.Labels,
		PublishedBy: subject,
	})
	if err != nil {
		s.metrics.publish("error")
		httpError(w, err)
		return
	}
	s.metrics.publish("ok")
	s.log.Info("published", "module", m.String(), "version", version.Version, "by", subject)
	s.recordAudit(r, auditEvent{
		Subject: subject, Action: "publish", Module: m.String(),
		Version: version.Version, Result: "ok", Detail: auditBreakingDetail(report),
	})
	writeJSON(w, http.StatusCreated, PublishResponse{Module: m.String(), Version: version, Breaking: report})
}

// checkBreaking compares an incoming schema archive with the version that
// precedes it. A report carrying findings means the push must be refused,
// unless policy or an admin override says otherwise.
func (s *Server) checkBreaking(ctx context.Context, m registry.Module, version string, schema []byte, allowBreaking bool) (*BreakingReport, error) {
	if !s.cfg.BreakingCheck.Enabled() {
		return &BreakingReport{Skipped: "breaking_check is off"}, nil
	}
	if s.buf == nil {
		if s.cfg.BreakingCheck == config.BreakingRequired {
			return nil, fmt.Errorf("buf binary unavailable")
		}
		s.log.Warn("publish not checked for compatibility", "module", m.String(), "reason", "buf unavailable")
		return &BreakingReport{Skipped: "buf binary unavailable"}, nil
	}
	idx, err := s.catalog.Index(m)
	if errors.Is(err, registry.ErrNotFound) {
		return &BreakingReport{Skipped: "first version of this module"}, nil
	}
	if err != nil {
		return nil, err
	}
	prev, ok := idx.Predecessor(version)
	if !ok {
		return &BreakingReport{Skipped: "first version of this module"}, nil
	}
	// Under allow-major, a major bump is where breaking changes are meant to
	// live, so the gate steps aside and says so in the report.
	if s.cfg.BreakingPolicy == config.PolicyAllowMajor &&
		registry.MajorOf(version) != registry.MajorOf(prev.Version) {
		return &BreakingReport{
			Against:    prev.Version,
			Skipped:    "major version bump under the allow-major policy",
			Overridden: "policy allow-major",
		}, nil
	}
	prevZip, err := s.catalog.ReadAll(m, prev.Version, "schema.zip")
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "buffet-breaking-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			s.log.Warn("clean up breaking-check workspace", "dir", dir, "err", err)
		}
	}()

	newPath := filepath.Join(dir, "new.zip")
	oldPath := filepath.Join(dir, "old.zip")
	if err := os.WriteFile(newPath, schema, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(oldPath, prevZip, 0o600); err != nil {
		return nil, err
	}

	res, err := s.buf.Breaking(ctx, dir, newPath, oldPath)
	if err != nil {
		if s.cfg.BreakingCheck == config.BreakingRequired {
			return nil, err
		}
		s.log.Warn("publish not checked for compatibility", "module", m.String(), "err", err)
		return &BreakingReport{Skipped: "check could not run: " + err.Error()}, nil
	}
	report := &BreakingReport{Checked: true, Against: prev.Version}
	if !res.Compatible {
		report.Findings = res.Output
		if allowBreaking {
			// Recorded rather than hidden: the version keeps the note, and so
			// does the log.
			report.Overridden = "admin override"
			s.log.Warn("breaking change published by override",
				"module", m.String(), "version", version, "against", prev.Version)
		}
	}
	return report, nil
}

func readUploads(r *http.Request) (map[string][]byte, error) {
	out := map[string][]byte{}
	for field, headers := range r.MultipartForm.File {
		for _, fh := range headers {
			f, err := fh.Open()
			if err != nil {
				return nil, err
			}
			buf := make([]byte, fh.Size)
			if _, err := io.ReadFull(f, buf); err != nil {
				f.Close()
				return nil, fmt.Errorf("read %s: %w", field, err)
			}
			f.Close()
			out[field] = buf
		}
	}
	return out, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v) //nolint:errcheck // a failed write means the client is gone
}

func httpError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, registry.ErrVersionExists), errors.Is(err, registry.ErrNameTaken):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, registry.ErrDigestMismatch), errors.Is(err, registry.ErrInvalid):
		// Bad input is the caller's problem; answering 500 sends them looking
		// at our logs for a fault that is in their request.
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
