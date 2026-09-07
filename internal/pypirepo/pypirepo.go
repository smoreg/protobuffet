// Package pypirepo serves published Python SDKs as a PEP 503 simple index, so
// pip installs them with nothing but --index-url.
package pypirepo

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"

	"github.com/smoreg/protobuffet/internal/pack"
	"github.com/smoreg/protobuffet/internal/registry"
)

// Handler serves the simple index and the wheel files.
type Handler struct {
	Catalog *registry.Catalog
	BaseURL string
}

// New builds a handler.
func New(catalog *registry.Catalog, baseURL string) *Handler {
	return &Handler{Catalog: catalog, BaseURL: strings.TrimRight(baseURL, "/")}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := strings.Trim(strings.TrimPrefix(r.URL.Path, "/"), "/")
	p = strings.TrimPrefix(p, "pypi/")

	switch {
	case p == "simple":
		h.serveRoot(w, baseURL(h.BaseURL, r))
	case strings.HasPrefix(p, "simple/"):
		h.serveProject(w, strings.Trim(strings.TrimPrefix(p, "simple/"), "/"), baseURL(h.BaseURL, r))
	case strings.HasPrefix(p, "files/"):
		h.serveFile(w, r, strings.TrimPrefix(p, "files/"))
	default:
		http.NotFound(w, r)
	}
}

// serveRoot lists every published project.
func (h *Handler) serveRoot(w http.ResponseWriter, base string) {
	names := map[string]bool{}
	h.eachArtifact(func(_ registry.Module, _ registry.Version, a registry.Artifact) {
		names[pack.NormalizePythonName(a.Name)] = true
	})
	var sb strings.Builder
	sb.WriteString("<!DOCTYPE html><html><body>\n")
	for name := range names {
		fmt.Fprintf(&sb, `<a href="%s/pypi/simple/%s/">%s</a><br>`+"\n", base, name, html.EscapeString(name))
	}
	sb.WriteString("</body></html>\n")
	writeHTML(w, sb.String())
}

// serveProject lists the files of one project, newest last, each with the
// digest pip verifies after download.
func (h *Handler) serveProject(w http.ResponseWriter, name, base string) {
	want := pack.NormalizePythonName(name)
	var links []string
	h.eachArtifact(func(m registry.Module, v registry.Version, a registry.Artifact) {
		if pack.NormalizePythonName(a.Name) != want {
			return
		}
		file := pack.WheelFilename(a.Name, a.Version)
		links = append(links, fmt.Sprintf(
			`<a href="%s/pypi/files/%s/%s/%s/%s#sha256=%s" data-requires-python="%s">%s</a><br>`,
			base, m.Owner, m.Name, v.Version, file, a.SHA256,
			html.EscapeString(requiresPython(a)), html.EscapeString(file),
		))
	})
	if len(links) == 0 {
		http.Error(w, fmt.Sprintf("python package %q not found", name), http.StatusNotFound)
		return
	}
	writeHTML(w, "<!DOCTYPE html><html><body>\n"+strings.Join(links, "\n")+"\n</body></html>\n")
}

// serveFile handles "<owner>/<name>/<version>/<file>.whl".
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || !strings.HasSuffix(parts[3], ".whl") {
		http.NotFound(w, r)
		return
	}
	mod, err := registry.ParseModuleParts(parts[0], parts[1])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	version, err := registry.NormalizeVersion(parts[2])
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
	a, ok := v.Artifact(registry.TargetPython)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, size, err := h.Catalog.Open(mod, v.Version, a.File)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(size))
	http.ServeContent(w, r, parts[3], v.CreatedAt, f)
}

func (h *Handler) eachArtifact(fn func(registry.Module, registry.Version, registry.Artifact)) {
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
			if a, ok := v.Artifact(registry.TargetPython); ok {
				fn(m, v, a)
			}
		}
	}
}

func requiresPython(a registry.Artifact) string {
	if s, ok := a.Meta["requires_python"].(string); ok {
		return s
	}
	return ""
}

func writeHTML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(body)) //nolint:errcheck // a failed write means the client is gone
}

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

func writeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, registry.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
