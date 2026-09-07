package gomodproxy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smoreg/protobuffet/internal/gomodproxy"
	"github.com/smoreg/protobuffet/internal/pack"
	"github.com/smoreg/protobuffet/internal/registry"
)

// The go command escapes uppercase letters in module paths as !x, so a module
// under a capitalised host or org must still resolve.
const mixedCasePath = "registry.test/gen/go/Acme/Catalog"

func serve(t *testing.T) (*httptest.Server, *registry.Catalog) {
	t.Helper()
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := registry.NewCatalog(store)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.StripPrefix("/go/", gomodproxy.New(catalog)))
	t.Cleanup(ts.Close)
	return ts, catalog
}

func publishModule(t *testing.T, c *registry.Catalog, modPath, version string) {
	t.Helper()
	dir := t.TempDir()
	if err := writeFiles(dir, map[string]string{
		"go.mod":            "module " + modPath + "\n\ngo 1.21\n",
		"catalog/v1/api.go": "package catalogv1\n",
	}); err != nil {
		t.Fatal(err)
	}
	body, err := pack.GoModuleZip(dir, modPath, version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Publish(registry.PublishRequest{
		Module:    registry.Module{Owner: "acme", Name: "catalog"},
		Version:   version,
		SchemaZip: []byte("schema " + version),
		Artifacts: []registry.Artifact{{
			Target: registry.TargetGo, Name: modPath, Version: version, File: "go.zip",
		}},
		Files: map[string][]byte{"go.zip": body},
		Now:   time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
}

func writeFiles(dir string, files map[string]string) error {
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func TestProxyEndpoints(t *testing.T) {
	ts, c := serve(t)
	publishModule(t, c, mixedCasePath, "v1.0.0")
	publishModule(t, c, mixedCasePath, "v1.1.0")

	escaped := "registry.test/gen/go/!acme/!catalog"

	t.Run("list is sorted and excludes pseudo-versions", func(t *testing.T) {
		body := fetch(t, ts.URL+"/go/"+escaped+"/@v/list", http.StatusOK)
		if got := strings.Fields(body); len(got) != 2 || got[0] != "v1.0.0" || got[1] != "v1.1.0" {
			t.Errorf("list = %q", body)
		}
		publishModule(t, c, mixedCasePath, "v0.0.0-20260907120000-abcdef012345")
		body = fetch(t, ts.URL+"/go/"+escaped+"/@v/list", http.StatusOK)
		if strings.Contains(body, "v0.0.0-") {
			t.Errorf("pseudo-version listed as a release: %q", body)
		}
	})

	t.Run("latest and info", func(t *testing.T) {
		var info struct {
			Version string
			Time    time.Time
		}
		if err := json.Unmarshal([]byte(fetch(t, ts.URL+"/go/"+escaped+"/@latest", http.StatusOK)), &info); err != nil {
			t.Fatal(err)
		}
		if info.Version == "" {
			t.Error("@latest has no version")
		}
		if err := json.Unmarshal([]byte(fetch(t, ts.URL+"/go/"+escaped+"/@v/v1.0.0.info", http.StatusOK)), &info); err != nil {
			t.Fatal(err)
		}
		if info.Version != "v1.0.0" || info.Time.IsZero() {
			t.Errorf("info = %+v", info)
		}
	})

	t.Run("mod comes from inside the zip", func(t *testing.T) {
		body := fetch(t, ts.URL+"/go/"+escaped+"/@v/v1.0.0.mod", http.StatusOK)
		if !strings.Contains(body, "module "+mixedCasePath) {
			t.Errorf("go.mod = %q", body)
		}
	})

	t.Run("unknown module and version are 404", func(t *testing.T) {
		fetch(t, ts.URL+"/go/registry.test/gen/go/nope/@v/list", http.StatusNotFound)
		fetch(t, ts.URL+"/go/"+escaped+"/@v/v9.9.9.info", http.StatusNotFound)
		fetch(t, ts.URL+"/go/"+escaped+"/@v/v1.0.0.xyz", http.StatusNotFound)
		fetch(t, ts.URL+"/go/nonsense", http.StatusNotFound)
	})

	t.Run("unescaped uppercase is rejected", func(t *testing.T) {
		// The go command never sends this; accepting it would mean two URLs
		// for one module.
		fetch(t, ts.URL+"/go/"+mixedCasePath+"/@v/list", http.StatusBadRequest)
	})

	t.Run("only GET and HEAD", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/go/"+escaped+"/@v/list", "text/plain", strings.NewReader(""))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST status = %d", resp.StatusCode)
		}
	})
}

func fetch(t *testing.T, url string, wantStatus int) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s: status %d, want %d: %s", url, resp.StatusCode, wantStatus, body)
	}
	return string(body)
}
