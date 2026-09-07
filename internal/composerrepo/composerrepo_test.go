package composerrepo_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smoreg/protobuffet/internal/composerrepo"
	"github.com/smoreg/protobuffet/internal/npmrepo"
	"github.com/smoreg/protobuffet/internal/pypirepo"
	"github.com/smoreg/protobuffet/internal/registry"
)

// seed publishes one version carrying a Composer, an npm and a Python
// artifact, which is what the three metadata endpoints render.
func seed(t *testing.T) *registry.Catalog {
	t.Helper()
	store, err := registry.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := registry.NewCatalog(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Publish(registry.PublishRequest{
		Module:    registry.Module{Owner: "acme", Name: "catalog"},
		Version:   "v1.0.0",
		SchemaZip: []byte("schema"),
		Artifacts: []registry.Artifact{
			{Target: registry.TargetComposer, Name: "acme/catalog-proto", Version: "1.0.0", File: "composer.zip",
				Meta: map[string]any{
					"require":  map[string]any{"php": ">=8.1"},
					"autoload": map[string]any{"classmap": []any{"."}},
					"license":  "Apache-2.0",
				}},
			{Target: registry.TargetNPM, Name: "@acme/catalog", Version: "1.0.0", File: "npm.tgz",
				Meta: map[string]any{"dependencies": map[string]any{"@bufbuild/protobuf": "^2.2.0"}, "type": "module"}},
			{Target: registry.TargetPython, Name: "acme-catalog-proto", Version: "1.0.0", File: "python.whl",
				Meta: map[string]any{"requires_python": ">=3.8"}},
		},
		Files: map[string][]byte{
			"composer.zip": []byte("composer archive body"),
			"npm.tgz":      []byte("npm tarball body"),
			"python.whl":   []byte("wheel body"),
		},
		Now: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestComposerMetadataAndDownload(t *testing.T) {
	c := seed(t)
	ts := httptest.NewServer(http.StripPrefix("/composer", composerrepo.New(c, "")))
	defer ts.Close()

	// Composer 2 reads the root first, then one document per package. Serving
	// every version inline would mean downloading the whole registry on each
	// `composer update`.
	var root struct {
		MetadataURL       string   `json:"metadata-url"`
		AvailablePackages []string `json:"available-packages"`
	}
	rootBody := fetch(t, ts.URL+"/composer/packages.json", http.StatusOK)
	if err := json.Unmarshal([]byte(rootBody), &root); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(root.MetadataURL, "/composer/p2/%package%.json") {
		t.Errorf("metadata-url = %q", root.MetadataURL)
	}
	if len(root.AvailablePackages) != 1 || root.AvailablePackages[0] != "acme/catalog-proto" {
		t.Errorf("available-packages = %v", root.AvailablePackages)
	}

	var doc struct {
		Packages map[string][]struct {
			Name     string
			Version  string
			Require  map[string]string
			Autoload map[string]any
			License  []string
			Dist     struct{ URL, Shasum, Reference, Type string }
		}
	}
	body := fetch(t, ts.URL+"/composer/p2/acme/catalog-proto.json", http.StatusOK)
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	versions := doc.Packages["acme/catalog-proto"]
	if len(versions) != 1 {
		t.Fatalf("package document = %s", body)
	}
	v := versions[0]
	if v.Version != "1.0.0" {
		t.Errorf("version = %q", v.Version)
	}
	if v.Require["php"] != ">=8.1" || v.Autoload == nil || len(v.License) != 1 {
		t.Errorf("metadata lost: %+v", v)
	}
	// Composer verifies this digest after downloading; an empty one makes the
	// package uninstallable.
	if v.Dist.Shasum == "" || v.Dist.Reference == "" || v.Dist.Type != "zip" {
		t.Errorf("dist = %+v", v.Dist)
	}
	if got := fetch(t, v.Dist.URL, http.StatusOK); got != "composer archive body" {
		t.Errorf("dist body = %q", got)
	}
	// The URL is absolute and derived from the request when base_url is unset.
	if v.Dist.URL[:len(ts.URL)] != ts.URL {
		t.Errorf("dist url %q is not under %q", v.Dist.URL, ts.URL)
	}
	fetch(t, ts.URL+"/composer/dist/acme/catalog/v9.9.9.zip", http.StatusNotFound)
	fetch(t, ts.URL+"/composer/nonsense", http.StatusNotFound)
	fetch(t, ts.URL+"/composer/p2/acme/unknown.json", http.StatusNotFound)
	// Composer also asks for a ~dev variant; an empty document keeps its log
	// clean.
	if devDoc := fetch(t, ts.URL+"/composer/p2/acme/catalog-proto~dev.json", http.StatusOK); !strings.Contains(devDoc, "acme/catalog-proto") {
		t.Errorf("~dev document = %q", devDoc)
	}
}

func TestNPMPackumentAndTarball(t *testing.T) {
	c := seed(t)
	ts := httptest.NewServer(npmrepo.New(c, ""))
	defer ts.Close()

	var doc struct {
		Name     string
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Type         string
			Dependencies map[string]string
			Dist         struct{ Tarball, Shasum string }
		}
	}
	// Scoped names arrive percent-encoded from npm, and plain from curl.
	for _, path := range []string{"/npm/@acme%2fcatalog", "/npm/@acme/catalog"} {
		if err := json.Unmarshal([]byte(fetch(t, ts.URL+path, http.StatusOK)), &doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if doc.DistTags["latest"] != "1.0.0" {
			t.Errorf("%s: dist-tags = %v", path, doc.DistTags)
		}
	}
	v := doc.Versions["1.0.0"]
	if v.Dependencies["@bufbuild/protobuf"] != "^2.2.0" || v.Type != "module" {
		t.Errorf("version metadata = %+v", v)
	}
	if v.Dist.Shasum == "" {
		t.Error("npm requires a shasum")
	}
	if got := fetch(t, v.Dist.Tarball, http.StatusOK); got != "npm tarball body" {
		t.Errorf("tarball body = %q", got)
	}
	fetch(t, ts.URL+"/npm/@acme%2funknown", http.StatusNotFound)
	fetch(t, ts.URL+"/npm/dist/acme/catalog/v9.9.9.tgz", http.StatusNotFound)
}

func TestPyPISimpleIndex(t *testing.T) {
	c := seed(t)
	ts := httptest.NewServer(pypirepo.New(c, ""))
	defer ts.Close()

	root := fetch(t, ts.URL+"/pypi/simple", http.StatusOK)
	if !contains(root, "/pypi/simple/acme-catalog-proto/") {
		t.Errorf("index root = %q", root)
	}
	project := fetch(t, ts.URL+"/pypi/simple/acme-catalog-proto/", http.StatusOK)
	for _, want := range []string{
		"acme_catalog_proto-1.0.0-py3-none-any.whl",
		"#sha256=",
		`data-requires-python="&gt;=3.8"`,
	} {
		if !contains(project, want) {
			t.Errorf("project page missing %q:\n%s", want, project)
		}
	}
	// pip normalizes names before asking, but a human might not.
	if page := fetch(t, ts.URL+"/pypi/simple/Acme.Catalog_Proto/", http.StatusOK); !contains(page, ".whl") {
		t.Error("unnormalized name did not resolve")
	}
	fetch(t, ts.URL+"/pypi/simple/unknown/", http.StatusNotFound)
	fetch(t, ts.URL+"/pypi/files/acme/catalog/v1.0.0/wrong.txt", http.StatusNotFound)
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func fetch(t *testing.T, url string, wantStatus int) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s: status %d, want %d: %s", url, resp.StatusCode, wantStatus, body)
	}
	return string(body)
}
