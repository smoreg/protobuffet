package registry

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/smoreg/protobuffet/internal/pkgname"
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

	mu        sync.RWMutex
	modules   map[string]*Index
	byTarget  map[Target]map[string]Module
	conflicts map[string]Conflict
}

// NewCatalog loads every module from the store.
//
// A name claimed by two modules on disk is a conflict, but not a reason to
// refuse to start: an unreachable registry is worse than an ambiguous package
// name, and the operator needs the API to fix it. The earliest publisher keeps
// the name, the conflict is logged loudly, and further publishes under that
// name are refused.
func NewCatalog(store *Store) (*Catalog, error) {
	c := &Catalog{
		store:     store,
		modules:   map[string]*Index{},
		byTarget:  map[Target]map[string]Module{},
		conflicts: map[string]Conflict{},
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
		c.modules[m.String()] = idx
	}
	c.rebuildOwnership()
	return c, nil
}

// Conflict records two modules publishing the same package name.
type Conflict struct {
	Target Target
	Name   string
	Owner  Module
	Other  Module
}

// Conflicts reports name collisions found on disk, for the operator to fix.
func (c *Catalog) Conflicts() []Conflict {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Conflict, 0, len(c.conflicts))
	for _, v := range c.conflicts {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// rebuildOwnership recomputes name ownership from the loaded indexes. The
// winner is the module that published the name first, so the outcome does not
// depend on directory iteration order.
func (c *Catalog) rebuildOwnership() {
	type claim struct {
		module Module
		first  time.Time
	}
	claims := map[Target]map[string]claim{}
	c.conflicts = map[string]Conflict{}

	for _, idx := range c.modules {
		for _, v := range idx.Versions {
			for target, a := range v.Artifacts {
				key := CanonicalPackageName(target, a.Name)
				byName, ok := claims[target]
				if !ok {
					byName = map[string]claim{}
					claims[target] = byName
				}
				existing, taken := byName[key]
				switch {
				case !taken:
					byName[key] = claim{module: idx.Module, first: v.CreatedAt}
				case existing.module == idx.Module:
					if v.CreatedAt.Before(existing.first) {
						byName[key] = claim{module: idx.Module, first: v.CreatedAt}
					}
				default:
					winner, loser := existing, claim{module: idx.Module, first: v.CreatedAt}
					if loser.first.Before(winner.first) {
						winner, loser = loser, winner
					}
					byName[key] = winner
					c.conflicts[string(target)+"/"+key] = Conflict{
						Target: target, Name: a.Name, Owner: winner.module, Other: loser.module,
					}
				}
			}
		}
	}

	c.byTarget = map[Target]map[string]Module{}
	for target, byName := range claims {
		owners := map[string]Module{}
		for name, cl := range byName {
			owners[name] = cl.module
		}
		c.byTarget[target] = owners
	}
}

// CanonicalPackageName is the key ownership is tracked under. It has to match
// how the serving side matches a request, or a name can be claimed twice and
// then collide at serve time: pip normalizes per PEP 503, Composer and npm
// treat names case-insensitively, and Go module paths are case-sensitive
// (the proxy protocol escapes capitals instead).
func CanonicalPackageName(target Target, name string) string {
	switch target {
	case TargetPython:
		return pkgname.NormalizePython(name)
	case TargetComposer:
		return pkgname.NormalizeComposer(name)
	case TargetNPM:
		return pkgname.NormalizeNPM(name)
	default:
		return name
	}
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

	m, ok := c.byTarget[t][CanonicalPackageName(t, name)]
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
	// Names are reserved under the write lock before anything reaches the
	// disk. Checking and then releasing the lock would let two concurrent
	// publishes both pass, both write, and leave the store in a state that
	// only surfaces on the next restart.
	reserved, err := c.reserveNames(req.Module, req.Artifacts)
	if err != nil {
		return Version{}, err
	}
	version, err := c.store.Publish(req)
	if err != nil {
		c.releaseNames(reserved)
		return Version{}, err
	}
	return version, c.reload(req.Module)
}

// reservation is one name held for a module until the publish completes.
type reservation struct {
	target Target
	key    string
}

// reserveNames claims every package name in one atomic step.
func (c *Catalog) reserveNames(m Module, artifacts []Artifact) ([]reservation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var reserved []reservation
	for _, a := range artifacts {
		key := CanonicalPackageName(a.Target, a.Name)
		owners, ok := c.byTarget[a.Target]
		if !ok {
			owners = map[string]Module{}
			c.byTarget[a.Target] = owners
		}
		if owner, taken := owners[key]; taken {
			if owner == m {
				continue
			}
			// Undo the partial reservation so a refused publish leaves no
			// trace in the ownership map.
			c.releaseLocked(reserved)
			return nil, fmt.Errorf("%s package %q: owned by %s: %w", a.Target, a.Name, owner, ErrNameTaken)
		}
		owners[key] = m
		reserved = append(reserved, reservation{target: a.Target, key: key})
	}
	return reserved, nil
}

func (c *Catalog) releaseNames(reserved []reservation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseLocked(reserved)
}

func (c *Catalog) releaseLocked(reserved []reservation) {
	for _, r := range reserved {
		delete(c.byTarget[r.target], r.key)
	}
}

// SetYanked marks or unmarks a version as yanked.
func (c *Catalog) SetYanked(m Module, version string, yanked bool, reason, by string, now time.Time) error {
	if err := c.store.SetYanked(m, version, yanked, reason, by, now); err != nil {
		return err
	}
	return c.reload(m)
}

// SetLabel points a label at a version.
func (c *Catalog) SetLabel(m Module, label, version string) error {
	if err := c.store.SetLabel(m, label, version); err != nil {
		return err
	}
	return c.reload(m)
}

// reload re-reads one module from disk into the catalog and recomputes name
// ownership, which is cheap and keeps a removed version from leaving a stale
// claim behind.
func (c *Catalog) reload(m Module) error {
	idx, err := c.store.Index(m)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.modules[m.String()] = idx
	c.rebuildOwnership()
	return nil
}

// Reload re-reads every module from disk. It exists for changes made outside
// this process, such as an offline garbage collection run.
func (c *Catalog) Reload() error {
	mods, err := c.store.ListModules()
	if err != nil {
		return err
	}
	loaded := make(map[string]*Index, len(mods))
	for _, m := range mods {
		idx, err := c.store.Index(m)
		if err != nil {
			return fmt.Errorf("load %s: %w", m, err)
		}
		loaded[m.String()] = idx
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.modules = loaded
	c.rebuildOwnership()
	return nil
}

// PrunePseudoVersions applies the retention policy and refreshes the catalog.
func (c *Catalog) PrunePseudoVersions(opts PruneOptions) ([]Pruned, error) {
	pruned, err := c.store.PrunePseudoVersions(opts)
	if err != nil || opts.DryRun {
		return pruned, err
	}
	touched := map[string]Module{}
	for _, p := range pruned {
		touched[p.Module.String()] = p.Module
	}
	for _, m := range touched {
		if err := c.reload(m); err != nil {
			return pruned, err
		}
	}
	return pruned, nil
}

// Open returns a stored file.
func (c *Catalog) Open(m Module, version, file string) (readSeekCloser, int64, error) {
	return c.store.Open(m, version, file)
}

// ReadAll reads a stored file into memory.
func (c *Catalog) ReadAll(m Module, version, file string) ([]byte, error) {
	return c.store.ReadAll(m, version, file)
}
