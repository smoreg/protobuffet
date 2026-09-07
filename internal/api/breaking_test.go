package api_test

import (
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
	"github.com/smoreg/protobuffet/internal/registry"
)

// gateServer runs a registry whose compatibility gate is driven by a fake buf,
// so the verdict is controlled by the test rather than by a real schema diff.
func gateServer(t *testing.T, bufScript string, mode config.BreakingMode, policy config.BreakingPolicy) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	bufPath := filepath.Join(dir, "buf")
	if err := os.WriteFile(bufPath, []byte("#!/bin/sh\n"+bufScript), 0o755); err != nil {
		t.Fatal(err)
	}
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
	cfg.BreakingCheck = mode
	cfg.BreakingPolicy = policy
	cfg.BufPath = bufPath
	cfg.Tokens = []config.Token{
		{Subject: "ci", SHA256: config.HashToken(testToken), Scopes: []string{"write"}},
		{Subject: "boss", SHA256: config.HashToken("admin-token"), Scopes: []string{"admin"}},
	}
	srv, err := api.New(cfg, catalog, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

const bufFindsBreakage = `echo "catalog.proto:20:10:Field \"2\" on message \"Product\" changed name from \"sku\" to \"stock\"."; exit 100` + "\n"

func TestGateRefusesBreakingChange(t *testing.T) {
	ts := gateServer(t, bufFindsBreakage, config.BreakingRequired, config.PolicyStrict)
	c := client.New(ts.URL, testToken)
	ctx := context.Background()

	// The first version has nothing to compare against.
	man, files := buildFixture(t, "v1.0.0")
	resp, ok, err := c.Publish(ctx, man, files)
	if err != nil || !ok {
		t.Fatalf("first publish: %v %v", ok, err)
	}
	if resp.Breaking == nil || !strings.Contains(resp.Breaking.Skipped, "first version") {
		t.Errorf("first version report = %+v", resp.Breaking)
	}

	// The second one is refused, and the findings come back so the developer
	// knows what to fix.
	man2, files2 := buildFixture(t, "v1.1.0")
	resp, ok, err = c.Publish(ctx, man2, files2)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a breaking change was published")
	}
	if resp.Breaking == nil || !strings.Contains(resp.Breaking.Findings, "changed name") {
		t.Fatalf("findings = %+v", resp.Breaking)
	}
	if resp.Breaking.Against != "v1.0.0" {
		t.Errorf("compared against %q", resp.Breaking.Against)
	}
	// The refused version must not be half-stored.
	if _, err := c.Module(ctx, "acme/catalog"); err != nil {
		t.Fatal(err)
	}
	idx, _ := c.Module(ctx, "acme/catalog")
	if len(idx.Versions) != 1 {
		t.Errorf("versions after a refused publish = %d, want 1", len(idx.Versions))
	}
}

func TestAdminOverridePublishesAndIsRecorded(t *testing.T) {
	ts := gateServer(t, bufFindsBreakage, config.BreakingRequired, config.PolicyStrict)
	ctx := context.Background()

	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := client.New(ts.URL, testToken).Publish(ctx, man, files); err != nil || !ok {
		t.Fatalf("first publish: %v %v", ok, err)
	}

	man2, files2 := buildFixture(t, "v1.1.0")
	man2.AllowBreaking = true
	resp, ok, err := client.New(ts.URL, "admin-token").Publish(ctx, man2, files2)
	if err != nil || !ok {
		t.Fatalf("override publish: %v %v", ok, err)
	}
	// The override is recorded rather than hidden: someone reading the
	// response later has to see that the gate was bypassed.
	if resp.Breaking == nil || resp.Breaking.Overridden != "admin override" {
		t.Errorf("report = %+v", resp.Breaking)
	}
	if resp.Breaking.Findings == "" {
		t.Error("findings were dropped from an overridden publish")
	}
}

func TestAllowMajorPolicySkipsTheGateOnAMajorBump(t *testing.T) {
	ts := gateServer(t, bufFindsBreakage, config.BreakingRequired, config.PolicyAllowMajor)
	c := client.New(ts.URL, testToken)
	ctx := context.Background()

	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := c.Publish(ctx, man, files); err != nil || !ok {
		t.Fatalf("first publish: %v %v", ok, err)
	}

	// A minor bump is still checked, and still refused.
	man2, files2 := buildFixture(t, "v1.1.0")
	if _, ok, _ := c.Publish(ctx, man2, files2); ok {
		t.Error("minor version with breaking changes was published under allow-major")
	}

	// A major bump is where breaking changes are meant to live. Go puts the
	// major version in the import path from v2 on, so the module path changes
	// too.
	man3, files3 := buildFixtureAt(t, "v2.0.0", goModPath+"/v2")
	resp, ok, err := c.Publish(ctx, man3, files3)
	if err != nil || !ok {
		t.Fatalf("major publish: %v %v", ok, err)
	}
	if resp.Breaking == nil || resp.Breaking.Overridden != "policy allow-major" {
		t.Errorf("report = %+v", resp.Breaking)
	}
}

func TestRequiredModeReturns503WhenTheCheckCannotRun(t *testing.T) {
	// buf exits 1 without a verdict: the tool is broken, not the schema.
	ts := gateServer(t, `echo "Failure: cannot read input" >&2; exit 1`+"\n",
		config.BreakingRequired, config.PolicyStrict)
	c := client.New(ts.URL, testToken)
	ctx := context.Background()

	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := c.Publish(ctx, man, files); err != nil || !ok {
		t.Fatalf("first publish: %v %v", ok, err)
	}
	man2, files2 := buildFixture(t, "v1.1.0")
	_, _, err := c.Publish(ctx, man2, files2)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want a 503", err)
	}
}

func TestBestEffortPublishesWhenTheCheckCannotRun(t *testing.T) {
	ts := gateServer(t, `echo "Failure: cannot read input" >&2; exit 1`+"\n",
		config.BreakingBestEffort, config.PolicyStrict)
	c := client.New(ts.URL, testToken)
	ctx := context.Background()

	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := c.Publish(ctx, man, files); err != nil || !ok {
		t.Fatalf("first publish: %v %v", ok, err)
	}
	man2, files2 := buildFixture(t, "v1.1.0")
	resp, ok, err := c.Publish(ctx, man2, files2)
	if err != nil || !ok {
		t.Fatalf("best-effort publish: %v %v", ok, err)
	}
	if resp.Breaking == nil || !strings.Contains(resp.Breaking.Skipped, "could not run") {
		t.Errorf("report = %+v", resp.Breaking)
	}
}
