package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smoreg/protobuffet/internal/api"
	"github.com/smoreg/protobuffet/internal/client"
	"github.com/smoreg/protobuffet/internal/config"
	"github.com/smoreg/protobuffet/internal/pack"
	"github.com/smoreg/protobuffet/internal/registry"
)

const (
	testToken = "test-token-value"
	goModPath = "registry.test/gen/go/acme/catalog"
)

func newServer(t *testing.T) (*httptest.Server, config.Server) {
	t.Helper()
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := registry.NewCatalog(store)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultServer()
	cfg.PublicRead = true
	// Empty base URL exercises the request-derived fallback, which is what a
	// deployment behind an unknown hostname relies on.
	cfg.BaseURL = ""
	// The compatibility check shells out to buf, which is not guaranteed on a
	// test machine; publishing paths are covered without it.
	cfg.BreakingCheck = config.BreakingOff
	cfg.Tokens = []config.Token{{Subject: "test", SHA256: config.HashToken(testToken), Scopes: []string{"write"}}}

	srv, err := api.New(cfg, catalog, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cfg.BaseURL = ts.URL
	return ts, cfg
}

// buildFixture produces a publishable version: a schema archive plus one
// artifact per ecosystem, each in the exact format its package manager wants.
func buildFixture(t *testing.T, version string) (api.PublishManifest, map[string][]byte) {
	t.Helper()
	return buildFixtureAt(t, version, goModPath)
}

// buildFixtureAt is buildFixture with an explicit Go module path, which major
// versions need because Go encodes the major in the path from v2 on.
func buildFixtureAt(t *testing.T, version, modPath string) (api.PublishManifest, map[string][]byte) {
	t.Helper()
	schemaDir := t.TempDir()
	writeFile(t, filepath.Join(schemaDir, "buf.yaml"), "version: v2\n")
	writeFile(t, filepath.Join(schemaDir, "catalog/v1/catalog.proto"), "syntax = \"proto3\";\npackage catalog.v1;\n")
	schema, err := pack.Zip(schemaDir, "", pack.SkipHidden)
	if err != nil {
		t.Fatal(err)
	}

	goDir := t.TempDir()
	writeFile(t, filepath.Join(goDir, "go.mod"), "module "+modPath+"\n\ngo 1.21\n")
	writeFile(t, filepath.Join(goDir, "catalog/v1/catalog.pb.go"), "package catalogv1\n")
	goZip, err := pack.GoModuleZip(goDir, modPath, version)
	if err != nil {
		t.Fatal(err)
	}

	phpDir := t.TempDir()
	writeFile(t, filepath.Join(phpDir, "Catalog.php"), "<?php\n")
	writeFile(t, filepath.Join(phpDir, "composer.json"), `{"name": "acme/catalog-proto"}`)
	phpZip, err := pack.Zip(phpDir, "acme-catalog-proto-"+registry.EcosystemVersion(version), pack.SkipHidden)
	if err != nil {
		t.Fatal(err)
	}

	tsDir := t.TempDir()
	writeFile(t, filepath.Join(tsDir, "index.js"), "export {}\n")
	writeFile(t, filepath.Join(tsDir, "package.json"),
		`{"name": "@acme/catalog", "version": "`+registry.EcosystemVersion(version)+`"}`)
	tgz, err := pack.TarGz(tsDir, "package", pack.SkipHidden)
	if err != nil {
		t.Fatal(err)
	}

	pyDir := t.TempDir()
	writeFile(t, filepath.Join(pyDir, "catalog/v1/catalog_pb2.py"), "# generated\n")
	wheel, err := pack.Wheel(pyDir, pack.WheelMeta{
		Name:           "acme-catalog-proto",
		Version:        registry.PythonVersion(version),
		RequiresPython: ">=3.8",
	}, pack.SkipHidden)
	if err != nil {
		t.Fatal(err)
	}

	man := api.PublishManifest{
		Module:       "acme/catalog",
		Version:      version,
		SchemaSHA256: registry.SHA256Hex(schema),
		Labels:       []string{"main"},
		Artifacts: []registry.Artifact{
			{Target: registry.TargetGo, Name: modPath, Version: version, File: "go.zip", SHA256: registry.SHA256Hex(goZip)},
			{Target: registry.TargetComposer, Name: "acme/catalog-proto", Version: registry.EcosystemVersion(version), File: "composer.zip",
				Meta: map[string]any{"require": map[string]any{"php": ">=8.1"}, "autoload": map[string]any{"classmap": []any{"."}}}},
			{Target: registry.TargetNPM, Name: "@acme/catalog", Version: registry.EcosystemVersion(version), File: "npm.tgz",
				Meta: map[string]any{"dependencies": map[string]any{"@bufbuild/protobuf": "^2.2.0"}}},
			{Target: registry.TargetPython, Name: "acme-catalog-proto", Version: registry.PythonVersion(version), File: "python.whl",
				SHA256: registry.SHA256Hex(wheel), Meta: map[string]any{"requires_python": ">=3.8"}},
		},
	}
	files := map[string][]byte{
		"schema.zip":   schema,
		"go.zip":       goZip,
		"composer.zip": phpZip,
		"npm.tgz":      tgz,
		"python.whl":   wheel,
	}
	return man, files
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPublishRequiresAToken(t *testing.T) {
	ts, _ := newServer(t)
	man, files := buildFixture(t, "v1.0.0")

	c := client.New(ts.URL, "")
	if _, _, err := c.Publish(context.Background(), man, files); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("anonymous publish err = %v, want 401", err)
	}
	c = client.New(ts.URL, "wrong-token")
	if _, _, err := c.Publish(context.Background(), man, files); err == nil {
		t.Fatal("a bad token was accepted")
	}
}

func TestPublishAndServeEveryEcosystem(t *testing.T) {
	ts, _ := newServer(t)
	man, files := buildFixture(t, "v1.0.0")
	c := client.New(ts.URL, testToken)

	resp, ok, err := c.Publish(context.Background(), man, files)
	if err != nil || !ok {
		t.Fatalf("publish: ok=%v err=%v", ok, err)
	}
	if resp.Version.Version != "v1.0.0" {
		t.Fatalf("published version = %q", resp.Version.Version)
	}

	t.Run("go proxy", func(t *testing.T) {
		body := get(t, ts.URL+"/go/"+goModPath+"/@v/list")
		if strings.TrimSpace(body) != "v1.0.0" {
			t.Errorf("@v/list = %q", body)
		}
		var info struct{ Version string }
		if err := json.Unmarshal([]byte(get(t, ts.URL+"/go/"+goModPath+"/@v/v1.0.0.info")), &info); err != nil {
			t.Fatal(err)
		}
		if info.Version != "v1.0.0" {
			t.Errorf("@v/*.info Version = %q", info.Version)
		}
		if mod := get(t, ts.URL+"/go/"+goModPath+"/@v/v1.0.0.mod"); !strings.Contains(mod, "module "+goModPath) {
			t.Errorf("@v/*.mod = %q", mod)
		}
		if latest := get(t, ts.URL+"/go/"+goModPath+"/@latest"); !strings.Contains(latest, "v1.0.0") {
			t.Errorf("@latest = %q", latest)
		}
		if zip := get(t, ts.URL+"/go/"+goModPath+"/@v/v1.0.0.zip"); len(zip) < 100 {
			t.Errorf("@v/*.zip is suspiciously small: %d bytes", len(zip))
		}
	})

	t.Run("composer", func(t *testing.T) {
		// Composer 2 reads the root, then the per-package document it points at.
		var root struct {
			MetadataURL string `json:"metadata-url"`
		}
		if err := json.Unmarshal([]byte(get(t, ts.URL+"/composer/packages.json")), &root); err != nil {
			t.Fatal(err)
		}
		if root.MetadataURL == "" {
			t.Fatal("root document has no metadata-url")
		}
		var doc struct {
			Packages map[string][]struct {
				Name string
				Dist struct{ URL, Shasum string }
			}
		}
		if err := json.Unmarshal([]byte(get(t, ts.URL+"/composer/p2/acme/catalog-proto.json")), &doc); err != nil {
			t.Fatal(err)
		}
		versions := doc.Packages["acme/catalog-proto"]
		if len(versions) == 0 {
			t.Fatalf("package document missing versions: %+v", doc)
		}
		v := versions[0]
		if !strings.HasSuffix(v.Dist.URL, "/composer/dist/acme/catalog/v1.0.0.zip") {
			t.Errorf("dist url = %q", v.Dist.URL)
		}
		if body := get(t, v.Dist.URL); len(body) < 50 {
			t.Errorf("dist download is %d bytes", len(body))
		}
	})

	t.Run("npm", func(t *testing.T) {
		var doc struct {
			Name     string
			DistTags map[string]string `json:"dist-tags"`
			Versions map[string]struct {
				Dist struct{ Tarball string }
			}
		}
		if err := json.Unmarshal([]byte(get(t, ts.URL+"/npm/@acme%2fcatalog")), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.DistTags["latest"] != "1.0.0" {
			t.Errorf("dist-tags = %+v", doc.DistTags)
		}
		tarball := doc.Versions["1.0.0"].Dist.Tarball
		if body := get(t, tarball); len(body) < 50 {
			t.Errorf("tarball %q is %d bytes", tarball, len(body))
		}
	})

	t.Run("pypi simple index", func(t *testing.T) {
		root := get(t, ts.URL+"/pypi/simple")
		if !strings.Contains(root, "/pypi/simple/acme-catalog-proto/") {
			t.Errorf("index root = %q", root)
		}
		project := get(t, ts.URL+"/pypi/simple/acme-catalog-proto/")
		if !strings.Contains(project, "acme_catalog_proto-1.0.0-py3-none-any.whl") {
			t.Fatalf("project page = %q", project)
		}
		// pip verifies the fragment digest after downloading.
		if !strings.Contains(project, "#sha256=") {
			t.Error("project page has no sha256 fragment")
		}
		start := strings.Index(project, `href="`) + len(`href="`)
		end := strings.Index(project[start:], `"`) + start
		href := project[start:end]
		if body := get(t, strings.Split(href, "#")[0]); len(body) < 200 {
			t.Errorf("wheel download is %d bytes", len(body))
		}
		// pip normalizes the requested name, so an unnormalized path must work.
		if page := get(t, ts.URL+"/pypi/simple/Acme.Catalog_Proto/"); !strings.Contains(page, ".whl") {
			t.Error("unnormalized project name did not resolve")
		}
	})

	t.Run("api listing", func(t *testing.T) {
		mods, err := c.ListModules(context.Background())
		if err != nil || len(mods) != 1 || mods[0].Module != "acme/catalog" {
			t.Fatalf("ListModules = %+v, %v", mods, err)
		}
		idx, err := c.Module(context.Background(), "acme/catalog")
		if err != nil {
			t.Fatal(err)
		}
		if idx.Labels["main"] != "v1.0.0" {
			t.Errorf("labels = %+v", idx.Labels)
		}
	})

	t.Run("schema download feeds buf breaking", func(t *testing.T) {
		body, err := c.DownloadSchema(context.Background(), "acme/catalog", "latest")
		if err != nil || len(body) == 0 {
			t.Fatalf("DownloadSchema: %d bytes, %v", len(body), err)
		}
	})
}

func TestRepublishingDifferentContentIsRefused(t *testing.T) {
	ts, _ := newServer(t)
	c := client.New(ts.URL, testToken)

	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := c.Publish(context.Background(), man, files); err != nil || !ok {
		t.Fatalf("first publish: %v %v", ok, err)
	}
	other, otherFiles := buildFixture(t, "v1.0.0")
	otherFiles["schema.zip"] = append(otherFiles["schema.zip"], 0)
	other.SchemaSHA256 = registry.SHA256Hex(otherFiles["schema.zip"])
	if _, _, err := c.Publish(context.Background(), other, otherFiles); err == nil {
		t.Fatal("republishing different content under the same version was allowed")
	}
}

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s: %s", url, resp.Status, body)
	}
	return string(body)
}
