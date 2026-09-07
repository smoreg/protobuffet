package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/smoreg/protobuffet/internal/client"
)

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

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-Id", "from-the-proxy")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// A proxy that already assigned an id must see it preserved, otherwise
	// the two logs cannot be joined.
	if got := resp.Header.Get("X-Request-Id"); got != "from-the-proxy" {
		t.Errorf("request id = %q", got)
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
