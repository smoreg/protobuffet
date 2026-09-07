package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/smoreg/protobuffet/internal/config"
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
