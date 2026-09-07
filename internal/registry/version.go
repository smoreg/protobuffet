package registry

import (
	"fmt"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// PseudoVersionLayout is the timestamp layout Go requires in pseudo-versions.
const PseudoVersionLayout = "20060102150405"

// PseudoVersion builds a version for an untagged schema revision in the exact
// shape the Go toolchain accepts: v0.0.0-<utc timestamp>-<12 hex digits>.
// Anything else is rejected by `go mod download` with "invalid pseudo-version".
func PseudoVersion(t time.Time, digest string) (string, error) {
	if len(digest) < 12 {
		return "", fmt.Errorf("digest %q too short for a pseudo-version", digest)
	}
	return fmt.Sprintf("v0.0.0-%s-%s", t.UTC().Format(PseudoVersionLayout), digest[:12]), nil
}

// NormalizeVersion accepts "1.2.3" or "v1.2.3" and returns the canonical
// v-prefixed form used everywhere in the registry.
func NormalizeVersion(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("empty version")
	}
	if !strings.HasPrefix(s, "v") {
		s = "v" + s
	}
	if !semver.IsValid(s) {
		return "", fmt.Errorf("version %q is not valid semver", s)
	}
	// x/mod/semver accepts the shorthands v1 and v1.2; a registry version has
	// to be complete, because that is what ends up in a consumer's lock file.
	if semver.Canonical(s) != s {
		return "", fmt.Errorf("version %q must be vMAJOR.MINOR.PATCH", s)
	}
	return s, nil
}

// EcosystemVersion converts a registry version to the spelling a non-Go
// ecosystem expects: Composer and npm both reject the leading v.
func EcosystemVersion(v string) string { return strings.TrimPrefix(v, "v") }

// PythonVersion converts a registry version to PEP 440, which is stricter than
// semver: it has no place for a dash, so a pseudo-version becomes a dev
// release and a prerelease tag loses its separators (v1.2.3-rc.1 -> 1.2.3rc1).
func PythonVersion(v string) string {
	base := EcosystemVersion(v)
	if IsPseudo(v) {
		parts := strings.SplitN(base, "-", 3)
		if len(parts) >= 2 {
			return parts[0] + ".dev" + parts[1]
		}
	}
	release, pre, found := strings.Cut(base, "-")
	if !found {
		return base
	}
	return release + strings.NewReplacer(".", "", "-", "").Replace(pre)
}

// CompareVersions orders two registry versions by semver precedence.
func CompareVersions(a, b string) int { return semver.Compare(a, b) }

// IsPseudo reports whether a version was generated rather than tagged.
func IsPseudo(v string) bool {
	return strings.HasPrefix(v, "v0.0.0-") && strings.Count(v, "-") >= 2
}
