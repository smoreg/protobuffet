package api

import (
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/smoreg/protobuffet/internal/registry"
)

// The UI is deliberately one file with no assets: a registry that needs a
// build step to render a list of modules is a registry nobody self-hosts.
var uiTmpl = template.Must(template.New("ui").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root{--bg:#f6f7f8;--fg:#16202a;--dim:#5d6b76;--line:#dde3e8;--card:#fff;--accent:#0a6270}
@media(prefers-color-scheme:dark){:root{--bg:#0d1418;--fg:#e3ebef;--dim:#8ea0aa;--line:#22333b;--card:#131e24;--accent:#63c0d4}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.6 ui-sans-serif,-apple-system,"Segoe UI",Helvetica,Arial,sans-serif}
.wrap{max-width:900px;margin:0 auto;padding:32px 20px 80px}
h1{font-size:24px;margin:0 0 4px;letter-spacing:-.01em}
h2{font-size:17px;margin:32px 0 10px}
.sub{color:var(--dim);margin:0 0 28px}
a{color:var(--accent)}
table{width:100%;border-collapse:collapse;background:var(--card);border:1px solid var(--line);border-radius:6px;overflow:hidden}
th,td{text-align:left;padding:10px 14px;border-bottom:1px solid var(--line);font-size:14px}
th{font-size:11px;letter-spacing:.08em;text-transform:uppercase;color:var(--dim);font-weight:500}
tr:last-child td{border-bottom:none}
code,pre{font-family:ui-monospace,"SF Mono",Menlo,monospace;font-size:13px}
pre{background:var(--card);border:1px solid var(--line);border-radius:6px;padding:12px 14px;overflow-x:auto}
.tag{display:inline-block;padding:1px 7px;border:1px solid var(--line);border-radius:99px;font-size:12px;color:var(--dim)}
.empty{color:var(--dim);padding:24px 0}
</style></head><body><div class="wrap">
<h1>{{.Title}}</h1><p class="sub">{{.Subtitle}}</p>
{{if .Modules}}
<table><thead><tr><th>Module</th><th>Latest</th><th>Versions</th><th>Updated</th></tr></thead><tbody>
{{range .Modules}}<tr>
<td><a href="/m/{{.Module}}">{{.Module}}</a></td>
<td><code>{{.Latest}}</code></td><td>{{.Versions}}</td><td>{{.Updated}}</td>
</tr>{{end}}
</tbody></table>
{{end}}
{{if .Versions}}
<table><thead><tr><th>Version</th><th>Published</th><th>Targets</th><th>Schema</th></tr></thead><tbody>
{{range .Versions}}<tr>
<td><code>{{.Version}}</code> {{range .Labels}}<span class="tag">{{.}}</span> {{end}}</td>
<td>{{.Published}}</td><td>{{range .Targets}}<span class="tag">{{.}}</span> {{end}}</td>
<td><a href="/api/v1/modules/{{$.ModuleName}}/versions/{{.Version}}/schema.zip">schema.zip</a></td>
</tr>{{end}}
</tbody></table>
{{range .Install}}<h2>{{.Title}}</h2><pre>{{.Body}}</pre>{{end}}
{{end}}
{{if .Empty}}<p class="empty">Nothing published yet. Push a module with <code>buffet publish</code>.</p>{{end}}
</div></body></html>`))

type uiModule struct {
	Module   string
	Latest   string
	Versions int
	Updated  string
}

type uiVersion struct {
	Version   string
	Published string
	Targets   []string
	Labels    []string
}

type uiInstall struct {
	Title string
	Body  string
}

type uiData struct {
	Title      string
	Subtitle   string
	ModuleName string
	Modules    []uiModule
	Versions   []uiVersion
	Install    []uiInstall
	Empty      bool
}

// baseURL is the externally visible root: the configured one, or the one the
// request came in on.
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return s.cfg.BaseURL
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

func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	mods, err := s.store.ListModules()
	if err != nil {
		httpError(w, err)
		return
	}
	data := uiData{
		Title:    "protobuffet",
		Subtitle: "Protobuf schema registry serving Go, Composer and npm packages from " + s.baseURL(r),
		Empty:    len(mods) == 0,
	}
	for _, m := range mods {
		idx, err := s.store.Index(m)
		if err != nil {
			continue
		}
		um := uiModule{Module: m.String(), Versions: len(idx.Versions), Updated: idx.Updated.Format(time.DateOnly)}
		if v, ok := idx.Latest(); ok {
			um.Latest = v.Version
		}
		data.Modules = append(data.Modules, um)
	}
	renderUI(w, data)
}

func (s *Server) uiModule(w http.ResponseWriter, r *http.Request) {
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
	data := uiData{
		Title:      m.String(),
		Subtitle:   "Schema module with " + plural(len(idx.Versions), "version") + ".",
		ModuleName: m.String(),
	}
	labels := map[string][]string{}
	for name, version := range idx.Labels {
		labels[version] = append(labels[version], name)
	}
	for i := len(idx.Versions) - 1; i >= 0; i-- {
		v := idx.Versions[i]
		uv := uiVersion{
			Version:   v.Version,
			Published: v.CreatedAt.Format(time.RFC3339),
			Labels:    labels[v.Version],
		}
		for _, t := range registry.AllTargets {
			if _, ok := v.Artifact(t); ok {
				uv.Targets = append(uv.Targets, string(t))
			}
		}
		data.Versions = append(data.Versions, uv)
	}
	if latest, ok := idx.Latest(); ok {
		data.Install = installSnippets(latest, s.baseURL(r))
	}
	renderUI(w, data)
}

// installSnippets renders the copy-paste commands for each published target.
func installSnippets(v registry.Version, base string) []uiInstall {
	var out []uiInstall
	if a, ok := v.Artifact(registry.TargetGo); ok {
		out = append(out, uiInstall{
			Title: "Go",
			Body: "export GOPROXY=" + base + "/go,https://proxy.golang.org,direct\n" +
				"export GOSUMDB=off\n" +
				"go get " + a.Name + "@" + a.Version,
		})
	}
	if a, ok := v.Artifact(registry.TargetComposer); ok {
		out = append(out, uiInstall{
			Title: "PHP (Composer)",
			Body: "composer config repositories.protobuffet composer " + base + "/composer\n" +
				"composer require " + a.Name + ":" + a.Version,
		})
	}
	if a, ok := v.Artifact(registry.TargetNPM); ok {
		out = append(out, uiInstall{
			Title: "JavaScript (npm)",
			Body:  "npm install " + a.Name + "@" + a.Version + " --registry " + base + "/npm",
		})
	}
	return out
}

func renderUI(w http.ResponseWriter, data uiData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	uiTmpl.Execute(w, data)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
