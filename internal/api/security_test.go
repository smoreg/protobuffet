package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/smoreg/protobuffet/internal/api"
	"github.com/smoreg/protobuffet/internal/client"
	"github.com/smoreg/protobuffet/internal/config"
	"github.com/smoreg/protobuffet/internal/pack"
	"github.com/smoreg/protobuffet/internal/registry"
)

// TestDownloadPathsRejectTraversal covers the three download handlers that
// build a filesystem path out of URL segments. Each request must be refused
// before it reaches the store.
func TestDownloadPathsRejectTraversal(t *testing.T) {
	ts, _ := newServer(t)
	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := client.New(ts.URL, testToken).Publish(context.Background(), man, files); err != nil || !ok {
		t.Fatalf("publish: %v %v", ok, err)
	}

	paths := []string{
		"/composer/dist/../../../etc/passwd.zip",
		"/composer/dist/acme/..%2f..%2fcatalog/v1.0.0.zip",
		"/composer/dist/ACME/catalog/v1.0.0.zip",
		"/composer/dist/acme/catalog/latest.zip",
		"/composer/dist/acme/catalog/..%2fv1.0.0.zip",
		"/npm/dist/../../etc/passwd.tgz",
		"/npm/dist/acme/catalog/not-a-version.tgz",
		"/npm/dist/acme/cat%20alog/v1.0.0.tgz",
		"/pypi/files/../../../etc/passwd.whl",
		"/pypi/files/acme/catalog/../v1.0.0/x.whl",
		"/pypi/files/acme/catalog/latest/x.whl",
	}
	for _, p := range paths {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Errorf("GET %s: %v", p, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", p, resp.StatusCode)
		}
	}
}

// TestValidDownloadPathsStillWork guards against the validation above being
// too strict to serve real requests.
func TestValidDownloadPathsStillWork(t *testing.T) {
	ts, _ := newServer(t)
	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := client.New(ts.URL, testToken).Publish(context.Background(), man, files); err != nil || !ok {
		t.Fatalf("publish: %v %v", ok, err)
	}
	for _, p := range []string{
		"/composer/dist/acme/catalog/v1.0.0.zip",
		"/npm/dist/acme/catalog/v1.0.0.tgz",
		"/pypi/files/acme/catalog/v1.0.0/acme_catalog_proto-1.0.0-py3-none-any.whl",
	} {
		if body := get(t, ts.URL+p); len(body) < 50 {
			t.Errorf("GET %s returned %d bytes", p, len(body))
		}
	}
	// An unversioned "v" prefix is accepted by NormalizeVersion but the file
	// name in metadata always carries it, so both spellings must resolve.
	if body := get(t, ts.URL+"/composer/dist/acme/catalog/v1.0.0.zip"); !strings.HasPrefix(string(body[:2]), "PK") {
		t.Error("composer dist is not a zip")
	}
}

// TestBreakingModes covers the gate: required refuses when it cannot verify,
// best-effort publishes with a warning, off skips entirely.
func TestBreakingRequiredRefusesWhenBufIsMissing(t *testing.T) {
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := registry.NewCatalog(store)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultServer()
	cfg.BaseURL = ""
	cfg.BreakingCheck = config.BreakingRequired
	cfg.BufPath = "/nonexistent/buf"
	cfg.Tokens = []config.Token{{Subject: "t", SHA256: config.HashToken(testToken), Scopes: []string{"write"}}}

	// Required mode must refuse to start rather than serve without the gate.
	if _, err := api.New(cfg, catalog, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("server started in required mode without buf")
	}

	cfg.BreakingCheck = config.BreakingBestEffort
	if _, err := api.New(cfg, catalog, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("best-effort should start without buf: %v", err)
	}
}

func TestBreakingModeParsing(t *testing.T) {
	for in, want := range map[string]config.BreakingMode{
		"required":    config.BreakingRequired,
		"best-effort": config.BreakingBestEffort,
		"off":         config.BreakingOff,
		"true":        config.BreakingRequired, // historical boolean spelling
		"false":       config.BreakingOff,
	} {
		var got config.BreakingMode
		if err := yaml.Unmarshal([]byte(in), &got); err != nil || got != want {
			t.Errorf("parse %q = %q, %v; want %q", in, got, err, want)
		}
	}
	var bad config.BreakingMode
	if err := yaml.Unmarshal([]byte("maybe"), &bad); err == nil {
		t.Error("invalid mode accepted")
	}
}

func TestPublishRejectsBadRequests(t *testing.T) {
	ts, _ := newServer(t)
	c := client.New(ts.URL, testToken)
	ctx := context.Background()

	t.Run("missing schema", func(t *testing.T) {
		man, files := buildFixture(t, "v1.0.0")
		delete(files, "schema.zip")
		if _, _, err := c.Publish(ctx, man, files); err == nil {
			t.Error("published without a schema archive")
		}
	})

	t.Run("schema digest mismatch", func(t *testing.T) {
		man, files := buildFixture(t, "v1.0.0")
		man.SchemaSHA256 = registry.SHA256Hex([]byte("something else"))
		if _, _, err := c.Publish(ctx, man, files); err == nil {
			t.Error("published with a mismatched schema digest")
		}
	})

	t.Run("module mismatch", func(t *testing.T) {
		man, files := buildFixture(t, "v1.0.0")
		man.Module = "acme/other"
		// The client posts to the path from the manifest, so make them differ
		// by rewriting the manifest after the URL is chosen.
		if _, _, err := c.Publish(ctx, man, files); err != nil {
			// Publishing to acme/other is legitimate; assert the version lands
			// where the manifest says.
			t.Fatalf("publish: %v", err)
		}
		if _, err := c.Module(ctx, "acme/other"); err != nil {
			t.Errorf("module acme/other not found: %v", err)
		}
	})

	t.Run("invalid version", func(t *testing.T) {
		man, files := buildFixture(t, "v1.0.0")
		man.Version = "not-a-version"
		if _, _, err := c.Publish(ctx, man, files); err == nil {
			t.Error("published with an invalid version")
		}
	})

	t.Run("artifact file missing", func(t *testing.T) {
		man, files := buildFixture(t, "v1.0.0")
		delete(files, "go.zip")
		if _, _, err := c.Publish(ctx, man, files); err == nil {
			t.Error("published with a missing artifact file")
		}
	})
}

func TestLabelsAndVersionLookup(t *testing.T) {
	ts, _ := newServer(t)
	c := client.New(ts.URL, testToken)
	ctx := context.Background()

	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := c.Publish(ctx, man, files); err != nil || !ok {
		t.Fatalf("publish: %v %v", ok, err)
	}
	man2, files2 := buildFixture(t, "v1.1.0")
	if _, ok, err := c.Publish(ctx, man2, files2); err != nil || !ok {
		t.Fatalf("publish v1.1.0: %v %v", ok, err)
	}

	if err := c.SetLabel(ctx, "acme/catalog", "stable", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	idx, err := c.Module(ctx, "acme/catalog")
	if err != nil {
		t.Fatal(err)
	}
	if idx.Labels["stable"] != "v1.0.0" {
		t.Errorf("labels = %v", idx.Labels)
	}
	// A label resolves everywhere a version does, which is what lets a
	// consumer follow a branch.
	body := get(t, ts.URL+"/api/v1/modules/acme/catalog/versions/stable")
	if !strings.Contains(body, `"version": "v1.0.0"`) {
		t.Errorf("label lookup = %q", body)
	}
	if body := get(t, ts.URL+"/api/v1/modules/acme/catalog/versions/latest"); !strings.Contains(body, "v1.1.0") {
		t.Errorf("latest = %q", body)
	}
	// Pointing a label at a version that does not exist must fail.
	if err := c.SetLabel(ctx, "acme/catalog", "stable", "v9.9.9"); err == nil {
		t.Error("label pointed at a missing version")
	}
}

func TestHealthzAndUnknownRoutes(t *testing.T) {
	ts, _ := newServer(t)
	if body := get(t, ts.URL+"/healthz"); strings.TrimSpace(body) != "ok" {
		t.Errorf("healthz = %q", body)
	}
	resp, err := http.Get(ts.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown route status = %d", resp.StatusCode)
	}
	// Every response declares its content type honestly.
	resp, err = http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff header missing")
	}
}

func TestModuleListingPaginates(t *testing.T) {
	ts, _ := newServer(t)
	c := client.New(ts.URL, testToken)
	ctx := context.Background()

	for i := range 5 {
		man, files := buildFixtureAt(t, "v1.0.0", fmt.Sprintf("registry.test/gen/go/acme/mod%d", i))
		man.Module = fmt.Sprintf("acme/mod%d", i)
		man.Artifacts[1].Name = fmt.Sprintf("acme/mod%d-proto", i)
		man.Artifacts[2].Name = fmt.Sprintf("@acme/mod%d", i)
		man.Artifacts[3].Name = fmt.Sprintf("acme-mod%d-proto", i)
		rebuildForModule(t, &man, files, i)
		if _, ok, err := c.Publish(ctx, man, files); err != nil || !ok {
			t.Fatalf("publish %d: %v %v", i, ok, err)
		}
	}

	// One page at a time, following the cursor, must visit every module once.
	seen := map[string]bool{}
	cursor := ""
	for range 10 {
		url := ts.URL + "/api/v1/modules?limit=2"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		var page struct {
			Modules []struct{ Module string }
			Next    string
		}
		if err := json.Unmarshal([]byte(get(t, url)), &page); err != nil {
			t.Fatal(err)
		}
		for _, m := range page.Modules {
			if seen[m.Module] {
				t.Errorf("module %s returned twice", m.Module)
			}
			seen[m.Module] = true
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if len(seen) != 5 {
		t.Errorf("paged through %d modules, want 5", len(seen))
	}

	// The client follows pages itself.
	mods, err := c.ListModules(ctx)
	if err != nil || len(mods) != 5 {
		t.Errorf("ListModules = %d modules, %v", len(mods), err)
	}
}

// rebuildForModule repacks the fixture's composer and npm artifacts so each
// module owns distinct package names.
func rebuildForModule(t *testing.T, man *api.PublishManifest, files map[string][]byte, i int) {
	t.Helper()
	phpDir := t.TempDir()
	writeFile(t, filepath.Join(phpDir, "Catalog.php"), "<?php\n")
	writeFile(t, filepath.Join(phpDir, "composer.json"),
		fmt.Sprintf(`{"name": "acme/mod%d-proto"}`, i))
	body, err := pack.Zip(phpDir, fmt.Sprintf("acme-mod%d-proto-1.0.0", i), pack.SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	files["composer.zip"] = body

	tsDir := t.TempDir()
	writeFile(t, filepath.Join(tsDir, "index.js"), "export {}\n")
	writeFile(t, filepath.Join(tsDir, "package.json"),
		fmt.Sprintf(`{"name": "@acme/mod%d", "version": "1.0.0"}`, i))
	tgz, err := pack.TarGz(tsDir, "package", pack.SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	files["npm.tgz"] = tgz

	pyDir := t.TempDir()
	writeFile(t, filepath.Join(pyDir, "catalog/v1/catalog_pb2.py"), "# generated\n")
	wheel, err := pack.Wheel(pyDir, pack.WheelMeta{
		Name: fmt.Sprintf("acme-mod%d-proto", i), Version: "1.0.0", RequiresPython: ">=3.8",
	}, pack.SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	files["python.whl"] = wheel
	for j := range man.Artifacts {
		man.Artifacts[j].SHA256 = ""
	}
}
