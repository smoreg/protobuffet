package api_test

import (
	"context"
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

// rebuildOtherArtifacts repacks the non-Go artifacts under distinct package
// names, so the test exercises module scope rather than name ownership.
func rebuildOtherArtifacts(t *testing.T, man *api.PublishManifest, files map[string][]byte) {
	t.Helper()
	// The server recomputes digests, but a stale claim from the original
	// fixture would be checked against the new bytes and refused.
	for i := range man.Artifacts {
		man.Artifacts[i].SHA256 = ""
	}
	phpDir := t.TempDir()
	writeFile(t, filepath.Join(phpDir, "Catalog.php"), "<?php\n")
	writeFile(t, filepath.Join(phpDir, "composer.json"), `{"name": "other/module-proto"}`)
	zipBody, err := pack.Zip(phpDir, "other-module-proto-1.0.0", pack.SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	files["composer.zip"] = zipBody

	tsDir := t.TempDir()
	writeFile(t, filepath.Join(tsDir, "index.js"), "export {}\n")
	writeFile(t, filepath.Join(tsDir, "package.json"), `{"name": "@other/module", "version": "1.0.0"}`)
	tgz, err := pack.TarGz(tsDir, "package", pack.SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	files["npm.tgz"] = tgz

	pyDir := t.TempDir()
	writeFile(t, filepath.Join(pyDir, "catalog/v1/catalog_pb2.py"), "# generated\n")
	wheel, err := pack.Wheel(pyDir, pack.WheelMeta{
		Name: "other-module-proto", Version: "1.0.0", RequiresPython: ">=3.8",
	}, pack.SkipHidden)
	if err != nil {
		t.Fatal(err)
	}
	files["python.whl"] = wheel
}

// TestModuleScopedTokenCannotReachOtherModules covers the blast radius of a
// leaked credential: without module scopes, one team's token can publish
// under another team's name and take its package names.
func TestModuleScopedTokenCannotReachOtherModules(t *testing.T) {
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
	cfg.Tokens = []config.Token{
		{Subject: "acme-ci", SHA256: config.HashToken("acme-token"), Scopes: []string{"write"}, Modules: []string{"acme/*"}},
		{Subject: "boss", SHA256: config.HashToken("admin-token"), Scopes: []string{"admin"}},
	}
	srv, err := api.New(cfg, catalog, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	scoped := client.New(ts.URL, "acme-token")
	ctx := context.Background()

	// Inside the scope: allowed.
	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := scoped.Publish(ctx, man, files); err != nil || !ok {
		t.Fatalf("publish inside scope: %v %v", ok, err)
	}

	// Outside it: refused, and the message names the module so the operator
	// does not have to guess.
	other, otherFiles := buildFixtureAt(t, "v1.0.0", "registry.test/gen/go/other/module")
	other.Module = "other/module"
	other.Artifacts[1].Name = "other/module-proto"
	other.Artifacts[2].Name = "@other/module"
	other.Artifacts[3].Name = "other-module-proto"
	rebuildOtherArtifacts(t, &other, otherFiles)
	_, _, err = scoped.Publish(ctx, other, otherFiles)
	if err == nil {
		t.Fatal("a scoped token published outside its modules")
	}
	if !strings.Contains(err.Error(), "other/module") || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want a 403 naming the module", err)
	}

	// Labels and yanks are writes too.
	if err := scoped.SetLabel(ctx, "acme/catalog", "stable", "v1.0.0"); err != nil {
		t.Errorf("label inside scope: %v", err)
	}
	admin := client.New(ts.URL, "admin-token")
	if _, ok, err := admin.Publish(ctx, other, otherFiles); err != nil || !ok {
		t.Fatalf("admin publish: %v %v", ok, err)
	}
	if err := scoped.SetLabel(ctx, "other/module", "stable", "v1.0.0"); err == nil {
		t.Error("a scoped token labelled a module outside its scope")
	}
	if err := scoped.SetYanked(ctx, "other/module", "v1.0.0", true, ""); err == nil {
		t.Error("a scoped token yanked a version outside its scope")
	}
}

// TestScopeIsNotStickyAcrossRequests guards a bug that only showed on the
// second call: the middleware reassigned its captured scope parameter, so one
// request permanently changed how the route behaved for everyone after it.
func TestScopeIsNotStickyAcrossRequests(t *testing.T) {
	ts, _ := newServer(t) // public_read is on, which is what exposed it
	for i := range 3 {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/whoami", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("call %d: status %d, want 200", i+1, resp.StatusCode)
		}
	}
	// And it still refuses an anonymous caller, public reads or not.
	resp, err := http.Get(ts.URL + "/api/v1/whoami")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous whoami: status %d, want 401", resp.StatusCode)
	}
}
