package pack

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/smoreg/protobuffet/internal/pkgname"
)

// WheelMeta is the metadata that ends up in a wheel's dist-info directory.
type WheelMeta struct {
	Name           string
	Version        string
	Summary        string
	RequiresDist   []string
	RequiresPython string
	License        string
}

// NormalizePythonName applies PEP 503 normalization, which is what pip uses to
// match a requested package against an index. The registry tracks ownership
// under the same rule, so the two cannot disagree.
func NormalizePythonName(name string) string { return pkgname.NormalizePython(name) }

// wheelDistribution is the escaped name used inside wheel filenames (PEP 427):
// like the normalized name, but with underscores.
func wheelDistribution(name string) string { return pkgname.WheelDistribution(name) }

// WheelFilename returns the canonical file name for a pure-Python wheel.
func WheelFilename(name, version string) string {
	return fmt.Sprintf("%s-%s-py3-none-any.whl", wheelDistribution(name), version)
}

// Wheel packs dir into a pure-Python wheel (PEP 427). Generated protobuf code
// is architecture independent, so py3-none-any is always the right tag.
//
// A wheel is preferred over an sdist because installing one is just unzipping:
// no setuptools, no build step, no compiler on the consumer side.
func Wheel(dir string, meta WheelMeta, keep Filter) ([]byte, error) {
	if meta.Name == "" || meta.Version == "" {
		return nil, fmt.Errorf("wheel: name and version are required")
	}
	files, err := Files(dir, keep)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("pack %s: no files matched", dir)
	}

	distInfo := fmt.Sprintf("%s-%s.dist-info", wheelDistribution(meta.Name), meta.Version)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// RECORD lists every file with its digest; it is what pip verifies and
	// what uninstall later reads, so it has to cover the dist-info files too.
	var record []string
	add := func(name string, body []byte) error {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: epoch})
		if err != nil {
			return err
		}
		if _, err := w.Write(body); err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		digest := base64.RawURLEncoding.EncodeToString(sum[:])
		record = append(record, fmt.Sprintf("%s,sha256=%s,%d", name, digest, len(body)))
		return nil
	}

	for _, rel := range files {
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		if err := add(rel, body); err != nil {
			return nil, err
		}
	}

	if err := add(path.Join(distInfo, "METADATA"), []byte(wheelMetadata(meta))); err != nil {
		return nil, err
	}
	wheelFile := "Wheel-Version: 1.0\nGenerator: protobuffet\nRoot-Is-Purelib: true\nTag: py3-none-any\n"
	if err := add(path.Join(distInfo, "WHEEL"), []byte(wheelFile)); err != nil {
		return nil, err
	}

	sort.Strings(record)
	record = append(record, path.Join(distInfo, "RECORD")+",,")
	w, err := zw.CreateHeader(&zip.FileHeader{
		Name: path.Join(distInfo, "RECORD"), Method: zip.Deflate, Modified: epoch,
	})
	if err != nil {
		return nil, err
	}
	if _, err := w.Write([]byte(strings.Join(record, "\n") + "\n")); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func wheelMetadata(m WheelMeta) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Metadata-Version: 2.1\nName: %s\nVersion: %s\n", m.Name, m.Version)
	if m.Summary != "" {
		fmt.Fprintf(&sb, "Summary: %s\n", m.Summary)
	}
	if m.License != "" {
		fmt.Fprintf(&sb, "License: %s\n", m.License)
	}
	if m.RequiresPython != "" {
		fmt.Fprintf(&sb, "Requires-Python: %s\n", m.RequiresPython)
	}
	for _, r := range m.RequiresDist {
		fmt.Fprintf(&sb, "Requires-Dist: %s\n", r)
	}
	return sb.String()
}
