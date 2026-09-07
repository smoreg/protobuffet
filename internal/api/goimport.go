package api

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/smoreg/protobuffet/internal/registry"
)

// goImportTmpl is the discovery document the go command looks for. The "mod"
// vcs type means "fetch this module from the proxy at this URL", which is what
// lets a consumer use the registry with GOPRIVATE alone, without setting
// GOPROXY on every machine and in every CI image.
//
// https://go.dev/ref/mod#vcs-find
var goImportTmpl = template.Must(template.New("goimport").Parse(
	`<!DOCTYPE html><html><head>
<meta name="go-import" content="{{.ModulePath}} mod {{.ProxyURL}}">
</head><body>
Module <a href="/m/{{.Module}}">{{.ModulePath}}</a> is served by protobuffet.
</body></html>
`))

type goImportData struct {
	ModulePath string
	ProxyURL   string
	Module     string
}

// serveGoImport answers a `go get` discovery request. It returns false when the
// request is not one, so the caller can carry on.
func (s *Server) serveGoImport(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Query().Get("go-get") != "1" {
		return false
	}
	base := s.baseURL(r)
	host := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	// The go command asks about the package path, which may sit below the
	// module root, so walk up until a published module matches.
	full := host + strings.TrimSuffix(r.URL.Path, "/")
	for candidate := full; strings.Contains(candidate, "/"); candidate = candidate[:strings.LastIndex(candidate, "/")] {
		mod, _, err := s.catalog.FindByTarget(registry.TargetGo, candidate)
		if err != nil {
			continue
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := goImportTmpl.Execute(w, goImportData{
			ModulePath: candidate,
			ProxyURL:   base + "/go",
			Module:     mod.String(),
		}); err != nil {
			s.log.Error("render go-import", "err", err)
		}
		return true
	}
	http.Error(w, fmt.Sprintf("no Go module published under %q", full), http.StatusNotFound)
	return true
}
