package pack

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestZipIsDeterministic(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"a.proto":       "syntax = \"proto3\";",
		"sub/b.proto":   "syntax = \"proto3\";",
		".hidden/x.txt": "ignored",
	})
	first, err := Zip(dir, "", SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	// Touch a file so mtimes differ between the two runs.
	if err := os.Chtimes(filepath.Join(dir, "a.proto"), epoch, epoch.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	second, err := Zip(dir, "", SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("packing the same tree twice produced different bytes")
	}

	zr, err := zip.NewReader(bytes.NewReader(first), int64(len(first)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if len(names) != 2 || names[0] != "a.proto" || names[1] != "sub/b.proto" {
		t.Fatalf("entries = %v, want the two protos in sorted order", names)
	}
}

func TestGoModuleZipLayout(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"go.mod":               "module example.com/gen/go/acme/catalog\n\ngo 1.21\n",
		"catalog/v1/api.pb.go": "package catalogv1\n",
	})
	body, err := GoModuleZip(dir, "example.com/gen/go/acme/catalog", "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	want := "example.com/gen/go/acme/catalog@v1.2.3/go.mod"
	var found bool
	for _, f := range zr.File {
		if f.Name == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("module zip has no %s entry", want)
	}
}

func TestTarGzUsesPackagePrefix(t *testing.T) {
	dir := writeTree(t, map[string]string{"index.js": "export {}\n"})
	body, err := TarGz(dir, "package", SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if h.Name != "package/index.js" {
		t.Fatalf("entry = %q, want package/index.js", h.Name)
	}
	if _, err := TarGz(dir, "", SkipHidden); err == nil {
		t.Error("an empty prefix should be rejected: npm requires package/")
	}
}

func TestExtractZipRefusesTraversal(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("../escaped.txt")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "nope")
	zw.Close()

	if err := ExtractZip(buf.Bytes(), t.TempDir(), false); err == nil {
		t.Fatal("a zip entry escaping the destination was extracted")
	}
}

func TestWheelLayout(t *testing.T) {
	dir := writeTree(t, map[string]string{"catalog/v1/catalog_pb2.py": "# generated\n"})
	body, err := Wheel(dir, WheelMeta{
		Name:           "acme-catalog-proto",
		Version:        "1.0.0",
		RequiresDist:   []string{"protobuf>=5.27"},
		RequiresPython: ">=3.8",
	}, SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(content)
	}
	for _, want := range []string{
		"catalog/v1/catalog_pb2.py",
		"acme_catalog_proto-1.0.0.dist-info/METADATA",
		"acme_catalog_proto-1.0.0.dist-info/WHEEL",
		"acme_catalog_proto-1.0.0.dist-info/RECORD",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("wheel is missing %s", want)
		}
	}
	if md := got["acme_catalog_proto-1.0.0.dist-info/METADATA"]; !strings.Contains(md, "Requires-Dist: protobuf>=5.27") {
		t.Errorf("METADATA = %q", md)
	}
	// RECORD must cover every file, including itself with an empty digest.
	record := got["acme_catalog_proto-1.0.0.dist-info/RECORD"]
	if !strings.Contains(record, "catalog/v1/catalog_pb2.py,sha256=") ||
		!strings.Contains(record, "dist-info/RECORD,,") {
		t.Errorf("RECORD = %q", record)
	}
	if name := WheelFilename("acme-catalog-proto", "1.0.0"); name != "acme_catalog_proto-1.0.0-py3-none-any.whl" {
		t.Errorf("WheelFilename = %q", name)
	}
	if n := NormalizePythonName("Acme.Catalog_Proto"); n != "acme-catalog-proto" {
		t.Errorf("NormalizePythonName = %q", n)
	}
}

func TestExtractZipRoundTrip(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"buf.yaml":                   "version: v2\n",
		"proto/catalog/v1/api.proto": "syntax = \"proto3\";\n",
	})
	body, err := Zip(dir, "", SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := ExtractZip(body, out, false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(out, "proto/catalog/v1/api.proto"))
	if err != nil || string(got) != "syntax = \"proto3\";\n" {
		t.Fatalf("extracted content = %q, %v", got, err)
	}

	// A prefixed archive, as produced for Composer, with the prefix stripped.
	prefixed, err := Zip(dir, "acme-catalog-1.0.0", SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	stripped := t.TempDir()
	if err := ExtractZip(prefixed, stripped, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stripped, "buf.yaml")); err != nil {
		t.Errorf("strip did not remove the prefix: %v", err)
	}
}

func TestFilesFiltering(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"keep.proto":              "x",
		".git/config":             "x",
		"vendor/dep/file.go":      "x",
		"node_modules/pkg/i.js":   "x",
		"sub/.hidden/thing.proto": "x",
		"sub/keep.proto":          "x",
	})
	files, err := Files(dir, SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"keep.proto", "sub/keep.proto"}
	if len(files) != len(want) {
		t.Fatalf("files = %v, want %v", files, want)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Errorf("files[%d] = %q, want %q", i, files[i], want[i])
		}
	}
	// An empty selection is an error: publishing an empty artifact is never
	// what the caller meant.
	if _, err := Zip(t.TempDir(), "", SkipHidden); err == nil {
		t.Error("packing an empty directory succeeded")
	}
}

func TestWheelRequiresNameAndVersion(t *testing.T) {
	dir := writeTree(t, map[string]string{"x.py": "# x\n"})
	if _, err := Wheel(dir, WheelMeta{Version: "1.0.0"}, SkipHidden); err == nil {
		t.Error("wheel without a name accepted")
	}
	if _, err := Wheel(dir, WheelMeta{Name: "x"}, SkipHidden); err == nil {
		t.Error("wheel without a version accepted")
	}
}
