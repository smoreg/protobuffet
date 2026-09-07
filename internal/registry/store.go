package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Errors returned by Store, meant to be mapped onto HTTP status codes by callers.
var (
	ErrNotFound       = errors.New("not found")
	ErrVersionExists  = errors.New("version already published with different content")
	ErrDigestMismatch = errors.New("artifact digest mismatch")
)

// Store keeps modules on a filesystem. Layout under root:
//
//	modules/<owner>/<name>/index.json
//	modules/<owner>/<name>/versions/<version>/schema.zip
//	modules/<owner>/<name>/versions/<version>/<artifact files>
//
// Published versions are immutable: re-publishing identical content succeeds
// (CI retries stay safe), different content is refused.
type Store struct {
	root string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewStore prepares the directory tree under root.
func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "modules"), 0o755); err != nil {
		return nil, fmt.Errorf("create store root: %w", err)
	}
	return &Store{root: root, locks: map[string]*sync.Mutex{}}, nil
}

// Root reports the directory the store was opened on.
func (s *Store) Root() string { return s.root }

func (s *Store) lock(m Module) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := m.String()
	if _, ok := s.locks[key]; !ok {
		s.locks[key] = &sync.Mutex{}
	}
	return s.locks[key]
}

func (s *Store) moduleDir(m Module) string {
	return filepath.Join(s.root, "modules", m.Owner, m.Name)
}

func (s *Store) versionDir(m Module, version string) string {
	return filepath.Join(s.moduleDir(m), "versions", version)
}

// ListModules returns every module known to the store, sorted.
func (s *Store) ListModules() ([]Module, error) {
	base := filepath.Join(s.root, "modules")
	owners, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Module
	for _, o := range owners {
		if !o.IsDir() {
			continue
		}
		names, err := os.ReadDir(filepath.Join(base, o.Name()))
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if !n.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(base, o.Name(), n.Name(), "index.json")); err != nil {
				continue
			}
			out = append(out, Module{Owner: o.Name(), Name: n.Name()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// Index reads a module index.
func (s *Store) Index(m Module) (*Index, error) {
	b, err := os.ReadFile(filepath.Join(s.moduleDir(m), "index.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("module %s: %w", m, ErrNotFound)
		}
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, fmt.Errorf("module %s: parse index: %w", m, err)
	}
	return &idx, nil
}

// PublishRequest carries one version and its files. Files are keyed by the
// artifact file name; schema is the .proto module archive.
type PublishRequest struct {
	Module      Module
	Version     string
	SchemaZip   []byte
	Artifacts   []Artifact
	Files       map[string][]byte
	Labels      []string
	PublishedBy string
	Now         time.Time
}

// Publish writes a version and updates the module index.
func (s *Store) Publish(req PublishRequest) (Version, error) {
	mu := s.lock(req.Module)
	mu.Lock()
	defer mu.Unlock()

	version, err := NormalizeVersion(req.Version)
	if err != nil {
		return Version{}, err
	}
	schemaSHA := sha256hex(req.SchemaZip)

	for _, a := range req.Artifacts {
		body, ok := req.Files[a.File]
		if !ok {
			return Version{}, fmt.Errorf("artifact %s: file %q missing from request", a.Target, a.File)
		}
		if a.SHA256 != "" && a.SHA256 != sha256hex(body) {
			return Version{}, fmt.Errorf("artifact %s: %w", a.Target, ErrDigestMismatch)
		}
	}

	idx, err := s.Index(req.Module)
	if errors.Is(err, ErrNotFound) {
		idx = &Index{Module: req.Module, Labels: map[string]string{}}
	} else if err != nil {
		return Version{}, err
	}

	if existing, ok := idx.Find(version); ok {
		if existing.SchemaSHA == schemaSHA {
			return existing, nil // idempotent re-publish
		}
		return Version{}, fmt.Errorf("%s@%s: %w", req.Module, version, ErrVersionExists)
	}

	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	v := Version{
		Version:     version,
		CreatedAt:   now.UTC(),
		SchemaSHA:   schemaSHA,
		Artifacts:   map[Target]Artifact{},
		PublishedBy: req.PublishedBy,
	}

	dir := s.versionDir(req.Module, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Version{}, err
	}
	if err := writeFileAtomic(filepath.Join(dir, "schema.zip"), req.SchemaZip); err != nil {
		return Version{}, err
	}
	for _, a := range req.Artifacts {
		body := req.Files[a.File]
		if err := writeFileAtomic(filepath.Join(dir, a.File), body); err != nil {
			return Version{}, err
		}
		a.Size = int64(len(body))
		if a.SHA256 == "" {
			a.SHA256 = sha256hex(body)
		}
		v.Artifacts[a.Target] = a
	}

	idx.Versions = append(idx.Versions, v)
	sort.Slice(idx.Versions, func(i, j int) bool {
		return CompareVersions(idx.Versions[i].Version, idx.Versions[j].Version) < 0
	})
	if idx.Labels == nil {
		idx.Labels = map[string]string{}
	}
	for _, l := range req.Labels {
		if l != "" {
			idx.Labels[l] = version
		}
	}
	idx.Updated = now.UTC()
	if err := s.writeIndex(req.Module, idx); err != nil {
		return Version{}, err
	}
	return v, nil
}

func (s *Store) writeIndex(m Module, idx *Index) error {
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.moduleDir(m), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.moduleDir(m), "index.json"), append(b, '\n'))
}

// SetLabel points a mutable label (for example "main") at a version.
func (s *Store) SetLabel(m Module, label, version string) error {
	mu := s.lock(m)
	mu.Lock()
	defer mu.Unlock()

	idx, err := s.Index(m)
	if err != nil {
		return err
	}
	if _, ok := idx.Find(version); !ok {
		return fmt.Errorf("%s@%s: %w", m, version, ErrNotFound)
	}
	if idx.Labels == nil {
		idx.Labels = map[string]string{}
	}
	idx.Labels[label] = version
	return s.writeIndex(m, idx)
}

// Resolve turns a version, a label or "latest" into a concrete version.
func (s *Store) Resolve(m Module, ref string) (Version, error) {
	idx, err := s.Index(m)
	if err != nil {
		return Version{}, err
	}
	if ref == "" || ref == "latest" {
		v, ok := idx.Latest()
		if !ok {
			return Version{}, fmt.Errorf("%s: no versions: %w", m, ErrNotFound)
		}
		return v, nil
	}
	if target, ok := idx.Labels[ref]; ok {
		ref = target
	}
	normalized, err := NormalizeVersion(ref)
	if err != nil {
		return Version{}, err
	}
	v, ok := idx.Find(normalized)
	if !ok {
		return Version{}, fmt.Errorf("%s@%s: %w", m, normalized, ErrNotFound)
	}
	return v, nil
}

// Open returns a file stored for a version: "schema.zip" or an artifact file.
func (s *Store) Open(m Module, version, file string) (io.ReadSeekCloser, int64, error) {
	if strings.ContainsAny(file, `/\`) || file == "" || strings.HasPrefix(file, ".") {
		return nil, 0, fmt.Errorf("file %q: %w", file, ErrNotFound)
	}
	p := filepath.Join(s.versionDir(m, version), file)
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, fmt.Errorf("%s@%s/%s: %w", m, version, file, ErrNotFound)
		}
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

// ReadAll reads a stored file into memory. Used for small files and for the
// server side compatibility check.
func (s *Store) ReadAll(m Module, version, file string) ([]byte, error) {
	f, _, err := s.Open(m, version, file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// FindByTarget locates the module and version that published an artifact under
// the given ecosystem name, for example a Go module path. Registries are
// addressed by package name, not by module name, so the lookup has to go the
// other way round.
func (s *Store) FindByTarget(t Target, name string) (Module, *Index, error) {
	mods, err := s.ListModules()
	if err != nil {
		return Module{}, nil, err
	}
	for _, m := range mods {
		idx, err := s.Index(m)
		if err != nil {
			continue
		}
		for _, v := range idx.Versions {
			if a, ok := v.Artifact(t); ok && a.Name == name {
				return m, idx, nil
			}
		}
	}
	return Module{}, nil, fmt.Errorf("%s package %q: %w", t, name, ErrNotFound)
}

func writeFileAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SHA256Hex exposes the digest helper to other packages.
func SHA256Hex(b []byte) string { return sha256hex(b) }
