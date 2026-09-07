package registry

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func publishTo(t *testing.T, c *Catalog, mod string, version string, artifacts ...Artifact) (Version, error) {
	t.Helper()
	m, err := ParseModule(mod)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, a := range artifacts {
		files[a.File] = []byte(mod + version + string(a.Target))
	}
	return c.Publish(PublishRequest{
		Module:    m,
		Version:   version,
		SchemaZip: []byte(mod + version),
		Artifacts: artifacts,
		Files:     files,
		Now:       time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC),
	})
}

func goArtifact(name, version string) Artifact {
	return Artifact{Target: TargetGo, Name: name, Version: version, File: "go.zip"}
}

func TestCatalogRefusesPackageNameOwnedByAnotherModule(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	c, err := NewCatalog(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publishTo(t, c, "acme/catalog", "v1.0.0", goArtifact("registry.test/gen/go/catalog", "v1.0.0")); err != nil {
		t.Fatal(err)
	}
	// A second module claiming the same import path would make resolution
	// depend on directory iteration order.
	_, err = publishTo(t, c, "acme/catalog-v2", "v1.0.0", goArtifact("registry.test/gen/go/catalog", "v1.0.0"))
	if !errors.Is(err, ErrNameTaken) {
		t.Fatalf("err = %v, want ErrNameTaken", err)
	}
	// The rejected publish must not have left anything behind.
	if _, err := store.Index(Module{Owner: "acme", Name: "catalog-v2"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("rejected module was persisted: %v", err)
	}
	// The owner can keep publishing its own name.
	if _, err := publishTo(t, c, "acme/catalog", "v1.1.0", goArtifact("registry.test/gen/go/catalog", "v1.1.0")); err != nil {
		t.Errorf("owner rejected: %v", err)
	}
}

func TestCatalogStartupDetectsConflicts(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	// Two modules with the same npm name, written straight to the store, as a
	// registry created before ownership checks existed would look.
	for _, mod := range []string{"acme/one", "acme/two"} {
		m, _ := ParseModule(mod)
		if _, err := store.Publish(PublishRequest{
			Module:    m,
			Version:   "v1.0.0",
			SchemaZip: []byte(mod),
			Artifacts: []Artifact{{Target: TargetNPM, Name: "@acme/shared", Version: "1.0.0", File: "npm.tgz"}},
			Files:     map[string][]byte{"npm.tgz": []byte(mod)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewCatalog(store); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("startup err = %v, want ErrNameTaken", err)
	}
}

func TestCatalogReadsWithoutTouchingDisk(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	c, _ := NewCatalog(store)
	for i := range 3 {
		mod := fmt.Sprintf("acme/mod%d", i)
		if _, err := publishTo(t, c, mod, "v1.0.0", goArtifact(fmt.Sprintf("registry.test/gen/go/mod%d", i), "v1.0.0")); err != nil {
			t.Fatal(err)
		}
	}
	mods, err := c.ListModules()
	if err != nil || len(mods) != 3 {
		t.Fatalf("ListModules = %v, %v", mods, err)
	}
	m, idx, err := c.FindByTarget(TargetGo, "registry.test/gen/go/mod1")
	if err != nil || m.String() != "acme/mod1" || len(idx.Versions) != 1 {
		t.Fatalf("FindByTarget = %v %v %v", m, idx, err)
	}
	if _, _, err := c.FindByTarget(TargetGo, "registry.test/gen/go/nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown package err = %v", err)
	}
	if v, err := c.Resolve(m, "latest"); err != nil || v.Version != "v1.0.0" {
		t.Errorf("Resolve = %v %v", v, err)
	}
}

func BenchmarkCatalogFindByTarget(b *testing.B) {
	store, _ := NewStore(b.TempDir())
	c, _ := NewCatalog(store)
	for i := range 200 {
		m := Module{Owner: "acme", Name: fmt.Sprintf("mod%d", i)}
		name := fmt.Sprintf("registry.test/gen/go/mod%d", i)
		for v := range 5 {
			version := fmt.Sprintf("v1.%d.0", v)
			if _, err := c.Publish(PublishRequest{
				Module:    m,
				Version:   version,
				SchemaZip: []byte(name + version),
				Artifacts: []Artifact{goArtifact(name, version)},
				Files:     map[string][]byte{"go.zip": []byte(version)},
			}); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if _, _, err := c.FindByTarget(TargetGo, fmt.Sprintf("registry.test/gen/go/mod%d", i%200)); err != nil {
			b.Fatal(err)
		}
	}
}
