package registry

import (
	"fmt"
	"sync"
)

// ErrNameTaken means another module already publishes a package under this
// name in that ecosystem.
var ErrNameTaken = fmt.Errorf("package name already owned by another module")

// Catalog is the in-memory view of the store. Every read path goes through it,
// so serving a package-manager request costs a map lookup instead of walking
// the data directory and parsing every index.json — the difference between a
// `go get` that is instant and one that reads the whole registry five times.
//
// The store on disk stays the source of truth; the catalog is derived state,
// rebuilt at startup and updated under the same lock that writes.
type Catalog struct {
	store *Store

	mu       sync.RWMutex
	modules  map[string]*Index
	byTarget map[Target]map[string]Module
}

// NewCatalog loads every module from the store. It fails if two modules claim
// the same package name in one ecosystem, because serving that is ambiguous
// and silently picking one is how a consumer ends up with the wrong package.
func NewCatalog(store *Store) (*Catalog, error) {
	c := &Catalog{
		store:    store,
		modules:  map[string]*Index{},
		byTarget: map[Target]map[string]Module{},
	}
	mods, err := store.ListModules()
	if err != nil {
		return nil, err
	}
	for _, m := range mods {
		idx, err := store.Index(m)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", m, err)
		}
		if err := c.claimNames(m, idx); err != nil {
			return nil, err
		}
		c.modules[m.String()] = idx
	}
	return c, nil
}

// claimNames records package-name ownership for a module's versions.
func (c *Catalog) claimNames(m Module, idx *Index) error {
	for _, v := range idx.Versions {
		for target, a := range v.Artifacts {
			if err := c.claim(target, a.Name, m); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Catalog) claim(target Target, name string, m Module) error {
	owners, ok := c.byTarget[target]
	if !ok {
		owners = map[string]Module{}
		c.byTarget[target] = owners
	}
	if owner, taken := owners[name]; taken && owner != m {
		return fmt.Errorf("%s package %q: owned by %s, claimed by %s: %w", target, name, owner, m, ErrNameTaken)
	}
	owners[name] = m
	return nil
}

// Store exposes the underlying store for file access.
func (c *Catalog) Store() *Store { return c.store }

// Root reports the data directory.
func (c *Catalog) Root() string { return c.store.Root() }

// ListModules returns every module, sorted by name.
func (c *Catalog) ListModules() ([]Module, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]Module, 0, len(c.modules))
	for _, idx := range c.modules {
		out = append(out, idx.Module)
	}
	sortModules(out)
	return out, nil
}

// Index returns a module index. The returned value is shared, so callers must
// not modify it.
func (c *Catalog) Index(m Module) (*Index, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	idx, ok := c.modules[m.String()]
	if !ok {
		return nil, fmt.Errorf("module %s: %w", m, ErrNotFound)
	}
	return idx, nil
}

// FindByTarget resolves an ecosystem package name to the module publishing it.
func (c *Catalog) FindByTarget(t Target, name string) (Module, *Index, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	m, ok := c.byTarget[t][name]
	if !ok {
		return Module{}, nil, fmt.Errorf("%s package %q: %w", t, name, ErrNotFound)
	}
	return m, c.modules[m.String()], nil
}

// Resolve turns a version, a label or "latest" into a concrete version.
func (c *Catalog) Resolve(m Module, ref string) (Version, error) {
	idx, err := c.Index(m)
	if err != nil {
		return Version{}, err
	}
	return resolveIn(idx, m, ref)
}

// Publish writes a version through the store and refreshes the catalog.
func (c *Catalog) Publish(req PublishRequest) (Version, error) {
	// Check name ownership before touching the disk: a rejected publish must
	// not leave artifacts behind.
	c.mu.RLock()
	for _, a := range req.Artifacts {
		if owner, taken := c.byTarget[a.Target][a.Name]; taken && owner != req.Module {
			c.mu.RUnlock()
			return Version{}, fmt.Errorf("%s package %q: owned by %s: %w", a.Target, a.Name, owner, ErrNameTaken)
		}
	}
	c.mu.RUnlock()

	version, err := c.store.Publish(req)
	if err != nil {
		return Version{}, err
	}
	return version, c.reload(req.Module)
}

// SetLabel points a label at a version.
func (c *Catalog) SetLabel(m Module, label, version string) error {
	if err := c.store.SetLabel(m, label, version); err != nil {
		return err
	}
	return c.reload(m)
}

// reload re-reads one module from disk into the catalog.
func (c *Catalog) reload(m Module) error {
	idx, err := c.store.Index(m)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.modules[m.String()] = idx
	return c.claimNames(m, idx)
}

// Open returns a stored file.
func (c *Catalog) Open(m Module, version, file string) (readSeekCloser, int64, error) {
	return c.store.Open(m, version, file)
}

// ReadAll reads a stored file into memory.
func (c *Catalog) ReadAll(m Module, version, file string) ([]byte, error) {
	return c.store.ReadAll(m, version, file)
}
