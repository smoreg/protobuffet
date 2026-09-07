// Package npmrepo exposes published TypeScript/JavaScript SDKs as an npm
// registry, enough of it for `npm install --registry` to work:
// a packument per package and a tarball per version.
package npmrepo

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/smoreg/protobuffet/internal/registry"
)

// Handler serves package metadata and tarballs.
type Handler struct {
	Store   *registry.Store
	BaseURL string
}

// New builds a handler.
func New(store *registry.Store, baseURL string) *Handler {
	return &Handler{Store: store, BaseURL: strings.TrimRight(baseURL, "/")}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Scoped packages arrive as "@scope%2fname", so the raw escaped path is
	// what has to be parsed: the decoded path would have lost the distinction
	// between a slash in the name and a path separator.
	raw := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	raw = strings.TrimPrefix(raw, "npm/")

	if strings.HasPrefix(raw, "dist/") {
		h.serveTarball(w, r, strings.TrimPrefix(raw, "dist/"))
		return
	}
	name := unescapeName(raw)
	if name == "" {
		http.NotFound(w, r)
		return
	}
	h.servePackument(w, name, baseURL(h.BaseURL, r))
}

type packument struct {
	Name     string                `json:"name"`
	DistTags map[string]string     `json:"dist-tags"`
	Versions map[string]npmVersion `json:"versions"`
	Time     map[string]time.Time  `json:"time,omitempty"`
}

type npmVersion struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Type         string            `json:"type,omitempty"`
	License      string            `json:"license,omitempty"`
	Dependencies map[string]string `json:"dependencies,omitempty"`
	Dist         npmDist           `json:"dist"`
}

type npmDist struct {
	Tarball string `json:"tarball"`
	Shasum  string `json:"shasum"`
}

// baseURL falls back to the request host when no external URL is configured.
func baseURL(configured string, r *http.Request) string {
	if configured != "" {
		return configured
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	}
	return scheme + "://" + r.Host
}

func (h *Handler) servePackument(w http.ResponseWriter, name, base string) {
	mods, err := h.Store.ListModules()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	doc := packument{
		Name:     name,
		DistTags: map[string]string{},
		Versions: map[string]npmVersion{},
		Time:     map[string]time.Time{},
	}
	var latest string
	for _, m := range mods {
		idx, err := h.Store.Index(m)
		if err != nil {
			continue
		}
		for _, v := range idx.Versions {
			a, ok := v.Artifact(registry.TargetNPM)
			if !ok || a.Name != name {
				continue
			}
			nv := npmVersion{
				Name:    a.Name,
				Version: a.Version,
				Dist: npmDist{
					Tarball: fmt.Sprintf("%s/npm/dist/%s/%s/%s.tgz", base, m.Owner, m.Name, v.Version),
					Shasum:  a.SHA1,
				},
			}
			if deps, ok := a.Meta["dependencies"].(map[string]any); ok {
				nv.Dependencies = toStringMap(deps)
			}
			if s, ok := a.Meta["type"].(string); ok {
				nv.Type = s
			}
			if s, ok := a.Meta["license"].(string); ok {
				nv.License = s
			}
			doc.Versions[a.Version] = nv
			doc.Time[a.Version] = v.CreatedAt
			latest = a.Version
		}
	}
	if len(doc.Versions) == 0 {
		http.Error(w, fmt.Sprintf("npm package %q not found", name), http.StatusNotFound)
		return
	}
	doc.DistTags["latest"] = latest
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(doc)
}

// serveTarball handles "<owner>/<name>/<version>.tgz".
func (h *Handler) serveTarball(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || !strings.HasSuffix(parts[2], ".tgz") {
		http.NotFound(w, r)
		return
	}
	mod := registry.Module{Owner: parts[0], Name: parts[1]}
	version := strings.TrimSuffix(parts[2], ".tgz")

	idx, err := h.Store.Index(mod)
	if err != nil {
		writeErr(w, err)
		return
	}
	v, ok := idx.Find(version)
	if !ok {
		http.NotFound(w, r)
		return
	}
	a, ok := v.Artifact(registry.TargetNPM)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, size, err := h.Store.Open(mod, version, a.File)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", fmt.Sprint(size))
	http.ServeContent(w, r, a.File, v.CreatedAt, f)
}

// unescapeName turns "@scope%2fname" or "@scope/name" into "@scope/name".
func unescapeName(raw string) string {
	raw = strings.TrimSuffix(raw, "/")
	raw = strings.ReplaceAll(raw, "%2f", "/")
	raw = strings.ReplaceAll(raw, "%2F", "/")
	if raw == "" || strings.Count(raw, "/") > 1 {
		return ""
	}
	return raw
}

func toStringMap(in map[string]any) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func writeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, registry.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
