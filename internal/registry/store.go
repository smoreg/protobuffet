package registry

import (
	"crypto/sha1"
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

// NewStore prepares the directory tree under root and verifies it is writable.
// The check matters in containers: a volume owned by root leaves a nonroot
// process able to read but not write, and without this the failure surfaces
// only at the first publish.
func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "modules"), 0o755); err != nil {
		return nil, fmt.Errorf("create store root: %w", err)
	}
	probe, err := os.CreateTemp(root, ".writable-")
	if err != nil {
		return nil, fmt.Errorf("data directory %s is not writable: %w (check volume ownership: the container runs as uid 65532)", root, err)
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		return nil, err
	}
	if err := os.Remove(name); err != nil {
		return nil, err
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

// underRoot rejects any path that escapes the store, whatever produced it.
// Callers validate their input; this is the backstop that keeps a future
// caller from turning a validation slip into arbitrary filesystem access.
func (s *Store) underRoot(path string) error {
	base := filepath.Clean(filepath.Join(s.root, "modules"))
	clean := filepath.Clean(path)
	if clean != base && !strings.HasPrefix(clean, base+string(os.PathSeparator)) {
		return fmt.Errorf("path %q escapes the store: %w", path, ErrNotFound)
	}
	return nil
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
	sortModules(out)
	return out, nil
}

func sortModules(mods []Module) {
	sort.Slice(mods, func(i, j int) bool { return mods[i].String() < mods[j].String() })
}

// Index reads a module index.
func (s *Store) Index(m Module) (*Index, error) {
	path := filepath.Join(s.moduleDir(m), "index.json")
	if err := s.underRoot(path); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
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

	// Digests are computed here, from the bytes that will actually be stored.
	// A digest supplied by the client is treated as a claim to verify, never
	// as the value to publish: a wrong shasum in Composer or npm metadata
	// makes the package uninstallable, and versions are immutable.
	for i, a := range req.Artifacts {
		body, ok := req.Files[a.File]
		if !ok {
			return Version{}, fmt.Errorf("artifact %s: file %q missing from request", a.Target, a.File)
		}
		sha256sum, sha1sum := sha256hex(body), sha1hex(body)
		if a.SHA256 != "" && a.SHA256 != sha256sum {
			return Version{}, fmt.Errorf("artifact %s: sha256 claimed %s, got %s: %w",
				a.Target, short(a.SHA256), short(sha256sum), ErrDigestMismatch)
		}
		if a.SHA1 != "" && a.SHA1 != sha1sum {
			return Version{}, fmt.Errorf("artifact %s: sha1 claimed %s, got %s: %w",
				a.Target, short(a.SHA1), short(sha1sum), ErrDigestMismatch)
		}
		req.Artifacts[i].SHA256 = sha256sum
		req.Artifacts[i].SHA1 = sha1sum
	}

	idx, err := s.Index(req.Module)
	if errors.Is(err, ErrNotFound) {
		idx = &Index{Module: req.Module, Labels: map[string]string{}}
	} else if err != nil {
		return Version{}, err
	}

	if existing, ok := idx.Find(version); ok {
		// A CI retry re-uploads byte-identical content and must succeed. The
		// same schema with different generated code is a different release
		// wearing an old number, and that has to be refused: consumers who
		// already resolved this version would never see the new code.
		if diff := compareVersions(existing, schemaSHA, req.Artifacts); diff == "" {
			return existing, nil
		} else {
			return Version{}, fmt.Errorf("%s@%s: %s: %w", req.Module, version, diff, ErrVersionExists)
		}
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

	// Stage the whole version in a temporary directory and move it into place
	// once every file is written. A crash mid-publish then leaves a staging
	// directory to clean up, never a half-written version that the index does
	// not know about but a later publish would silently reuse.
	final := s.versionDir(req.Module, version)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return Version{}, err
	}
	// A leftover directory without an index entry is an orphan from an earlier
	// crash: the index is authoritative, so it goes.
	if _, statErr := os.Stat(final); statErr == nil {
		if err := os.RemoveAll(final); err != nil {
			return Version{}, fmt.Errorf("remove orphaned version directory: %w", err)
		}
	}
	staging, err := os.MkdirTemp(filepath.Dir(final), ".staging-"+version+"-")
	if err != nil {
		return Version{}, err
	}
	// Removing a directory that was already renamed into place is expected.
	defer os.RemoveAll(staging) //nolint:errcheck // best-effort cleanup of a staging dir

	if err := writeFileAtomic(filepath.Join(staging, "schema.zip"), req.SchemaZip); err != nil {
		return Version{}, err
	}
	for _, a := range req.Artifacts {
		body := req.Files[a.File]
		if err := writeFileAtomic(filepath.Join(staging, a.File), body); err != nil {
			return Version{}, err
		}
		a.Size = int64(len(body))
		v.Artifacts[a.Target] = a
	}
	if err := os.Rename(staging, final); err != nil {
		return Version{}, fmt.Errorf("publish %s@%s: %w", req.Module, version, err)
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
		// The files are in place but unreferenced; the next publish of this
		// version will clear them as an orphan.
		return Version{}, err
	}
	return v, nil
}

// compareVersions describes how a re-publish differs from what is stored, or
// returns "" when the two are identical.
func compareVersions(existing Version, schemaSHA string, artifacts []Artifact) string {
	if existing.SchemaSHA != schemaSHA {
		return "schema differs"
	}
	if len(existing.Artifacts) != len(artifacts) {
		return fmt.Sprintf("published with %d artifacts, re-published with %d",
			len(existing.Artifacts), len(artifacts))
	}
	for _, a := range artifacts {
		old, ok := existing.Artifacts[a.Target]
		if !ok {
			return fmt.Sprintf("target %s is new", a.Target)
		}
		if old.Name != a.Name {
			return fmt.Sprintf("target %s: package name changed from %q to %q", a.Target, old.Name, a.Name)
		}
		if a.SHA256 != "" && old.SHA256 != a.SHA256 {
			return fmt.Sprintf("target %s: content differs", a.Target)
		}
	}
	return ""
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
	return resolveIn(idx, m, ref)
}

func resolveIn(idx *Index, m Module, ref string) (Version, error) {
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

// readSeekCloser is what http.ServeContent needs from a stored file.
type readSeekCloser = io.ReadSeekCloser

// Open returns a file stored for a version: "schema.zip" or an artifact file.
func (s *Store) Open(m Module, version, file string) (io.ReadSeekCloser, int64, error) {
	if strings.ContainsAny(file, `/\`) || file == "" || strings.HasPrefix(file, ".") {
		return nil, 0, fmt.Errorf("file %q: %w", file, ErrNotFound)
	}
	p := filepath.Join(s.versionDir(m, version), file)
	if err := s.underRoot(p); err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, fmt.Errorf("%s@%s/%s: %w", m, version, file, ErrNotFound)
		}
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close() //nolint:errcheck // already failing
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

// CleanOrphans removes version directories that no index references, and
// leftover staging directories. Returns the paths removed.
func (s *Store) CleanOrphans() ([]string, error) {
	mods, err := s.ListModules()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, m := range mods {
		mu := s.lock(m)
		mu.Lock()
		idx, err := s.Index(m)
		if err != nil {
			mu.Unlock()
			return removed, err
		}
		known := map[string]bool{}
		for _, v := range idx.Versions {
			known[v.Version] = true
		}
		entries, err := os.ReadDir(filepath.Join(s.moduleDir(m), "versions"))
		if err != nil && !os.IsNotExist(err) {
			mu.Unlock()
			return removed, err
		}
		for _, e := range entries {
			if !e.IsDir() || known[e.Name()] {
				continue
			}
			path := filepath.Join(s.moduleDir(m), "versions", e.Name())
			if err := os.RemoveAll(path); err != nil {
				mu.Unlock()
				return removed, err
			}
			removed = append(removed, path)
		}
		mu.Unlock()
	}
	return removed, nil
}

func writeFileAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // best-effort cleanup, the file is renamed away on success
	if _, err := tmp.Write(body); err != nil {
		tmp.Close() //nolint:errcheck // already failing
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck // already failing
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	// A rename is only durable once the directory entry is flushed.
	return fsyncDir(dir)
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sha1hex is what Composer and npm put in their metadata. It guards against a
// corrupted transfer, not against a hostile one; authenticity comes from TLS.
func sha1hex(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

// SHA256Hex exposes the digest helper to other packages.
func SHA256Hex(b []byte) string { return sha256hex(b) }
