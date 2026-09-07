package pypirepo_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smoreg/protobuffet/internal/pack"
	"github.com/smoreg/protobuffet/internal/pypirepo"
	"github.com/smoreg/protobuffet/internal/registry"
)

func serve(t *testing.T, mods map[string][]string) (*httptest.Server, *registry.Catalog) {
	t.Helper()
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := registry.NewCatalog(store)
	if err != nil {
		t.Fatal(err)
	}
	i := 0
	for mod, versions := range mods {
		m, err := registry.ParseModule(mod)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range versions {
			pkg := strings.ReplaceAll(m.Owner+"-"+m.Name, "/", "-") + "-proto"
			if _, err := c.Publish(registry.PublishRequest{
				Module:    m,
				Version:   v,
				SchemaZip: []byte(mod + v),
				Artifacts: []registry.Artifact{{
					Target: registry.TargetPython, Name: pkg,
					Version: registry.PythonVersion(v), File: "python.whl",
					Meta: map[string]any{"requires_python": ">=3.8"},
				}},
				Files: map[string][]byte{"python.whl": []byte(mod + v)},
				Now:   time.Date(2026, 9, 7, 12, i, 0, 0, time.UTC),
			}); err != nil {
				t.Fatal(err)
			}
			i++
		}
	}
	ts := httptest.NewServer(pypirepo.New(c, ""))
	t.Cleanup(ts.Close)
	return ts, c
}

func fetch(t *testing.T, url string, want int) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("GET %s: status %d, want %d: %s", url, resp.StatusCode, want, body)
	}
	return string(body)
}

func TestSimpleIndexShape(t *testing.T) {
	ts, _ := serve(t, map[string][]string{"acme/catalog": {"v1.0.0", "v1.1.0"}})

	root := fetch(t, ts.URL+"/pypi/simple", http.StatusOK)
	if !strings.Contains(root, "/pypi/simple/acme-catalog-proto/") {
		t.Errorf("root index = %q", root)
	}

	page := fetch(t, ts.URL+"/pypi/simple/acme-catalog-proto/", http.StatusOK)
	for _, want := range []string{
		"acme_catalog_proto-1.0.0-py3-none-any.whl",
		"acme_catalog_proto-1.1.0-py3-none-any.whl",
		// pip verifies the fragment digest after download.
		"#sha256=",
		`data-requires-python="&gt;=3.8"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("project page missing %q:\n%s", want, page)
		}
	}
}

// TestNameNormalizationMatchesPip is the rule that decides whether two
// packages are one project. Ownership is tracked under the same rule, so a
// second module cannot publish onto this page.
func TestNameNormalizationMatchesPip(t *testing.T) {
	ts, _ := serve(t, map[string][]string{"acme/catalog": {"v1.0.0"}})
	for _, spelling := range []string{
		"acme-catalog-proto", "Acme.Catalog.Proto", "acme_catalog_proto", "ACME--CATALOG__PROTO",
	} {
		if page := fetch(t, ts.URL+"/pypi/simple/"+spelling+"/", http.StatusOK); !strings.Contains(page, ".whl") {
			t.Errorf("spelling %q did not resolve", spelling)
		}
	}
	fetch(t, ts.URL+"/pypi/simple/unrelated/", http.StatusNotFound)
}

func TestAnotherModuleCannotPublishOntoTheSamePage(t *testing.T) {
	ts, c := serve(t, map[string][]string{"acme/catalog": {"v1.0.0"}})
	// The whole point: a write token must not be able to shadow someone
	// else's Python package by spelling the name differently.
	evil, _ := registry.ParseModule("evil/module")
	_, err := c.Publish(registry.PublishRequest{
		Module:    evil,
		Version:   "v9.9.9",
		SchemaZip: []byte("evil"),
		Artifacts: []registry.Artifact{{
			Target: registry.TargetPython, Name: "Acme.Catalog.Proto",
			Version: "9.9.9", File: "python.whl",
		}},
		Files: map[string][]byte{"python.whl": []byte("evil")},
	})
	if err == nil {
		t.Fatal("a second module claimed the same normalized name")
	}
	page := fetch(t, ts.URL+"/pypi/simple/acme-catalog-proto/", http.StatusOK)
	if strings.Contains(page, "9.9.9") {
		t.Errorf("the page serves the other module's wheel:\n%s", page)
	}
}

func TestWheelDownload(t *testing.T) {
	ts, _ := serve(t, map[string][]string{"acme/catalog": {"v1.0.0"}})
	page := fetch(t, ts.URL+"/pypi/simple/acme-catalog-proto/", http.StatusOK)

	start := strings.Index(page, `href="`) + len(`href="`)
	href := page[start : start+strings.Index(page[start:], `"`)]
	url, digest, _ := strings.Cut(href, "#sha256=")

	body := fetch(t, url, http.StatusOK)
	if body != "acme/catalogv1.0.0" {
		t.Errorf("wheel body = %q", body)
	}
	if digest != registry.SHA256Hex([]byte(body)) {
		t.Errorf("digest %q does not match the served bytes", digest)
	}
	if got := pack.WheelFilename("acme-catalog-proto", "1.0.0"); !strings.Contains(href, got) {
		t.Errorf("href %q does not carry the wheel name %q", href, got)
	}

	fetch(t, ts.URL+"/pypi/files/acme/catalog/v9.9.9/x.whl", http.StatusNotFound)
	fetch(t, ts.URL+"/pypi/files/acme/catalog/v1.0.0/x.txt", http.StatusNotFound)
	fetch(t, ts.URL+"/pypi/files/../../etc/passwd.whl", http.StatusNotFound)
	fetch(t, ts.URL+"/pypi/nonsense", http.StatusNotFound)
}
