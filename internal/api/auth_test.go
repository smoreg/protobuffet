package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/smoreg/protobuffet/internal/api"
	"github.com/smoreg/protobuffet/internal/client"
	"github.com/smoreg/protobuffet/internal/config"
	"github.com/smoreg/protobuffet/internal/registry"
)

// newPrivateServer serves a registry that requires a token even to read.
func newPrivateServer(t *testing.T, tokens ...config.Token) *httptest.Server {
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
	cfg.PublicRead = false
	cfg.BreakingCheck = config.BreakingOff
	cfg.Tokens = tokens

	srv, err := api.New(cfg, catalog, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestPrivateRegistryRequiresATokenToRead(t *testing.T) {
	readToken := "read-token"
	writeToken := "write-token"
	ts := newPrivateServer(t,
		config.Token{Subject: "reader", SHA256: config.HashToken(readToken), Scopes: []string{"read"}},
		config.Token{Subject: "ci", SHA256: config.HashToken(writeToken), Scopes: []string{"write"}},
	)

	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := client.New(ts.URL, writeToken).Publish(context.Background(), man, files); err != nil || !ok {
		t.Fatalf("publish with a write token: %v %v", ok, err)
	}

	// Anonymous read is refused, and the challenge names both schemes so the
	// go command and Composer know what to send.
	resp, err := http.Get(ts.URL + "/go/" + goModPath + "/@v/list")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous read: status %d, want 401", resp.StatusCode)
	}
	if auth := resp.Header.Get("WWW-Authenticate"); auth == "" {
		t.Error("no WWW-Authenticate challenge")
	}

	// Basic auth is how the go command (via .netrc) and Composer authenticate.
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/go/"+goModPath+"/@v/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("anything", readToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("basic auth read: status %d", resp.StatusCode)
	}

	// A read token must not be able to publish.
	other, otherFiles := buildFixture(t, "v1.1.0")
	if _, _, err := client.New(ts.URL, readToken).Publish(context.Background(), other, otherFiles); err == nil {
		t.Error("read token published a version")
	}
}

func TestAllowBreakingNeedsAnAdminToken(t *testing.T) {
	writeToken := "write-token"
	ts := newPrivateServer(t,
		config.Token{Subject: "ci", SHA256: config.HashToken(writeToken), Scopes: []string{"write"}},
	)
	man, files := buildFixture(t, "v1.0.0")
	man.AllowBreaking = true

	_, _, err := client.New(ts.URL, writeToken).Publish(context.Background(), man, files)
	if err == nil {
		t.Fatal("a write token used allow_breaking")
	}
}

func TestReloadTokensSwapsCredentials(t *testing.T) {
	dir := t.TempDir()
	tokensPath := filepath.Join(dir, "tokens.yaml")
	writeTokens := func(token, subject string) {
		t.Helper()
		body := "tokens:\n  - subject: " + subject + "\n    sha256: " + config.HashToken(token) + "\n    scopes: [write]\n"
		if err := os.WriteFile(tokensPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeTokens("first-token", "ci")

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
	cfg.TokensFile = tokensPath
	cfg.Tokens = []config.Token{{Subject: "ci", SHA256: config.HashToken("first-token"), Scopes: []string{"write"}}}

	srv, err := api.New(cfg, catalog, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := client.New(ts.URL, "first-token").Publish(context.Background(), man, files); err != nil || !ok {
		t.Fatalf("publish with the first token: %v %v", ok, err)
	}

	// Rotate: the old credential stops working and the new one starts,
	// without dropping the listener.
	writeTokens("second-token", "ci")
	if err := srv.ReloadTokens(); err != nil {
		t.Fatal(err)
	}
	man2, files2 := buildFixture(t, "v1.1.0")
	if _, _, err := client.New(ts.URL, "first-token").Publish(context.Background(), man2, files2); err == nil {
		t.Error("the rotated-out token still works")
	}
	if _, ok, err := client.New(ts.URL, "second-token").Publish(context.Background(), man2, files2); err != nil || !ok {
		t.Fatalf("publish with the new token: %v %v", ok, err)
	}

	// A broken file must not take the running credentials down with it.
	if err := os.WriteFile(tokensPath, []byte("tokens: [{subject: x, sha256: short, scopes: [read]}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := srv.ReloadTokens(); err == nil {
		t.Error("an invalid tokens file was accepted")
	}
	man3, files3 := buildFixture(t, "v1.2.0")
	if _, ok, err := client.New(ts.URL, "second-token").Publish(context.Background(), man3, files3); err != nil || !ok {
		t.Errorf("a failed reload broke the live credentials: %v %v", ok, err)
	}
}
