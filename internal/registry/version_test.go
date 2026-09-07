package registry

import (
	"regexp"
	"testing"
	"time"
)

// goPseudoVersion is the shape the go command accepts; if this regexp and the
// generator ever disagree, `go mod download` fails with a confusing error.
var goPseudoVersion = regexp.MustCompile(`^v0\.0\.0-\d{14}-[0-9a-f]{12}$`)

func TestPseudoVersionMatchesGoFormat(t *testing.T) {
	v, err := PseudoVersion(time.Date(2026, 9, 7, 15, 4, 5, 0, time.UTC), "abcdef0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if v != "v0.0.0-20260907150405-abcdef012345" {
		t.Fatalf("PseudoVersion = %q", v)
	}
	if !goPseudoVersion.MatchString(v) {
		t.Fatalf("%q does not match the go pseudo-version format", v)
	}
	if !IsPseudo(v) {
		t.Error("IsPseudo said no")
	}
	// A hand-written tag must not be mistaken for a generated version: that
	// would hide it from @v/list and let retention delete it.
	for _, tag := range []string{"v0.0.0-alpha-1", "v0.0.0-rc-2", "v1.0.0-20260907150405-abcdef012345", "v0.0.0-20260907150405-XYZ012345678"} {
		if IsPseudo(tag) {
			t.Errorf("IsPseudo(%q) = true", tag)
		}
	}
	if _, err := PseudoVersion(time.Now(), "short"); err == nil {
		t.Error("a short digest should be rejected")
	}
}

func TestNormalizeVersion(t *testing.T) {
	for in, want := range map[string]string{"1.2.3": "v1.2.3", "v1.2.3": "v1.2.3", "v2.0.0-rc.1": "v2.0.0-rc.1"} {
		got, err := NormalizeVersion(in)
		if err != nil || got != want {
			t.Errorf("NormalizeVersion(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "latest", "v1", "1.2.3.4"} {
		if _, err := NormalizeVersion(bad); err == nil {
			t.Errorf("NormalizeVersion(%q) accepted an invalid version", bad)
		}
	}
	if got := EcosystemVersion("v1.2.3"); got != "1.2.3" {
		t.Errorf("EcosystemVersion = %q", got)
	}
}

func TestPythonVersionIsPEP440(t *testing.T) {
	cases := map[string]string{
		"v1.2.3":      "1.2.3",
		"v2.0.0-rc.1": "2.0.0rc1",
		// The digest goes into the PEP 440 local segment, so two revisions in
		// the same second do not produce one wheel name.
		"v0.0.0-20260907150405-abcdef012345": "0.0.0.dev20260907150405+abcdef012345",
	}
	for in, want := range cases {
		if got := PythonVersion(in); got != want {
			t.Errorf("PythonVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestComposerVersion(t *testing.T) {
	cases := map[string]string{
		"v1.2.3":                             "1.2.3",
		"v2.0.0-rc.1":                        "2.0.0-RC1", // Composer wants RC1, not rc.1
		"v2.0.0-rc1":                         "2.0.0-RC1",
		"v1.0.0-beta.2":                      "1.0.0-beta2",
		"v1.0.0-alpha":                       "1.0.0-alpha1",
		"v0.0.0-20260907150405-abcdef012345": "0.0.0-dev.20260907150405.abcdef012345",
		// An unrecognised suffix stays verbatim rather than being guessed at.
		"v1.0.0-nightly": "1.0.0-nightly",
	}
	for in, want := range cases {
		if got := ComposerVersion(in); got != want {
			t.Errorf("ComposerVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
