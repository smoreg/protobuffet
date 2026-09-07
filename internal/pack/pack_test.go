package pack

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
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
