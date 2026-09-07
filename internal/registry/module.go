// Package registry holds the domain model of the schema registry: modules,
// versions and the artifacts published for each version.
package registry

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Target names the ecosystem an artifact is published for.
type Target string

const (
	TargetGo       Target = "go"
	TargetComposer Target = "composer"
	TargetNPM      Target = "npm"
	TargetPython   Target = "python"
)

// AllTargets lists every supported ecosystem, in a stable order.
var AllTargets = []Target{TargetGo, TargetComposer, TargetNPM, TargetPython}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Module identifies a schema module as owner/name, e.g. "acme/catalog".
type Module struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

// ParseModule parses "owner/name" and rejects anything that would not be safe
// as a path segment. Both parts are lowercase to keep artifact paths
// case-insensitive across filesystems.
func ParseModule(s string) (Module, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok {
		return Module{}, fmt.Errorf("module %q: want owner/name", s)
	}
	if !nameRe.MatchString(owner) || !nameRe.MatchString(name) {
		return Module{}, fmt.Errorf("module %q: each part must match %s", s, nameRe)
	}
	return Module{Owner: owner, Name: name}, nil
}

func (m Module) String() string { return m.Owner + "/" + m.Name }

// Artifact is one publishable file for one ecosystem.
type Artifact struct {
	Target Target `json:"target"`
	// Name is the identifier in the target ecosystem: a Go module path, a
	// Composer package name or an npm package name.
	Name string `json:"name"`
	// Version is the version string as the target ecosystem spells it, which
	// is not always the registry version (Go keeps the v prefix, npm does not).
	Version string `json:"version"`
	// File is the artifact file name inside the version directory.
	File   string `json:"file"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	SHA1   string `json:"sha1"`
	// Meta carries ecosystem specific fields: Go requirements, Composer
	// autoload rules, npm dependencies.
	Meta map[string]any `json:"meta,omitempty"`
}

// Version is one published revision of a module.
type Version struct {
	Version   string              `json:"version"`
	CreatedAt time.Time           `json:"created_at"`
	SchemaSHA string              `json:"schema_sha256"`
	Artifacts map[Target]Artifact `json:"artifacts"`
	// PublishedBy records the token subject that pushed the version.
	PublishedBy string `json:"published_by,omitempty"`
}

// Artifact returns the artifact for a target.
func (v Version) Artifact(t Target) (Artifact, bool) {
	a, ok := v.Artifacts[t]
	return a, ok
}

// Index is the per-module metadata file: every version plus mutable labels.
type Index struct {
	Module   Module            `json:"module"`
	Versions []Version         `json:"versions"`
	Labels   map[string]string `json:"labels,omitempty"`
	Updated  time.Time         `json:"updated"`
}

// Find returns the named version.
func (i *Index) Find(version string) (Version, bool) {
	for _, v := range i.Versions {
		if v.Version == version {
			return v, true
		}
	}
	return Version{}, false
}

// Latest returns the highest semver version, which is the last entry because
// Versions is kept sorted on write.
func (i *Index) Latest() (Version, bool) {
	if len(i.Versions) == 0 {
		return Version{}, false
	}
	return i.Versions[len(i.Versions)-1], true
}
