// Package gomodproxy serves the Go module proxy protocol over the registry,
// so `go get` treats a published schema as an ordinary module.
//
// Protocol reference: https://go.dev/ref/mod#goproxy-protocol
package gomodproxy

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/mod/module"

	"github.com/smoreg/protobuffet/internal/registry"
)

// Handler serves the proxy endpoints. Mount it under a prefix, for example
// http.StripPrefix("/go", h).
type Handler struct {
	Catalog *registry.Catalog
}

// New builds a handler.
func New(catalog *registry.Catalog) *Handler { return &Handler{Catalog: catalog} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")

	if rest, ok := cutSuffix(p, "/@latest"); ok {
		h.serveLatest(w, rest)
		return
	}
	escaped, file, ok := cut(p, "/@v/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	modPath, err := module.UnescapePath(escaped)
	if err != nil {
		http.Error(w, fmt.Sprintf("bad module path: %v", err), http.StatusBadRequest)
		return
	}

	switch {
	case file == "list":
		h.serveList(w, modPath)
	case strings.HasSuffix(file, ".info"):
		h.serveInfo(w, modPath, strings.TrimSuffix(file, ".info"))
	case strings.HasSuffix(file, ".mod"):
		h.serveMod(w, modPath, strings.TrimSuffix(file, ".mod"))
	case strings.HasSuffix(file, ".zip"):
		h.serveZip(w, r, modPath, strings.TrimSuffix(file, ".zip"))
	default:
		http.NotFound(w, r)
	}
}

// lookup finds the module that publishes a Go module path, along with the
// versions that actually carry a Go artifact.
func (h *Handler) lookup(modPath string) (registry.Module, []registry.Version, error) {
	mod, idx, err := h.Catalog.FindByTarget(registry.TargetGo, modPath)
	if err != nil {
		return registry.Module{}, nil, err
	}
	var out []registry.Version
	for _, v := range idx.Versions {
		if a, ok := v.Artifact(registry.TargetGo); ok && a.Name == modPath {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return mod, nil, fmt.Errorf("module %q: %w", modPath, registry.ErrNotFound)
	}
	return mod, out, nil
}

func (h *Handler) serveList(w http.ResponseWriter, modPath string) {
	_, versions, err := h.lookup(modPath)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, v := range versions {
		// Pseudo-versions are resolvable but must not be listed: the go
		// command treats listed versions as releases and would pick one over
		// a tagged release. Yanked versions are hidden for the same reason,
		// while staying downloadable for anyone who already pinned them.
		if registry.IsPseudo(v.Version) || v.Yanked {
			continue
		}
		fmt.Fprintln(w, v.Version) //nolint:errcheck // a failed write means the client is gone
	}
}

func (h *Handler) serveLatest(w http.ResponseWriter, escaped string) {
	modPath, err := module.UnescapePath(escaped)
	if err != nil {
		http.Error(w, fmt.Sprintf("bad module path: %v", err), http.StatusBadRequest)
		return
	}
	_, versions, err := h.lookup(modPath)
	if err != nil {
		writeErr(w, err)
		return
	}
	var latest registry.Version
	var found bool
	for i := len(versions) - 1; i >= 0; i-- {
		if !versions[i].Yanked {
			latest, found = versions[i], true
			break
		}
	}
	if !found {
		writeErr(w, fmt.Errorf("module %q: every version is yanked: %w", modPath, registry.ErrNotFound))
		return
	}
	writeJSON(w, info{Version: latest.Version, Time: latest.CreatedAt})
}

func (h *Handler) resolve(modPath, escapedVersion string) (registry.Module, registry.Version, error) {
	version, err := module.UnescapeVersion(escapedVersion)
	if err != nil {
		return registry.Module{}, registry.Version{}, fmt.Errorf("bad version: %w", err)
	}
	mod, versions, err := h.lookup(modPath)
	if err != nil {
		return registry.Module{}, registry.Version{}, err
	}
	for _, v := range versions {
		if v.Version == version {
			return mod, v, nil
		}
	}
	return mod, registry.Version{}, fmt.Errorf("%s@%s: %w", modPath, version, registry.ErrNotFound)
}

func (h *Handler) serveInfo(w http.ResponseWriter, modPath, escapedVersion string) {
	_, v, err := h.resolve(modPath, escapedVersion)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, info{Version: v.Version, Time: v.CreatedAt})
}

func (h *Handler) serveMod(w http.ResponseWriter, modPath, escapedVersion string) {
	mod, v, err := h.resolve(modPath, escapedVersion)
	if err != nil {
		writeErr(w, err)
		return
	}
	a, _ := v.Artifact(registry.TargetGo)
	body, err := h.Catalog.ReadAll(mod, v.Version, a.File)
	if err != nil {
		writeErr(w, err)
		return
	}
	// The go.mod is inside the module zip by definition, so it is read from
	// there instead of being stored twice.
	gomod, err := modFromZip(body, modPath, v.Version)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// The body is a go.mod file, served verbatim as the proxy protocol
	// requires; the server sends X-Content-Type-Options: nosniff.
	w.Write(gomod) //nolint:errcheck,gosec // verbatim go.mod, text/plain + nosniff
}

func (h *Handler) serveZip(w http.ResponseWriter, r *http.Request, modPath, escapedVersion string) {
	mod, v, err := h.resolve(modPath, escapedVersion)
	if err != nil {
		writeErr(w, err)
		return
	}
	a, _ := v.Artifact(registry.TargetGo)
	f, size, err := h.Catalog.Open(mod, v.Version, a.File)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", fmt.Sprint(size))
	if a.SHA256 != "" {
		w.Header().Set("ETag", `"`+a.SHA256+`"`)
	}
	http.ServeContent(w, r, a.File, v.CreatedAt, f)
}

// modFromZip extracts go.mod from a module zip.
func modFromZip(data []byte, modPath, version string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	want := modPath + "@" + version + "/go.mod"
	for _, f := range zr.File {
		if f.Name == want {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("go.mod missing from module zip %s@%s", modPath, version)
}

type info struct {
	Version string    `json:"Version"`
	Time    time.Time `json:"Time"`
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck // a failed write means the client is gone
}

func writeErr(w http.ResponseWriter, err error) {
	// The message can contain a module path taken from the URL, so it is
	// escaped: http.Error sets a text content type, but a browser sniffing
	// the body should still not find markup.
	msg := html.EscapeString(err.Error())
	if errors.Is(err, registry.ErrNotFound) {
		http.Error(w, msg, http.StatusNotFound)
		return
	}
	http.Error(w, msg, http.StatusInternalServerError)
}

func cut(s, sep string) (before, after string, found bool) {
	return strings.Cut(s, sep)
}

func cutSuffix(s, suffix string) (string, bool) {
	if strings.HasSuffix(s, suffix) {
		return strings.TrimSuffix(s, suffix), true
	}
	return s, false
}
