package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smoreg/protobuffet/internal/config"
	"github.com/smoreg/protobuffet/internal/registry"
)

func testConfig(t *testing.T) config.Server {
	t.Helper()
	cfg := config.DefaultServer()
	cfg.Data = filepath.Join(t.TempDir(), "data")
	cfg.BreakingCheck = config.BreakingOff
	cfg.Tokens = []config.Token{{Subject: "t", SHA256: config.HashToken("token"), Scopes: []string{"admin"}}}
	return cfg
}

func TestBuildServesHealthz(t *testing.T) {
	srv, _, err := build(testConfig(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d", resp.StatusCode)
	}
	// Timeouts protect against a stuck client holding a connection open.
	if srv.ReadHeaderTimeout == 0 || srv.IdleTimeout == 0 {
		t.Error("server has no timeouts")
	}
}

func TestBuildFailsOnUnwritableData(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not apply")
	}
	dir := t.TempDir()
	readonly := filepath.Join(dir, "readonly")
	if err := os.Mkdir(readonly, 0o500); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Data = filepath.Join(readonly, "data")

	// A container started with a root-owned volume fails exactly like this,
	// and it must fail at startup rather than at the first publish.
	if _, _, err := build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("build succeeded on an unwritable data directory")
	}
}

func TestBuildRefusesRequiredModeWithoutBuf(t *testing.T) {
	cfg := testConfig(t)
	cfg.BreakingCheck = config.BreakingRequired
	cfg.BufPath = "/nonexistent/buf"
	if _, _, err := build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Error("started in required mode without buf")
	}
}

func TestBaseURLOrDerived(t *testing.T) {
	if got := baseURLOrDerived(""); got != "(derived from request)" {
		t.Errorf("empty = %q", got)
	}
	if got := baseURLOrDerived("https://x.example"); got != "https://x.example" {
		t.Errorf("set = %q", got)
	}
}

func TestRunGCPrunesAndReportsOrphans(t *testing.T) {
	cfg := testConfig(t)
	cfg.Retention.KeepPseudoVersions = 1

	// Publish two pseudo-versions and a release straight into the store, as a
	// registry that has been taking CI pushes for a while would look.
	store, err := registry.NewStore(cfg.Data)
	if err != nil {
		t.Fatal(err)
	}
	mod := registry.Module{Owner: "acme", Name: "catalog"}
	base := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	for i, v := range []string{
		"v1.0.0",
		"v0.0.0-20260101000000-aaaaaaaaaaaa",
		"v0.0.0-20260901000000-bbbbbbbbbbbb",
	} {
		if _, err := store.Publish(registry.PublishRequest{
			Module: mod, Version: v, SchemaZip: []byte(v),
			Now: base.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// An orphaned directory, as a crash mid-publish leaves behind.
	orphan := filepath.Join(cfg.Data, "modules", "acme", "catalog", "versions", "v7.7.7")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// A dry run reports without touching anything.
	if err := runGC(cfg, log, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadAll(mod, "v0.0.0-20260101000000-aaaaaaaaaaaa", "schema.zip"); err != nil {
		t.Errorf("dry run deleted a version: %v", err)
	}

	if err := runGC(cfg, log, false); err != nil {
		t.Fatal(err)
	}
	// The oldest pseudo-version goes, the newest stays, the release is never
	// touched, and the orphan is gone.
	if _, err := store.ReadAll(mod, "v0.0.0-20260101000000-aaaaaaaaaaaa", "schema.zip"); err == nil {
		t.Error("old pseudo-version survived")
	}
	if _, err := store.ReadAll(mod, "v0.0.0-20260901000000-bbbbbbbbbbbb", "schema.zip"); err != nil {
		t.Errorf("newest pseudo-version was pruned: %v", err)
	}
	if _, err := store.ReadAll(mod, "v1.0.0", "schema.zip"); err != nil {
		t.Errorf("a release was pruned: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("orphaned directory survived")
	}
}

func TestRunGCRefusesWhileTheRegistryHoldsTheLock(t *testing.T) {
	cfg := testConfig(t)
	if _, _, err := build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	// The running server holds the data directory; a second writer would
	// interleave index rewrites with a publish.
	err := runGC(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), true)
	if err == nil {
		t.Fatal("gc ran against a locked data directory")
	}
	if !strings.Contains(err.Error(), "/api/v1/gc") {
		t.Errorf("err = %v, want it to point at the API", err)
	}
}
