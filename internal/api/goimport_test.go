package api_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/smoreg/protobuffet/internal/client"
)

// getWithHost sends a request pretending to arrive at a hostname, which is how
// it looks in production behind an ingress. It matters here because a Go
// module path cannot contain a port: the registry must be reachable on the
// default port for discovery to work at all.
func getWithHost(t *testing.T, url, host string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestGoImportDiscovery(t *testing.T) {
	ts, _ := newServer(t)
	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := client.New(ts.URL, testToken).Publish(context.Background(), man, files); err != nil || !ok {
		t.Fatalf("publish: %v %v", ok, err)
	}
	// The fixture publishes registry.test/gen/go/acme/catalog, so answer as
	// that host.
	const host = "registry.test"

	status, body := getWithHost(t, ts.URL+"/gen/go/acme/catalog?go-get=1", host)
	if status != http.StatusOK {
		t.Fatalf("discovery status = %d", status)
	}
	wantMeta := `<meta name="go-import" content="` + goModPath + ` mod http://` + host + `/go">`
	if !strings.Contains(body, wantMeta) {
		t.Errorf("body = %q\nwant to contain %q", body, wantMeta)
	}

	// A package below the module root resolves to the module root, which is
	// what the go command asks for when importing a subpackage.
	if _, sub := getWithHost(t, ts.URL+"/gen/go/acme/catalog/catalog/v1?go-get=1", host); !strings.Contains(sub, wantMeta) {
		t.Errorf("subpackage discovery = %q", sub)
	}

	// Unknown module: a clear 404 rather than an empty meta document.
	if status, _ := getWithHost(t, ts.URL+"/gen/go/acme/unknown?go-get=1", host); status != http.StatusNotFound {
		t.Errorf("unknown module status = %d, want 404", status)
	}

	// Without go-get=1 nothing special happens.
	if status, _ := getWithHost(t, ts.URL+"/gen/go/acme/catalog", host); status != http.StatusNotFound {
		t.Errorf("plain GET status = %d, want 404", status)
	}

	// Discovery also works when a configured base URL overrides the request
	// host, which is the case behind a proxy that rewrites Host.
	if status, body := getWithHost(t, ts.URL+"/gen/go/acme/catalog?go-get=1", "wrong.example"); status != http.StatusNotFound {
		t.Errorf("mismatched host: status = %d, body = %q; want 404", status, body)
	}
}
