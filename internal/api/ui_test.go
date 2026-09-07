package api_test

import (
	"context"
	"strings"
	"testing"

	"github.com/smoreg/protobuffet/internal/client"
)

func TestUIPages(t *testing.T) {
	ts, _ := newServer(t)

	// Empty registry: the page must say what to do, not render a blank table.
	if body := get(t, ts.URL+"/"); !strings.Contains(body, "buffet publish") {
		t.Errorf("empty index = %q", body)
	}

	man, files := buildFixture(t, "v1.0.0")
	if _, ok, err := client.New(ts.URL, testToken).Publish(context.Background(), man, files); err != nil || !ok {
		t.Fatalf("publish: %v %v", ok, err)
	}

	index := get(t, ts.URL+"/")
	for _, want := range []string{"acme/catalog", "v1.0.0", `href="/m/acme/catalog"`} {
		if !strings.Contains(index, want) {
			t.Errorf("index missing %q", want)
		}
	}

	page := get(t, ts.URL+"/m/acme/catalog")
	// The module page is where someone copies the install command from, so
	// every published ecosystem has to appear with a working invocation.
	for _, want := range []string{
		"v1.0.0",
		"main", // the label
		"GOPRIVATE",
		"go get " + goModPath + "@v1.0.0",
		"composer require acme/catalog-proto:1.0.0",
		"npm install @acme/catalog@1.0.0",
		"pip install --index-url",
		"schema.zip",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("module page missing %q", want)
		}
	}
	// GOSUMDB=off disables checksum verification for the whole build; the UI
	// must never suggest it.
	if strings.Contains(page, "GOSUMDB=off") {
		t.Error("module page suggests GOSUMDB=off")
	}

	if status, _ := getWithHost(t, ts.URL+"/m/acme/missing", "registry.test"); status != 404 {
		t.Errorf("unknown module page status = %d", status)
	}
}
