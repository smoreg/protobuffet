package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
