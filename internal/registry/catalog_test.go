package registry

import (
	"errors"
	"fmt"
	"sync"
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

func TestCatalogStartsDespiteConflictsOnDisk(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	// Two modules claiming one npm name, written straight to the store: a
	// registry from before ownership checks, or a partial restore.
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for i, mod := range []string{"acme/two", "acme/one"} {
		m, _ := ParseModule(mod)
		if _, err := store.Publish(PublishRequest{
			Module:    m,
			Version:   "v1.0.0",
			SchemaZip: []byte(mod),
			Artifacts: []Artifact{{Target: TargetNPM, Name: "@acme/shared", Version: "1.0.0", File: "npm.tgz"}},
			Files:     map[string][]byte{"npm.tgz": []byte(mod)},
			// acme/two published first, so it keeps the name.
			Now: base.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Refusing to start would leave the operator with no API to fix it, and a
	// conflict is data, not a code failure.
	c, err := NewCatalog(store)
	if err != nil {
		t.Fatalf("startup failed on a conflicting store: %v", err)
	}
	conflicts := c.Conflicts()
	if len(conflicts) != 1 || conflicts[0].Name != "@acme/shared" {
		t.Fatalf("conflicts = %+v", conflicts)
	}
	// The winner is the earliest publisher, not whoever the filesystem
	// happened to return first.
	if conflicts[0].Owner.String() != "acme/two" {
		t.Errorf("owner = %s, want acme/two", conflicts[0].Owner)
	}
	owner, _, err := c.FindByTarget(TargetNPM, "@acme/shared")
	if err != nil || owner.String() != "acme/two" {
		t.Errorf("FindByTarget = %s, %v", owner, err)
	}
	// A third module still cannot take the name.
	if _, err := publishTo(t, c, "acme/three", "v1.0.0",
		Artifact{Target: TargetNPM, Name: "@acme/shared", Version: "1.0.0", File: "npm.tgz"}); !errors.Is(err, ErrNameTaken) {
		t.Errorf("publish under a conflicted name = %v", err)
	}
}

func TestPythonNamesCannotBeClaimedTwice(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	c, _ := NewCatalog(store)

	if _, err := publishTo(t, c, "team1/svc", "v1.0.0",
		Artifact{Target: TargetPython, Name: "acme-catalog-proto", Version: "1.0.0", File: "python.whl"}); err != nil {
		t.Fatal(err)
	}
	// pip normalizes per PEP 503, so these are the same project. Tracking
	// ownership under the raw name would let another module publish wheels
	// onto the same index page and shadow the original with a higher version.
	for _, alias := range []string{"Acme.Catalog.Proto", "acme_catalog_proto", "ACME-CATALOG-PROTO"} {
		_, err := publishTo(t, c, "team2/evil", "v9.9.9",
			Artifact{Target: TargetPython, Name: alias, Version: "9.9.9", File: "python.whl"})
		if !errors.Is(err, ErrNameTaken) {
			t.Errorf("alias %q was accepted: %v", alias, err)
		}
	}
	// Composer and npm match case-insensitively too.
	if _, err := publishTo(t, c, "team1/svc", "v1.1.0",
		Artifact{Target: TargetComposer, Name: "acme/catalog-proto", Version: "1.1.0", File: "composer.zip"}); err != nil {
		t.Fatal(err)
	}
	if _, err := publishTo(t, c, "team2/evil", "v1.0.0",
		Artifact{Target: TargetComposer, Name: "Acme/Catalog-Proto", Version: "1.0.0", File: "composer.zip"}); !errors.Is(err, ErrNameTaken) {
		t.Errorf("composer name taken with different case: %v", err)
	}
}

func TestConcurrentPublishesCannotBothClaimAName(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	c, _ := NewCatalog(store)

	// Both goroutines race for one npm name. Exactly one must win, and the
	// store must be loadable afterwards: checking ownership and then dropping
	// the lock before writing let both through, and the registry only found
	// out at the next restart.
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, mod := range []string{"team1/m", "team2/m"} {
		wg.Add(1)
		go func(i int, mod string) {
			defer wg.Done()
			m, _ := ParseModule(mod)
			_, err := c.Publish(PublishRequest{
				Module:    m,
				Version:   "v1.0.0",
				SchemaZip: []byte(mod),
				Artifacts: []Artifact{{Target: TargetComposer, Name: "shared/pkg", Version: "1.0.0", File: "composer.zip"}},
				Files:     map[string][]byte{"composer.zip": []byte(mod)},
			})
			results[i] = err
		}(i, mod)
	}
	wg.Wait()

	failures := 0
	for _, err := range results {
		if err != nil {
			failures++
			if !errors.Is(err, ErrNameTaken) {
				t.Errorf("unexpected failure: %v", err)
			}
		}
	}
	if failures != 1 {
		t.Fatalf("%d of 2 concurrent publishes failed, want exactly 1", failures)
	}
	fresh, err := NewCatalog(store)
	if err != nil {
		t.Fatalf("store is unloadable after the race: %v", err)
	}
	if got := fresh.Conflicts(); len(got) != 0 {
		t.Errorf("conflicts on disk after the race: %+v", got)
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

func TestCatalogAccessors(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	c, _ := NewCatalog(store)
	if c.Store() != store {
		t.Error("Store() returned another store")
	}
	if c.Root() != store.Root() {
		t.Errorf("Root() = %q, want %q", c.Root(), store.Root())
	}
	if _, err := c.Index(Module{Owner: "acme", Name: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Index of a missing module: %v", err)
	}
	if _, err := c.Resolve(Module{Owner: "acme", Name: "missing"}, "latest"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve of a missing module: %v", err)
	}

	if _, err := publishTo(t, c, "acme/catalog", "v1.0.0", goArtifact("registry.test/gen/go/catalog", "v1.0.0")); err != nil {
		t.Fatal(err)
	}
	m := Module{Owner: "acme", Name: "catalog"}
	if err := c.SetLabel(m, "stable", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if v, err := c.Resolve(m, "stable"); err != nil || v.Version != "v1.0.0" {
		t.Errorf("label resolve = %v, %v", v, err)
	}
	// The catalog serves files through the store it wraps.
	if body, err := c.ReadAll(m, "v1.0.0", "schema.zip"); err != nil || len(body) == 0 {
		t.Errorf("ReadAll = %d bytes, %v", len(body), err)
	}
	f, size, err := c.Open(m, "v1.0.0", "go.zip")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if size == 0 {
		t.Error("Open reported an empty artifact")
	}
}

func TestMajorOf(t *testing.T) {
	for in, want := range map[string]string{
		"v1.2.3":      "v1",
		"v2.0.0":      "v2",
		"v0.9.0":      "v0",
		"v2.0.0-rc.1": "v2",
	} {
		if got := MajorOf(in); got != want {
			t.Errorf("MajorOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStoreLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	first, err := LockStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A gc run against a live registry's volume would otherwise interleave
	// index rewrites with a publish and lose one of them.
	if _, err := LockStore(dir); err == nil {
		t.Fatal("a second process took the lock")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := LockStore(dir)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	// Releasing twice, or releasing a nil lock, must not panic: both happen
	// on shutdown paths.
	if err := second.Release(); err == nil {
		t.Log("double release returned no error, which is acceptable")
	}
	var nilLock *Lock
	if err := nilLock.Release(); err != nil {
		t.Errorf("nil release = %v", err)
	}
}
