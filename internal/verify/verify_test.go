package verify

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smoreg/protobuffet/internal/pack"
	"github.com/smoreg/protobuffet/internal/registry"
)

func writeAt(t *testing.T, dir, name, body string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		writeAt(t, dir, name, body)
	}
	return dir
}

func TestGoModuleZip(t *testing.T) {
	const mod = "registry.test/gen/go/acme/catalog"
	good := tree(t, map[string]string{
		"go.mod":            "module " + mod + "\n\ngo 1.21\n",
		"catalog/v1/api.go": "package catalogv1\n",
	})
	body, err := pack.GoModuleZip(good, mod, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := GoModuleZip(body, mod, "v1.0.0"); err != nil {
		t.Fatalf("valid module rejected: %v", err)
	}
	// The version in the artifact must match the zip prefix, or `go get`
	// downloads a zip it then refuses.
	if err := GoModuleZip(body, mod, "v2.0.0"); err == nil {
		t.Error("wrong version accepted")
	}

	// A zip with the right prefix but a go.mod naming another module: the go
	// command downloads it and then refuses, so the registry must catch it.
	err = GoModuleZip(handmadeZip(t, map[string]string{
		mod + "@v1.0.0/go.mod":            "module registry.test/gen/go/other\n\ngo 1.21\n",
		mod + "@v1.0.0/catalog/v1/api.go": "package catalogv1\n",
	}), mod, "v1.0.0")
	if err == nil || !strings.Contains(err.Error(), "declares module") {
		t.Errorf("mismatched go.mod: err = %v", err)
	}

	// Missing go.mod entirely.
	err = GoModuleZip(handmadeZip(t, map[string]string{
		mod + "@v1.0.0/catalog/v1/api.go": "package catalogv1\n",
	}), mod, "v1.0.0")
	if err == nil || !strings.Contains(err.Error(), "go.mod is missing") {
		t.Errorf("zip without go.mod: err = %v", err)
	}
}

// handmadeZip builds an archive that the packing helpers would refuse to
// produce, which is exactly what a buggy or hostile client can upload.
func handmadeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestWheel(t *testing.T) {
	dir := tree(t, map[string]string{"catalog/v1/catalog_pb2.py": "# generated\n"})
	body, err := pack.Wheel(dir, pack.WheelMeta{Name: "acme-catalog-proto", Version: "1.0.0"}, pack.SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	if err := Wheel(body, "acme-catalog-proto", "1.0.0"); err != nil {
		t.Fatalf("valid wheel rejected: %v", err)
	}
	if err := Wheel(body, "acme-catalog-proto", "2.0.0"); err == nil {
		t.Error("wheel with the wrong version accepted")
	}
	if err := Wheel(body, "other-package", "1.0.0"); err == nil {
		t.Error("wheel with the wrong name accepted")
	}
	// A plain zip without dist-info is what a misconfigured template produces.
	plain, err := pack.Zip(dir, "", pack.SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	if err := Wheel(plain, "acme-catalog-proto", "1.0.0"); err == nil {
		t.Error("zip without dist-info accepted as a wheel")
	}
}

func TestComposerZip(t *testing.T) {
	dir := tree(t, map[string]string{
		"composer.json": `{"name": "acme/catalog-proto"}`,
		"Catalog.php":   "<?php\n",
	})
	body, err := pack.Zip(dir, "acme-catalog-proto-1.0.0", pack.SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	if err := ComposerZip(body, "acme/catalog-proto"); err != nil {
		t.Fatalf("valid archive rejected: %v", err)
	}
	if err := ComposerZip(body, "acme/other"); err == nil {
		t.Error("mismatched composer.json name accepted")
	}
	// Composer strips one leading directory; without it the package installs
	// its files into the wrong place.
	flat, err := pack.Zip(dir, "", pack.SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	if err := ComposerZip(flat, "acme/catalog-proto"); err == nil {
		t.Error("archive without a root directory accepted")
	}
}

func TestNPMTarball(t *testing.T) {
	dir := tree(t, map[string]string{
		"package.json": `{"name": "@acme/catalog", "version": "1.0.0"}`,
		"index.js":     "export {}\n",
	})
	body, err := pack.TarGz(dir, "package", pack.SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	if err := NPMTarball(body, "@acme/catalog", "1.0.0"); err != nil {
		t.Fatalf("valid tarball rejected: %v", err)
	}
	if err := NPMTarball(body, "@acme/catalog", "1.0.1"); err == nil {
		t.Error("mismatched version accepted")
	}
	wrongPrefix, err := pack.TarGz(dir, "acme-catalog", pack.SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	if err := NPMTarball(wrongPrefix, "@acme/catalog", "1.0.0"); err == nil {
		t.Error("tarball without the package/ prefix accepted")
	}
}

func TestArtifactDispatch(t *testing.T) {
	if err := Artifact(registry.Artifact{Target: "ruby"}, nil); err == nil {
		t.Error("unknown target accepted")
	}
}

func TestGoModuleMajorSuffix(t *testing.T) {
	// From v2 on, Go requires the major version in the import path. Publishing
	// without it produces a module the go command downloads and then refuses.
	body := handmadeZip(t, map[string]string{
		"registry.test/gen/go/acme/catalog@v2.0.0/go.mod": "module registry.test/gen/go/acme/catalog\n",
	})
	err := GoModuleZip(body, "registry.test/gen/go/acme/catalog", "v2.0.0")
	if err == nil || !strings.Contains(err.Error(), "/v2") {
		t.Errorf("err = %v, want a hint about the /v2 suffix", err)
	}

	// With the suffix it passes.
	const v2Path = "registry.test/gen/go/acme/catalog/v2"
	ok := handmadeZip(t, map[string]string{
		v2Path + "@v2.0.0/go.mod": "module " + v2Path + "\n",
		v2Path + "@v2.0.0/api.go": "package catalogv1\n",
	})
	if err := GoModuleZip(ok, v2Path, "v2.0.0"); err != nil {
		t.Errorf("valid v2 module rejected: %v", err)
	}
	// v0 and v1 carry no suffix.
	v1 := handmadeZip(t, map[string]string{
		"registry.test/gen/go/acme/catalog@v1.0.0/go.mod": "module registry.test/gen/go/acme/catalog\n",
	})
	if err := GoModuleZip(v1, "registry.test/gen/go/acme/catalog", "v1.0.0"); err != nil {
		t.Errorf("v1 module rejected: %v", err)
	}
}
