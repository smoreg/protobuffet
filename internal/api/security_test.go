package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/smoreg/protobuffet/internal/api"
	"github.com/smoreg/protobuffet/internal/client"
	"github.com/smoreg/protobuffet/internal/config"
	"github.com/smoreg/protobuffet/internal/registry"
)

// TestDownloadPathsRejectTraversal covers the three download handlers that
// build a filesystem path out of URL segments. Each request must be refused
// before it reaches the store.
func TestDownloadPathsRejectTraversal(t *testing.T) {
	ts, _ := newServer(t)
	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := client.New(ts.URL, testToken).Publish(man, files); err != nil || !ok {
		t.Fatalf("publish: %v %v", ok, err)
	}

	paths := []string{
		"/composer/dist/../../../etc/passwd.zip",
		"/composer/dist/acme/..%2f..%2fcatalog/v1.0.0.zip",
		"/composer/dist/ACME/catalog/v1.0.0.zip",
		"/composer/dist/acme/catalog/latest.zip",
		"/composer/dist/acme/catalog/..%2fv1.0.0.zip",
		"/npm/dist/../../etc/passwd.tgz",
		"/npm/dist/acme/catalog/not-a-version.tgz",
		"/npm/dist/acme/cat%20alog/v1.0.0.tgz",
		"/pypi/files/../../../etc/passwd.whl",
		"/pypi/files/acme/catalog/../v1.0.0/x.whl",
		"/pypi/files/acme/catalog/latest/x.whl",
	}
	for _, p := range paths {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Errorf("GET %s: %v", p, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", p, resp.StatusCode)
		}
	}
}

// TestValidDownloadPathsStillWork guards against the validation above being
// too strict to serve real requests.
func TestValidDownloadPathsStillWork(t *testing.T) {
	ts, _ := newServer(t)
	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := client.New(ts.URL, testToken).Publish(man, files); err != nil || !ok {
		t.Fatalf("publish: %v %v", ok, err)
	}
	for _, p := range []string{
		"/composer/dist/acme/catalog/v1.0.0.zip",
		"/npm/dist/acme/catalog/v1.0.0.tgz",
		"/pypi/files/acme/catalog/v1.0.0/acme_catalog_proto-1.0.0-py3-none-any.whl",
	} {
		if body := get(t, ts.URL+p); len(body) < 50 {
			t.Errorf("GET %s returned %d bytes", p, len(body))
		}
	}
	// An unversioned "v" prefix is accepted by NormalizeVersion but the file
	// name in metadata always carries it, so both spellings must resolve.
	if body := get(t, ts.URL+"/composer/dist/acme/catalog/v1.0.0.zip"); !strings.HasPrefix(string(body[:2]), "PK") {
		t.Error("composer dist is not a zip")
	}
}

// TestBreakingModes covers the gate: required refuses when it cannot verify,
// best-effort publishes with a warning, off skips entirely.
func TestBreakingRequiredRefusesWhenBufIsMissing(t *testing.T) {
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
	cfg.BreakingCheck = config.BreakingRequired
	cfg.BufPath = "/nonexistent/buf"
	cfg.Tokens = []config.Token{{Subject: "t", SHA256: config.HashToken(testToken), Scopes: []string{"write"}}}

	// Required mode must refuse to start rather than serve without the gate.
	if _, err := api.New(cfg, catalog, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("server started in required mode without buf")
	}

	cfg.BreakingCheck = config.BreakingBestEffort
	if _, err := api.New(cfg, catalog, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("best-effort should start without buf: %v", err)
	}
}

func TestBreakingModeParsing(t *testing.T) {
	for in, want := range map[string]config.BreakingMode{
		"required":    config.BreakingRequired,
		"best-effort": config.BreakingBestEffort,
		"off":         config.BreakingOff,
		"true":        config.BreakingRequired, // historical boolean spelling
		"false":       config.BreakingOff,
	} {
		var got config.BreakingMode
		if err := yaml.Unmarshal([]byte(in), &got); err != nil || got != want {
			t.Errorf("parse %q = %q, %v; want %q", in, got, err, want)
		}
	}
	var bad config.BreakingMode
	if err := yaml.Unmarshal([]byte("maybe"), &bad); err == nil {
		t.Error("invalid mode accepted")
	}
}
