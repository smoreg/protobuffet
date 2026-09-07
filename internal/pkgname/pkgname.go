// Package pkgname holds the naming rules each package ecosystem applies when
// it decides whether two names are the same.
//
// It exists as its own package because two layers need the same answer: the
// registry, when it records who owns a name, and the packers, when they build
// a file name. If those two disagreed, a name could be claimed twice and only
// collide later, at serve time.
package pkgname

import (
	"regexp"
	"strings"
)

var separators = regexp.MustCompile(`[-_.]+`)

// NormalizePython applies PEP 503 normalization, which is how pip matches a
// requested package against an index: Acme.Catalog_Proto and
// acme-catalog-proto are one project.
func NormalizePython(name string) string {
	return strings.ToLower(separators.ReplaceAllString(name, "-"))
}

// WheelDistribution is the escaped form used inside wheel filenames (PEP 427):
// the normalized name with underscores.
func WheelDistribution(name string) string {
	return strings.ReplaceAll(NormalizePython(name), "-", "_")
}

// NormalizeComposer lowercases a Composer package name. Composer treats
// vendor/name case-insensitively and rejects uppercase in new packages.
func NormalizeComposer(name string) string { return strings.ToLower(name) }

// NormalizeNPM lowercases an npm package name; npm has required lowercase
// names since 2017 and matches case-insensitively for the rest.
func NormalizeNPM(name string) string { return strings.ToLower(name) }
