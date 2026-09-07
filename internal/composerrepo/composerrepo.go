// Package composerrepo exposes published PHP SDKs as a Composer repository.
//
// A Composer repository is just packages.json plus downloadable dists, so a
// consumer only needs one repositories entry:
//
//	{"type": "composer", "url": "https://registry.example.com/composer"}
package composerrepo

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/smoreg/protobuffet/internal/registry"
)

// Handler serves packages.json and the dist archives.
type Handler struct {
	Catalog *registry.Catalog
	BaseURL string
}

// New builds a handler. baseURL must be reachable by the Composer client,
// because dist URLs are absolute.
func New(catalog *registry.Catalog, baseURL string) *Handler {
	return &Handler{Catalog: catalog, BaseURL: strings.TrimRight(baseURL, "/")}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := strings.Trim(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case p == "" || p == "packages.json":
		h.servePackages(w, baseURL(h.BaseURL, r))
	case strings.HasPrefix(p, "dist/"):
		h.serveDist(w, r, strings.TrimPrefix(p, "dist/"))
	default:
		http.NotFound(w, r)
	}
}

type packagesDoc struct {
	Packages map[string]map[string]packageVersion `json:"packages"`
}

type packageVersion struct {
	Name     string            `json:"name"`
	Version  string            `json:"version"`
	Type     string            `json:"type"`
	Time     time.Time         `json:"time"`
	Require  map[string]string `json:"require,omitempty"`
	Autoload map[string]any    `json:"autoload,omitempty"`
	License  []string          `json:"license,omitempty"`
	Dist     dist              `json:"dist"`
}

type dist struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	Reference string `json:"reference"`
	Shasum    string `json:"shasum"`
}

// baseURL falls back to the request host when no external URL is configured,
// which is what makes a zero-config deployment behind any hostname work.
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

func (h *Handler) servePackages(w http.ResponseWriter, base string) {
	mods, err := h.Catalog.ListModules()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	doc := packagesDoc{Packages: map[string]map[string]packageVersion{}}
	for _, m := range mods {
		idx, err := h.Catalog.Index(m)
		if err != nil {
			continue
		}
		for _, v := range idx.Versions {
			a, ok := v.Artifact(registry.TargetComposer)
			if !ok {
				continue
			}
			pv := packageVersion{
				Name:    a.Name,
				Version: a.Version,
				Type:    "library",
				Time:    v.CreatedAt,
				Dist: dist{
					Type:      "zip",
					URL:       fmt.Sprintf("%s/composer/dist/%s/%s/%s.zip", base, m.Owner, m.Name, v.Version),
					Reference: a.SHA256,
					Shasum:    a.SHA1,
				},
			}
			if req, ok := a.Meta["require"].(map[string]any); ok {
				pv.Require = toStringMap(req)
			}
			if al, ok := a.Meta["autoload"].(map[string]any); ok {
				pv.Autoload = al
			}
			if lic, ok := a.Meta["license"].(string); ok && lic != "" {
				pv.License = []string{lic}
			}
			if _, ok := doc.Packages[a.Name]; !ok {
				doc.Packages[a.Name] = map[string]packageVersion{}
			}
			doc.Packages[a.Name][a.Version] = pv
		}
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(doc)
}

// serveDist handles "<owner>/<name>/<version>.zip".
func (h *Handler) serveDist(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || !strings.HasSuffix(parts[2], ".zip") {
		http.NotFound(w, r)
		return
	}
	// Validate before the segments reach the store: they are attacker
	// controlled and end up in a filesystem path. A 404 rather than a 400
	// keeps the path structure out of the answer.
	mod, err := registry.ParseModuleParts(parts[0], parts[1])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	version, err := registry.NormalizeVersion(strings.TrimSuffix(parts[2], ".zip"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	idx, err := h.Catalog.Index(mod)
	if err != nil {
		writeErr(w, err)
		return
	}
	v, ok := idx.Find(version)
	if !ok {
		http.NotFound(w, r)
		return
	}
	a, ok := v.Artifact(registry.TargetComposer)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, size, err := h.Catalog.Open(mod, version, a.File)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", fmt.Sprint(size))
	http.ServeContent(w, r, a.File, v.CreatedAt, f)
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
