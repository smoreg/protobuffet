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
	"github.com/smoreg/protobuffet/internal/registry"
)

// maxUpload caps a publish request. Schema modules and generated SDKs are
// small; anything larger is a mistake or an attack.
const maxUpload = 128 << 20

// Server wires the store, the configuration and the HTTP routes together.
type Server struct {
	cfg   config.Server
	store *registry.Store
	buf   *buftool.Buf
	log   *slog.Logger
	mux   *http.ServeMux
}

// New builds a server. A missing buf binary is not fatal: the server degrades
// to accepting pushes without a compatibility check and says so in the log.
func New(cfg config.Server, store *registry.Store, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, store: store, log: log, mux: http.NewServeMux()}
	if cfg.BreakingCheck {
		b, err := buftool.Find(cfg.BufPath)
		if err != nil {
			log.Warn("compatibility checks disabled: buf not available", "err", err)
		} else {
			s.buf = b
			log.Info("compatibility checks enabled", "buf", b.Path())
		}
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	read := func(h http.Handler) http.Handler { return s.requireScope("read", h) }
	write := func(h http.HandlerFunc) http.Handler { return s.requireScope("write", h) }

	s.mux.Handle("GET /go/", read(http.StripPrefix("/go/", gomodproxy.New(s.store))))
	s.mux.Handle("GET /composer/", read(http.StripPrefix("/composer", composerrepo.New(s.store, s.cfg.BaseURL))))
	s.mux.Handle("GET /npm/", read(npmrepo.New(s.store, s.cfg.BaseURL)))

	s.mux.Handle("GET /api/v1/modules", read(http.HandlerFunc(s.listModules)))
	s.mux.Handle("GET /api/v1/modules/{owner}/{name}", read(http.HandlerFunc(s.getModule)))
	s.mux.Handle("GET /api/v1/modules/{owner}/{name}/versions/{version}", read(http.HandlerFunc(s.getVersion)))
	s.mux.Handle("GET /api/v1/modules/{owner}/{name}/versions/{version}/schema.zip", read(http.HandlerFunc(s.getSchema)))
	s.mux.Handle("POST /api/v1/modules/{owner}/{name}/versions", write(s.publish))
	s.mux.Handle("PUT /api/v1/modules/{owner}/{name}/labels/{label}", write(s.setLabel))

	s.mux.Handle("GET /m/{owner}/{name}", read(http.HandlerFunc(s.uiModule)))
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
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
	mods, err := s.store.ListModules()
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
		idx, err := s.store.Index(m)
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
	idx, err := s.store.Index(m)
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
	v, err := s.store.Resolve(m, r.PathValue("version"))
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
	v, err := s.store.Resolve(m, r.PathValue("version"))
	if err != nil {
		httpError(w, err)
		return
	}
	f, size, err := s.store.Open(m, v.Version, "schema.zip")
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
	if err := s.store.SetLabel(m, r.PathValue("label"), body.Version); err != nil {
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
	defer r.MultipartForm.RemoveAll()

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

	report, err := s.checkBreaking(r.Context(), m, schema)
	if err != nil {
		httpError(w, err)
		return
	}
	if report != nil && report.Findings != "" {
		writeJSON(w, http.StatusConflict, PublishResponse{Module: m.String(), Breaking: report})
		return
	}

	subject := ""
	if tok, ok := tokenFrom(r); ok {
		subject = tok.Subject
	}
	version, err := s.store.Publish(registry.PublishRequest{
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

// checkBreaking compares an incoming schema archive with the latest published
// one. It returns a report; findings mean the push must be refused.
func (s *Server) checkBreaking(ctx context.Context, m registry.Module, schema []byte) (*BreakingReport, error) {
	if s.buf == nil {
		return &BreakingReport{Skipped: "buf binary unavailable"}, nil
	}
	idx, err := s.store.Index(m)
	if errors.Is(err, registry.ErrNotFound) {
		return &BreakingReport{Skipped: "first version of this module"}, nil
	}
	if err != nil {
		return nil, err
	}
	prev, ok := idx.Latest()
	if !ok {
		return &BreakingReport{Skipped: "first version of this module"}, nil
	}
	prevZip, err := s.store.ReadAll(m, prev.Version, "schema.zip")
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "buffet-breaking-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

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
		// A check that cannot run must not silently pass, but it must not
		// take the registry down either: report it and let the push through.
		s.log.Warn("compatibility check failed to run", "module", m.String(), "err", err)
		return &BreakingReport{Skipped: "check could not run: " + err.Error()}, nil
	}
	report := &BreakingReport{Checked: true, Against: prev.Version}
	if !res.Compatible {
		report.Findings = res.Output
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
	enc.Encode(v)
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
