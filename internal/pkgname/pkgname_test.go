package pkgname

import "testing"

// These rules decide whether two published names are the same package. The
// registry tracks ownership under them, so a mistake here lets one module
// shadow another on a shared index page.
func TestNormalizePython(t *testing.T) {
	// PEP 503: lowercase, and any run of - _ . collapses to a single dash.
	for in, want := range map[string]string{
		"acme-catalog-proto":   "acme-catalog-proto",
		"Acme.Catalog.Proto":   "acme-catalog-proto",
		"acme_catalog_proto":   "acme-catalog-proto",
		"ACME--CATALOG__PROTO": "acme-catalog-proto",
		"acme.-_catalog":       "acme-catalog",
		"single":               "single",
	} {
		if got := NormalizePython(in); got != want {
			t.Errorf("NormalizePython(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWheelDistribution(t *testing.T) {
	// PEP 427 file names use the normalized name with underscores.
	for in, want := range map[string]string{
		"acme-catalog-proto": "acme_catalog_proto",
		"Acme.Catalog":       "acme_catalog",
	} {
		if got := WheelDistribution(in); got != want {
			t.Errorf("WheelDistribution(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestComposerAndNPMAreCaseInsensitive(t *testing.T) {
	if NormalizeComposer("Acme/Catalog-Proto") != "acme/catalog-proto" {
		t.Error("composer name not lowercased")
	}
	if NormalizeNPM("@Acme/Catalog") != "@acme/catalog" {
		t.Error("npm name not lowercased")
	}
	// Unlike Python, separators are meaningful here: acme-catalog and
	// acme_catalog are different packages to both tools.
	if NormalizeComposer("acme/a-b") == NormalizeComposer("acme/a_b") {
		t.Error("composer names should not collapse separators")
	}
}
