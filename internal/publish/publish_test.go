package publish

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smoreg/protobuffet/internal/buftool"
	"github.com/smoreg/protobuffet/internal/config"
	"github.com/smoreg/protobuffet/internal/registry"
)

// moduleDir lays out a schema repository: buf config, protos, and generated
// output that a fake buf "produces".
func moduleDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "buf.yaml", "version: v2\nmodules:\n  - path: proto\n")
	write(t, dir, "buf.lock", "version: v2\n")
	write(t, dir, "proto/catalog/v1/catalog.proto", "syntax = \"proto3\";\npackage catalog.v1;\n")
	write(t, dir, "buffet.yaml", `
version: v1
module: acme/catalog
proto: proto
targets:
  go:
    module: registry.test/gen/go/acme/catalog
    template: buf.gen.go.yaml
    out: gen/go
    require:
      - google.golang.org/protobuf v1.36.12
  composer:
    package: acme/catalog-proto
    template: buf.gen.php.yaml
    out: gen/php
    require:
      php: ">=8.1"
  python:
    package: acme-catalog-proto
    template: buf.gen.py.yaml
    out: gen/python
    requires:
      - "protobuf>=5.27"
  npm:
    package: "@acme/catalog"
    template: buf.gen.ts.yaml
    out: gen/ts
    dependencies:
      "@bufbuild/protobuf": "^2.2.0"
`)
	// Pre-create what generation would produce; the fake buf does nothing.
	write(t, dir, "gen/go/catalog/v1/catalog.pb.go", "package catalogv1\n")
	write(t, dir, "gen/php/Catalog.php", "<?php\n")
	write(t, dir, "gen/python/catalog/v1/catalog_pb2.py", "# generated\n")
	write(t, dir, "gen/ts/index.js", "export {}\n")
	return dir
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func noopBuf(t *testing.T) *buftool.Buf {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "buf")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := buftool.Find(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSchemaArchiveHoldsOnlyTheContract(t *testing.T) {
	dir := moduleDir(t)
	m, err := config.LoadModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	body, err := SchemaArchive(m)
	if err != nil {
		t.Fatal(err)
	}
	names := zipNames(t, body)
	want := map[string]bool{
		"buf.yaml":                       true,
		"buf.lock":                       true,
		"proto/catalog/v1/catalog.proto": true,
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("archive contains %q, which is not part of the contract", n)
		}
		delete(want, n)
	}
	for n := range want {
		t.Errorf("archive is missing %q", n)
	}

	// The digest identifies a schema revision, so generated output must not
	// influence it.
	write(t, dir, "gen/go/catalog/v1/catalog.pb.go", "package catalogv1 // changed\n")
	again, err := SchemaArchive(m)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, again) {
		t.Error("regenerating code changed the schema digest")
	}
}

func TestResolveVersion(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 15, 4, 5, 0, time.UTC)
	digest := strings.Repeat("ab", 32)

	if v, err := ResolveVersion(ctx, "1.2.3", t.TempDir(), digest, now); err != nil || v != "v1.2.3" {
		t.Errorf("explicit = %q, %v", v, err)
	}
	if _, err := ResolveVersion(ctx, "not-a-version", t.TempDir(), digest, now); err == nil {
		t.Error("invalid explicit version accepted")
	}
	// No git tag: a pseudo-version keeps every revision addressable.
	v, err := ResolveVersion(ctx, "", t.TempDir(), digest, now)
	if err != nil || v != "v0.0.0-20260907150405-ababababab ab"[:len("v0.0.0-20260907150405-")]+digest[:12] {
		t.Errorf("pseudo = %q, %v", v, err)
	}

	if _, err := exec.LookPath("git"); err == nil {
		dir := t.TempDir()
		gitInit(t, dir)
		if v, err := ResolveVersion(ctx, "", dir, digest, now); err != nil || v != "v2.5.0" {
			t.Errorf("tagged repo = %q, %v", v, err)
		}
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	write(t, dir, "file.txt", "x\n")
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"tag", "v2.5.0"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %s", out)
		}
	}
}

func TestPrepareBuildsEveryTarget(t *testing.T) {
	dir := moduleDir(t)
	m, err := config.LoadModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	build, err := Prepare(context.Background(), noopBuf(t), m, Options{Version: "v1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	if build.Version != "v1.2.3" || build.Module != "acme/catalog" {
		t.Fatalf("build = %+v", build)
	}

	byTarget := map[registry.Target]registry.Artifact{}
	for _, a := range build.Artifacts {
		byTarget[a.Target] = a
		if _, ok := build.Files[a.File]; !ok {
			t.Errorf("artifact %s references missing file %q", a.Target, a.File)
		}
		if a.SHA256 == "" {
			t.Errorf("artifact %s has no digest", a.Target)
		}
	}
	if len(byTarget) != 4 {
		t.Fatalf("targets = %v", byTarget)
	}
	// Each ecosystem spells the version its own way, and getting this wrong
	// makes the package unresolvable rather than merely odd.
	if got := byTarget[registry.TargetGo].Version; got != "v1.2.3" {
		t.Errorf("go version = %q", got)
	}
	for _, target := range []registry.Target{registry.TargetComposer, registry.TargetNPM, registry.TargetPython} {
		if got := byTarget[target].Version; got != "1.2.3" {
			t.Errorf("%s version = %q, want 1.2.3", target, got)
		}
	}
	// The Go zip must carry the module@version prefix or the proxy serves an
	// archive the go command rejects.
	names := zipNames(t, build.Files["go.zip"])
	prefix := "registry.test/gen/go/acme/catalog@v1.2.3/"
	var sawGoMod bool
	for _, n := range names {
		if !strings.HasPrefix(n, prefix) {
			t.Errorf("go.zip entry %q lacks the module prefix", n)
		}
		if n == prefix+"go.mod" {
			sawGoMod = true
		}
	}
	if !sawGoMod {
		t.Error("go.zip has no go.mod")
	}
}

func TestPrepareWithoutBufNeedsSkipGenerate(t *testing.T) {
	m, err := config.LoadModule(moduleDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(context.Background(), nil, m, Options{Version: "v1.0.0"}); err == nil {
		t.Error("Prepare without buf and without SkipGenerate succeeded")
	}
	if _, err := Prepare(context.Background(), nil, m, Options{Version: "v1.0.0", SkipGenerate: true}); err != nil {
		t.Errorf("SkipGenerate should not need buf: %v", err)
	}
}

func TestWriteGoModRejectsMalformedRequire(t *testing.T) {
	dir := t.TempDir()
	err := writeGoMod(dir, &config.GoTarget{
		Module: "registry.test/x", GoVersion: "1.21",
		Require: []string{"google.golang.org/protobuf"}, // version missing
	})
	if err == nil {
		t.Fatal("require without a version accepted")
	}
	if err := writeGoMod(dir, &config.GoTarget{
		Module: "registry.test/x", GoVersion: "1.21",
		Require: []string{"google.golang.org/protobuf v1.36.12"},
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"module registry.test/x", "go 1.21", "google.golang.org/protobuf v1.36.12"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("go.mod missing %q:\n%s", want, body)
		}
	}
}

func zipNames(t *testing.T, body []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names
}

func TestCheckMajorSuffix(t *testing.T) {
	const base = "registry.test/gen/go/acme/catalog"
	for _, version := range []string{"v0.1.0", "v1.9.9"} {
		if err := checkMajorSuffix(base, version); err != nil {
			t.Errorf("%s rejected: %v", version, err)
		}
	}
	err := checkMajorSuffix(base, "v2.0.0")
	if err == nil {
		t.Fatal("v2 without a path suffix accepted")
	}
	// The message has to say what to change, because the fix is in two files.
	for _, want := range []string{"/v2", "targets.go.module", "go_package_prefix"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %v", want, err)
		}
	}
	if err := checkMajorSuffix(base+"/v2", "v2.1.0"); err != nil {
		t.Errorf("suffixed path rejected: %v", err)
	}
	if err := checkMajorSuffix(base+"/v3", "v3.0.0"); err != nil {
		t.Errorf("v3 rejected: %v", err)
	}
}

func TestPrepareDoesNotMutateTheWorkingTree(t *testing.T) {
	dir := moduleDir(t)
	m, err := config.LoadModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	if _, err := Prepare(context.Background(), noopBuf(t), m, Options{Version: "v1.0.0"}); err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, dir)

	// A publish that writes go.mod, composer.json or package.json into the
	// user's tree either pollutes the repository or overwrites a file they
	// maintain by hand.
	for name := range after {
		if _, existed := before[name]; !existed {
			t.Errorf("publish created %s in the working tree", name)
		}
	}
	for name, sum := range before {
		if after[name] != sum {
			t.Errorf("publish modified %s", name)
		}
	}
}

func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out[rel] = string(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
