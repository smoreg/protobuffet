package registry

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testArtifact(file string) Artifact {
	return Artifact{Target: TargetGo, Name: "example.com/gen/go/acme/catalog", Version: "v1.0.0", File: file}
}

func publish(t *testing.T, s *Store, version string, schema []byte, labels ...string) Version {
	t.Helper()
	v, err := s.Publish(PublishRequest{
		Module:    Module{Owner: "acme", Name: "catalog"},
		Version:   version,
		SchemaZip: schema,
		Artifacts: []Artifact{testArtifact("go.zip")},
		Files:     map[string][]byte{"go.zip": []byte("zip-" + version)},
		Labels:    labels,
		Now:       time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("publish %s: %v", version, err)
	}
	return v
}

func TestPublishIsImmutableButIdempotent(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mod := Module{Owner: "acme", Name: "catalog"}

	first := publish(t, s, "v1.0.0", []byte("schema-a"))
	if first.Version != "v1.0.0" {
		t.Fatalf("version = %q", first.Version)
	}

	// Same content: a CI retry must succeed rather than fail the pipeline.
	again := publish(t, s, "v1.0.0", []byte("schema-a"))
	if again.CreatedAt != first.CreatedAt {
		t.Errorf("re-publishing identical content created a new version")
	}

	// Different content under the same version is a rewrite of history.
	_, err = s.Publish(PublishRequest{
		Module:    mod,
		Version:   "v1.0.0",
		SchemaZip: []byte("schema-b"),
		Artifacts: []Artifact{testArtifact("go.zip")},
		Files:     map[string][]byte{"go.zip": []byte("zip")},
	})
	if !errors.Is(err, ErrVersionExists) {
		t.Fatalf("want ErrVersionExists, got %v", err)
	}
}

func TestVersionsAreSortedBySemver(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	publish(t, s, "v1.10.0", []byte("c"))
	publish(t, s, "v1.2.0", []byte("a"))
	publish(t, s, "v1.9.0", []byte("b"))

	idx, err := s.Index(Module{Owner: "acme", Name: "catalog"})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{idx.Versions[0].Version, idx.Versions[1].Version, idx.Versions[2].Version}
	want := []string{"v1.2.0", "v1.9.0", "v1.10.0"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	latest, _ := idx.Latest()
	if latest.Version != "v1.10.0" {
		t.Errorf("latest = %q, want v1.10.0", latest.Version)
	}
}

func TestResolveLabelsAndLatest(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	mod := Module{Owner: "acme", Name: "catalog"}
	publish(t, s, "v1.0.0", []byte("a"), "main")
	publish(t, s, "v1.1.0", []byte("b"))

	if v, err := s.Resolve(mod, "main"); err != nil || v.Version != "v1.0.0" {
		t.Fatalf("label main = %v %v, want v1.0.0", v.Version, err)
	}
	if v, err := s.Resolve(mod, "latest"); err != nil || v.Version != "v1.1.0" {
		t.Fatalf("latest = %v %v, want v1.1.0", v.Version, err)
	}
	if v, err := s.Resolve(mod, "1.0.0"); err != nil || v.Version != "v1.0.0" {
		t.Fatalf("unprefixed version = %v %v", v.Version, err)
	}
	if _, err := s.Resolve(mod, "v9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing version err = %v, want ErrNotFound", err)
	}
}

func TestPublishRejectsDigestMismatch(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	a := testArtifact("go.zip")
	a.SHA256 = SHA256Hex([]byte("something else"))
	_, err := s.Publish(PublishRequest{
		Module:    Module{Owner: "acme", Name: "catalog"},
		Version:   "v1.0.0",
		SchemaZip: []byte("schema"),
		Artifacts: []Artifact{a},
		Files:     map[string][]byte{"go.zip": []byte("real body")},
	})
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
}

func TestPublishComputesDigestsItself(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	body := []byte("artifact body")
	v, err := s.Publish(PublishRequest{
		Module:    Module{Owner: "acme", Name: "catalog"},
		Version:   "v1.0.0",
		SchemaZip: []byte("schema"),
		Artifacts: []Artifact{{Target: TargetComposer, Name: "acme/catalog-proto", Version: "1.0.0", File: "composer.zip"}},
		Files:     map[string][]byte{"composer.zip": body},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := v.Artifacts[TargetComposer]
	if a.SHA256 != SHA256Hex(body) {
		t.Errorf("sha256 = %q", a.SHA256)
	}
	// Composer and npm read this field; an empty or wrong value makes the
	// published package uninstallable forever.
	if a.SHA1 != sha1hex(body) || a.SHA1 == "" {
		t.Errorf("sha1 = %q, want %q", a.SHA1, sha1hex(body))
	}
	if a.Size != int64(len(body)) {
		t.Errorf("size = %d", a.Size)
	}
}

func TestPublishRejectsWrongSHA1(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	_, err := s.Publish(PublishRequest{
		Module:    Module{Owner: "acme", Name: "catalog"},
		Version:   "v1.0.0",
		SchemaZip: []byte("schema"),
		Artifacts: []Artifact{{
			Target: TargetNPM, Name: "@acme/catalog", Version: "1.0.0", File: "npm.tgz",
			SHA1: "0000000000000000000000000000000000000000",
		}},
		Files: map[string][]byte{"npm.tgz": []byte("real body")},
	})
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
}

func TestOpenRejectsPathTraversal(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	publish(t, s, "v1.0.0", []byte("a"))
	if _, _, err := s.Open(Module{Owner: "acme", Name: "catalog"}, "v1.0.0", "../index.json"); err == nil {
		t.Fatal("path traversal was allowed")
	}
}

func TestFindByTarget(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	publish(t, s, "v1.0.0", []byte("a"))
	mod, idx, err := s.FindByTarget(TargetGo, "example.com/gen/go/acme/catalog")
	if err != nil {
		t.Fatal(err)
	}
	if mod.String() != "acme/catalog" || len(idx.Versions) != 1 {
		t.Fatalf("found %s with %d versions", mod, len(idx.Versions))
	}
	if _, _, err := s.FindByTarget(TargetGo, "example.com/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestParseModuleRejectsBadNames(t *testing.T) {
	for _, bad := range []string{"", "acme", "acme/", "/catalog", "ACME/catalog", "acme/../etc", "acme/cat alog"} {
		if _, err := ParseModule(bad); err == nil {
			t.Errorf("ParseModule(%q) accepted an invalid name", bad)
		}
	}
	if m, err := ParseModule("acme/catalog-v2"); err != nil || m.Name != "catalog-v2" {
		t.Errorf("ParseModule rejected a valid name: %v", err)
	}
}

func TestPredecessorPicksTheVersionBelow(t *testing.T) {
	idx := &Index{Versions: []Version{
		{Version: "v1.0.0"}, {Version: "v1.5.0"}, {Version: "v2.0.0"},
	}}
	cases := map[string]string{
		"v1.6.0": "v1.5.0", // newest below, not the highest overall
		"v1.0.1": "v1.0.0", // patch on an old line
		"v3.0.0": "v2.0.0",
		"v0.9.0": "v1.0.0", // older than everything: fall back to the lowest
	}
	for version, want := range cases {
		got, ok := idx.Predecessor(version)
		if !ok || got.Version != want {
			t.Errorf("Predecessor(%s) = %v (ok=%v), want %s", version, got.Version, ok, want)
		}
	}
	if _, ok := (&Index{}).Predecessor("v1.0.0"); ok {
		t.Error("empty index reported a predecessor")
	}
}

func TestRepublishRejectsDifferentArtifacts(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	mod := Module{Owner: "acme", Name: "catalog"}
	schema := []byte("schema")
	first := []byte("generated code v1")

	pub := func(body []byte, artifacts ...Artifact) error {
		files := map[string][]byte{}
		for _, a := range artifacts {
			files[a.File] = body
		}
		_, err := s.Publish(PublishRequest{
			Module: mod, Version: "v1.0.0", SchemaZip: schema,
			Artifacts: artifacts, Files: files,
		})
		return err
	}
	art := func() Artifact {
		return Artifact{Target: TargetGo, Name: "registry.test/gen/go/catalog", Version: "v1.0.0", File: "go.zip"}
	}

	if err := pub(first, art()); err != nil {
		t.Fatal(err)
	}
	// Same everything: a retried CI job must not fail.
	if err := pub(first, art()); err != nil {
		t.Fatalf("identical re-publish rejected: %v", err)
	}
	// Same schema, different generated code: silently keeping the old code is
	// how a release goes missing.
	err := pub([]byte("generated code v2"), art())
	if !errors.Is(err, ErrVersionExists) || !strings.Contains(err.Error(), "content differs") {
		t.Fatalf("err = %v, want ErrVersionExists with a reason", err)
	}
	// Adding a target to an existing version is also a different release.
	err = pub(first, art(), Artifact{Target: TargetNPM, Name: "@acme/catalog", Version: "1.0.0", File: "npm.tgz"})
	if !errors.Is(err, ErrVersionExists) {
		t.Fatalf("err = %v, want ErrVersionExists", err)
	}
}

func TestCleanOrphans(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	mod := Module{Owner: "acme", Name: "catalog"}
	publish(t, s, "v1.0.0", []byte("a"))

	orphan := filepath.Join(s.Root(), "modules", "acme", "catalog", "versions", "v9.9.9")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	removed, err := s.CleanOrphans()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || !strings.HasSuffix(removed[0], "v9.9.9") {
		t.Fatalf("removed = %v", removed)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("orphan survived")
	}
	// The real version must be untouched.
	if _, err := s.Resolve(mod, "v1.0.0"); err != nil {
		t.Errorf("published version removed: %v", err)
	}
}
