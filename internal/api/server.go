// Package api implements the protobuffet HTTP server: the publish/read API,
// the package-manager endpoints and a small browsing UI.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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

// maxUpload caps a publish request. Schema modules and generated SDKs are
// small; anything larger is a mistake or an attack.
const maxUpload = 128 << 20

// Server wires the store, the configuration and the HTTP routes together.
type Server struct {
	cfg     config.Server
	catalog *registry.Catalog
	buf     *buftool.Buf
	log     *slog.Logger
	mux     *http.ServeMux
}

// New builds a server. In the default "required" mode a missing buf binary is
// fatal: a registry that cannot check compatibility silently stops protecting
// consumers, and that is worse than not starting.
func New(cfg config.Server, catalog *registry.Catalog, log *slog.Logger) (*Server, error) {
	s := &Server{cfg: cfg, catalog: catalog, log: log, mux: http.NewServeMux()}
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

	s.mux.Handle("GET /m/{owner}/{name}", read(http.HandlerFunc(s.uiModule)))
	// Go module paths live under this prefix by convention; discovery for
	// them, and for any other path, is handled by the catch-all below.
	s.mux.Handle("GET /gen/", read(http.HandlerFunc(s.ui)))
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n")) //nolint:errcheck // a failed write means the client is gone
	})
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
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

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
	out := make([]entry, 0, len(mods))
	for _, m := range mods {
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
	writeJSON(w, http.StatusOK, out)
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
	writeJSON(w, http.StatusOK, map[string]string{"label": r.PathValue("label"), "version": body.Version})
}

func (s *Server) publish(w http.ResponseWriter, r *http.Request) {
	m, err := moduleFrom(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
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
		httpError(w, err)
		return
	}
	s.log.Info("published", "module", m.String(), "version", version.Version, "by", subject)
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
	case errors.Is(err, registry.ErrVersionExists):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, registry.ErrDigestMismatch):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
