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
	"sort"
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
		h.serveRoot(w, baseURL(h.BaseURL, r))
	case strings.HasPrefix(p, "p2/"):
		h.servePackage(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "p2/"), ".json"), baseURL(h.BaseURL, r))
	case strings.HasPrefix(p, "dist/"):
		h.serveDist(w, r, strings.TrimPrefix(p, "dist/"))
	default:
		http.NotFound(w, r)
	}
}

// rootDoc is what Composer 2 fetches first. Serving every version inline
// would mean re-downloading the whole registry on each update; metadata-url
// lets the client ask for one package at a time.
type rootDoc struct {
	Packages          map[string]any `json:"packages"`
	MetadataURL       string         `json:"metadata-url"`
	AvailablePackages []string       `json:"available-packages"`
	ProvidersAPI      string         `json:"providers-api,omitempty"`
}

// packageDoc is one package's metadata, in the expanded form Composer accepts.
type packageDoc struct {
	Packages map[string][]packageVersion `json:"packages"`
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

func (h *Handler) serveRoot(w http.ResponseWriter, base string) {
	names := map[string]bool{}
	h.eachVersion(func(_ registry.Module, _ registry.Version, a registry.Artifact) {
		names[a.Name] = true
	})
	doc := rootDoc{
		Packages:          map[string]any{},
		MetadataURL:       base + "/composer/p2/%package%.json",
		AvailablePackages: make([]string, 0, len(names)),
	}
	for name := range names {
		doc.AvailablePackages = append(doc.AvailablePackages, name)
	}
	sort.Strings(doc.AvailablePackages)
	writeJSON(w, doc)
}

// servePackage renders one package: "vendor/name" or "vendor/name~dev".
func (h *Handler) servePackage(w http.ResponseWriter, r *http.Request, name, base string) {
	// Composer asks for a ~dev variant as well; we have no dev branches, so
	// answer with an empty document rather than a 404, which it logs.
	devVariant := strings.HasSuffix(name, "~dev")
	name = strings.TrimSuffix(name, "~dev")

	doc := packageDoc{Packages: map[string][]packageVersion{}}
	if devVariant {
		doc.Packages[name] = []packageVersion{}
		writeJSON(w, doc)
		return
	}

	var versions []packageVersion
	h.eachVersion(func(m registry.Module, v registry.Version, a registry.Artifact) {
		if a.Name != name {
			return
		}
		versions = append(versions, composerVersion(m, v, a, base))
	})
	if len(versions) == 0 {
		http.NotFound(w, r)
		return
	}
	doc.Packages[name] = versions
	writeJSON(w, doc)
}

// eachVersion visits every published Composer artifact that is not yanked.
func (h *Handler) eachVersion(fn func(registry.Module, registry.Version, registry.Artifact)) {
	mods, err := h.Catalog.ListModules()
	if err != nil {
		return
	}
	for _, m := range mods {
		idx, err := h.Catalog.Index(m)
		if err != nil {
			continue
		}
		for _, v := range idx.Versions {
			a, ok := v.Artifact(registry.TargetComposer)
			// Composer has no yank concept, so a yanked version simply
			// disappears from the metadata; the dist stays downloadable for
			// lock files that already reference it.
			if !ok || v.Yanked {
				continue
			}
			fn(m, v, a)
		}
	}
}

func composerVersion(m registry.Module, v registry.Version, a registry.Artifact, base string) packageVersion {
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
	return pv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v) //nolint:errcheck // a failed write means the client is gone
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
