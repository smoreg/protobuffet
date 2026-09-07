package npmrepo_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/smoreg/protobuffet/internal/npmrepo"
	"github.com/smoreg/protobuffet/internal/registry"
)

func serve(t *testing.T, versions ...string) *httptest.Server {
	t.Helper()
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := registry.NewCatalog(store)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range versions {
		if _, err := c.Publish(registry.PublishRequest{
			Module:    registry.Module{Owner: "acme", Name: "catalog"},
			Version:   v,
			SchemaZip: []byte(v),
			Artifacts: []registry.Artifact{{
				Target: registry.TargetNPM, Name: "@acme/catalog",
				Version: registry.EcosystemVersion(v), File: "npm.tgz",
				Meta: map[string]any{"dependencies": map[string]any{"@bufbuild/protobuf": "^2.2.0"}},
			}},
			Files: map[string][]byte{"npm.tgz": []byte(v)},
			Now:   time.Date(2026, 9, 7, 12, i, 0, 0, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(npmrepo.New(c, ""))
	t.Cleanup(ts.Close)
	return ts
}

type packument struct {
	Name     string
	DistTags map[string]string `json:"dist-tags"`
	Versions map[string]struct {
		Dependencies map[string]string
		Dist         struct{ Tarball, Shasum string }
	}
}

func fetch(t *testing.T, url string, want int) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("GET %s: status %d, want %d: %s", url, resp.StatusCode, want, body)
	}
	return string(body)
}

func read(t *testing.T, ts *httptest.Server) packument {
	t.Helper()
	var doc packument
	if err := json.Unmarshal([]byte(fetch(t, ts.URL+"/npm/@acme%2fcatalog", http.StatusOK)), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestLatestPrefersAStableRelease covers the tag `npm install <pkg>` resolves.
// Pointing it at a prerelease makes an RC the default for every consumer, and
// nobody asked for that by publishing one.
func TestLatestPrefersAStableRelease(t *testing.T) {
	ts := serve(t, "v1.0.0", "v2.0.0-rc.1")
	doc := read(t, ts)

	if doc.DistTags["latest"] != "1.0.0" {
		t.Errorf("dist-tags.latest = %q, want 1.0.0", doc.DistTags["latest"])
	}
	// The prerelease is still installable, under its own tag.
	if doc.DistTags["next"] != "2.0.0-rc.1" {
		t.Errorf("dist-tags.next = %q", doc.DistTags["next"])
	}
	if len(doc.Versions) != 2 {
		t.Errorf("versions = %v", doc.Versions)
	}
}

func TestLatestFallsBackToAPrereleaseWhenThatIsAll(t *testing.T) {
	ts := serve(t, "v1.0.0-rc.1", "v1.0.0-rc.2")
	if got := read(t, ts).DistTags["latest"]; got != "1.0.0-rc.2" {
		t.Errorf("dist-tags.latest = %q, want 1.0.0-rc.2", got)
	}
}

func TestLatestIsHighestNotNewest(t *testing.T) {
	// A patch on an old line published after a new minor must not become
	// latest just because it arrived last.
	ts := serve(t, "v1.5.0", "v1.4.1")
	if got := read(t, ts).DistTags["latest"]; got != "1.5.0" {
		t.Errorf("dist-tags.latest = %q, want 1.5.0", got)
	}
}

func TestPackumentContentAndDownload(t *testing.T) {
	ts := serve(t, "v1.0.0")
	doc := read(t, ts)

	v := doc.Versions["1.0.0"]
	if v.Dependencies["@bufbuild/protobuf"] != "^2.2.0" {
		t.Errorf("dependencies = %v", v.Dependencies)
	}
	// npm verifies this after download; an empty shasum fails the install.
	if v.Dist.Shasum == "" {
		t.Error("no shasum")
	}
	if body := fetch(t, v.Dist.Tarball, http.StatusOK); body != "v1.0.0" {
		t.Errorf("tarball body = %q", body)
	}
}

func TestScopedNameForms(t *testing.T) {
	ts := serve(t, "v1.0.0")
	// npm sends the scope percent-encoded; a human or a proxy may not.
	for _, path := range []string{"/npm/@acme%2fcatalog", "/npm/@acme%2Fcatalog", "/npm/@acme/catalog"} {
		fetch(t, ts.URL+path, http.StatusOK)
	}
	fetch(t, ts.URL+"/npm/@acme%2funknown", http.StatusNotFound)
	fetch(t, ts.URL+"/npm/@a/b/c", http.StatusNotFound)
	fetch(t, ts.URL+"/npm/dist/acme/catalog/v9.9.9.tgz", http.StatusNotFound)
	fetch(t, ts.URL+"/npm/dist/../../etc/passwd.tgz", http.StatusNotFound)

	resp, err := http.Post(ts.URL+"/npm/@acme%2fcatalog", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d", resp.StatusCode)
	}
}
