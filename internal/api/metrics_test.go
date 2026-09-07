package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/smoreg/protobuffet/internal/api"
	"github.com/smoreg/protobuffet/internal/client"
	"github.com/smoreg/protobuffet/internal/config"
	"github.com/smoreg/protobuffet/internal/registry"
)

// trustedProxyServer runs a registry that believes forwarding headers.
func trustedProxyServer(t *testing.T) *httptest.Server {
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
	cfg.TrustedProxy = true
	cfg.Tokens = []config.Token{{Subject: "t", SHA256: config.HashToken(testToken), Scopes: []string{"write"}}}

	srv, err := api.New(cfg, catalog, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestMetricsEndpoint(t *testing.T) {
	ts, _ := newServer(t)
	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := client.New(ts.URL, testToken).Publish(context.Background(), man, files); err != nil || !ok {
		t.Fatalf("publish: %v %v", ok, err)
	}
	get(t, ts.URL+"/go/"+goModPath+"/@v/list")

	body := get(t, ts.URL+"/metrics")
	for _, want := range []string{
		`protobuffet_http_requests_total{route="/go",status="200"}`,
		"protobuffet_http_request_duration_seconds_bucket",
		`protobuffet_publishes_total{result="ok"} 1`,
		"protobuffet_modules 1",
		"protobuffet_versions 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
	// Module names must not become label values, or a large registry produces
	// a time series per module.
	if strings.Contains(body, "acme/catalog") {
		t.Error("metrics leak module names into labels")
	}
}

func TestRequestIDIsEchoedAndGenerated(t *testing.T) {
	ts, _ := newServer(t)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("no request id generated")
	}

	// Without a declared proxy in front, an inbound id is ignored: a client
	// could otherwise pick its own and make two unrelated requests look like
	// one in the audit log.
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-Id", "forged-by-the-client")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("X-Request-Id"); got == "forged-by-the-client" {
		t.Error("an untrusted request id was echoed back")
	}
}

func TestTrustedProxyHeadersAreHonouredWhenDeclared(t *testing.T) {
	ts := trustedProxyServer(t)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-Id", "from-the-proxy")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// With a proxy declared, its id is preserved so the two logs join up.
	if got := resp.Header.Get("X-Request-Id"); got != "from-the-proxy" {
		t.Errorf("request id = %q, want the proxy's", got)
	}

	// Control characters must not reach a log line.
	req.Header.Set("X-Request-Id", "bad\nid")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		// Go's client refuses to send an invalid header, which is also fine.
		return
	}
	resp.Body.Close()
	if strings.Contains(resp.Header.Get("X-Request-Id"), "\n") {
		t.Error("a newline survived into the request id")
	}
}

func TestMetricsCountBreakingRefusals(t *testing.T) {
	ts := gateServer(t, bufFindsBreakage, "required", "strict")
	c := client.New(ts.URL, testToken)
	ctx := context.Background()

	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := c.Publish(ctx, man, files); err != nil || !ok {
		t.Fatalf("first publish: %v %v", ok, err)
	}
	man2, files2 := buildFixture(t, "v1.1.0")
	if _, ok, _ := c.Publish(ctx, man2, files2); ok {
		t.Fatal("breaking change published")
	}
	body := get(t, ts.URL+"/metrics")
	if !strings.Contains(body, `protobuffet_publishes_total{result="breaking"} 1`) {
		t.Errorf("breaking refusals not counted:\n%s", body)
	}
}
