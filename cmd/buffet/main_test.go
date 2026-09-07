package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
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

const cliToken = "cli-token"

// registryServer starts a registry the CLI can talk to, with one module
// already published.
func registryServer(t *testing.T) *httptest.Server {
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
	cfg.BaseURL = ""
	cfg.BreakingCheck = config.BreakingOff
	cfg.Tokens = []config.Token{{Subject: "cli", SHA256: config.HashToken(cliToken), Scopes: []string{"admin"}}}

	srv, err := api.New(cfg, catalog, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Publish something for list and info to show.
	goDir := t.TempDir()
	writeFile(t, filepath.Join(goDir, "go.mod"), "module registry.test/gen/go/acme/catalog\n\ngo 1.21\n")
	writeFile(t, filepath.Join(goDir, "api.go"), "package catalogv1\n")
	body, err := pack.GoModuleZip(goDir, "registry.test/gen/go/acme/catalog", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	_, ok, err := client.New(ts.URL, cliToken).Publish(context.Background(), api.PublishManifest{
		Module: "acme/catalog", Version: "v1.0.0", Labels: []string{"main"},
		Artifacts: []registry.Artifact{{
			Target: registry.TargetGo, Name: "registry.test/gen/go/acme/catalog",
			Version: "v1.0.0", File: "go.zip",
		}},
		SchemaSHA256: registry.SHA256Hex([]byte("schema")),
	}, map[string][]byte{"schema.zip": []byte("schema"), "go.zip": body})
	if err != nil || !ok {
		t.Fatalf("seed publish: %v %v", ok, err)
	}
	return ts
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

// exec runs the CLI and returns the exit code with what it printed.
func execCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestCLIUsageAndVersion(t *testing.T) {
	if code, _, stderr := execCLI(t); code != 2 || !strings.Contains(stderr, "buffet publish") {
		t.Errorf("no args: code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := execCLI(t, "nonsense"); code != 2 || !strings.Contains(stderr, "unknown command") {
		t.Errorf("unknown command: code=%d stderr=%q", code, stderr)
	}
	if code, stdout, _ := execCLI(t, "version"); code != 0 || !strings.Contains(stdout, "buffet") {
		t.Errorf("version: code=%d stdout=%q", code, stdout)
	}
	if code, stdout, _ := execCLI(t, "help"); code != 0 || !strings.Contains(stdout, "buffet publish") {
		t.Errorf("help: code=%d", code)
	}
}

func TestCLIToken(t *testing.T) {
	code, stdout, _ := execCLI(t, "token")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	// Both halves must be printed: the secret for CI and the digest for the
	// server config.
	if !strings.Contains(stdout, "token:") || !strings.Contains(stdout, "sha256:") {
		t.Errorf("token output = %q", stdout)
	}
	var secret string
	for _, line := range strings.Split(stdout, "\n") {
		if after, ok := strings.CutPrefix(line, "token:"); ok {
			secret = strings.TrimSpace(after)
		}
	}
	digest := config.HashToken(secret)
	if !strings.Contains(stdout, digest) {
		t.Errorf("printed digest does not match the printed token")
	}
}

func TestCLIListAndInfo(t *testing.T) {
	ts := registryServer(t)
	t.Setenv("BUFFET_REGISTRY", ts.URL)
	t.Setenv("BUFFET_TOKEN", cliToken)

	code, stdout, stderr := execCLI(t, "list")
	if code != 0 {
		t.Fatalf("list: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "acme/catalog") || !strings.Contains(stdout, "v1.0.0") {
		t.Errorf("list output = %q", stdout)
	}

	code, stdout, stderr = execCLI(t, "info", "acme/catalog")
	if code != 0 {
		t.Fatalf("info: code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{"v1.0.0", "go:registry.test/gen/go/acme/catalog", "main"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("info output missing %q: %q", want, stdout)
		}
	}

	// info without an argument is a usage error, not a crash.
	if code, _, stderr := execCLI(t, "info"); code != 2 || !strings.Contains(stderr, "buffet info") {
		t.Errorf("info without args: code=%d stderr=%q", code, stderr)
	}
}

func TestCLIWithoutRegistryFails(t *testing.T) {
	t.Setenv("BUFFET_REGISTRY", "")
	t.Setenv("BUFFET_TOKEN", "")
	// Point the credentials file somewhere empty so a developer's real
	// credentials cannot make this pass.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	code, _, stderr := execCLI(t, "list")
	if code != 1 || !strings.Contains(stderr, "no registry") {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
}

func TestCLILoginStoresTheToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("HOME", home)

	code, stdout, stderr := execCLI(t, "login", "-registry", "https://schemas.example.com", "-token", "abc123")
	if code != 0 {
		t.Fatalf("login: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "credentials.json") {
		t.Errorf("login output = %q", stdout)
	}
	if got := storedToken("https://schemas.example.com"); got != "abc123" {
		t.Errorf("stored token = %q", got)
	}
	// A trailing slash must not create a second entry.
	if got := storedToken("https://schemas.example.com/"); got != "abc123" {
		t.Errorf("trailing slash lookup = %q", got)
	}
	if code, _, _ := execCLI(t, "login", "-registry", "https://x.example"); code != 2 {
		t.Error("login without a token succeeded")
	}
}

func TestCLIPublishDryRun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	writeFile(t, filepath.Join(dir, "proto/catalog/v1/catalog.proto"), "syntax = \"proto3\";\npackage catalog.v1;\n")
	writeFile(t, filepath.Join(dir, "gen/go/api.pb.go"), "package catalogv1\n")
	writeFile(t, filepath.Join(dir, "buffet.yaml"), `
version: v1
module: acme/catalog
proto: proto
targets:
  go:
    module: registry.test/gen/go/acme/catalog
    template: buf.gen.go.yaml
    out: gen/go
`)
	// -skip-generate and -skip-lint keep buf out of the picture; the point is
	// that a dry run builds artifacts and pushes nothing.
	code, stdout, stderr := execCLI(t, "publish",
		"-config", dir, "-version", "v1.2.3", "-dry-run", "-skip-generate", "-skip-lint")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{"acme/catalog", "v1.2.3", "target   go", "dry run: nothing was pushed"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
}

// fakeBufOnPath installs a buf script so `buffet check` can run without the
// real toolchain.
func fakeBufOnPath(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "buf"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCLICheck(t *testing.T) {
	ts := registryServer(t)
	t.Setenv("BUFFET_REGISTRY", ts.URL)
	t.Setenv("BUFFET_TOKEN", cliToken)

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	writeFile(t, filepath.Join(dir, "proto/catalog/v1/catalog.proto"), "syntax = \"proto3\";\n")
	writeFile(t, filepath.Join(dir, "buffet.yaml"), `
version: v1
module: acme/catalog
proto: proto
targets:
  go:
    module: registry.test/gen/go/acme/catalog
    template: buf.gen.go.yaml
    out: gen/go
`)
	writeFile(t, filepath.Join(dir, "gen/go/api.pb.go"), "package catalogv1\n")

	t.Run("compatible", func(t *testing.T) {
		fakeBufOnPath(t, "exit 0\n")
		code, stdout, stderr := execCLI(t, "check", "-config", dir)
		if code != 0 {
			t.Fatalf("code=%d stderr=%q", code, stderr)
		}
		if !strings.Contains(stdout, "compatible with acme/catalog") {
			t.Errorf("stdout = %q", stdout)
		}
	})

	t.Run("breaking", func(t *testing.T) {
		fakeBufOnPath(t, `echo "catalog.proto:1:1:Field \"2\" was deleted."; exit 100`+"\n")
		code, stdout, _ := execCLI(t, "check", "-config", dir)
		if code != 1 {
			t.Errorf("code = %d, want 1", code)
		}
		// The findings have to reach the developer, not just the exit code.
		if !strings.Contains(stdout, "was deleted") {
			t.Errorf("findings missing from output: %q", stdout)
		}
	})
}
