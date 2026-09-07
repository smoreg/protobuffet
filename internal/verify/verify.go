// Package verify checks that an uploaded artifact is actually installable by
// the package manager it targets.
//
// The registry stores versions immutably, so a malformed artifact cannot be
// fixed after the fact: the version is burned and consumers get a broken
// download. Every check here exists because some package manager refuses the
// artifact without it.
package verify

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/smoreg/protobuffet/internal/pack"
	"github.com/smoreg/protobuffet/internal/registry"
)

// Artifact dispatches to the check for the artifact's ecosystem.
func Artifact(a registry.Artifact, body []byte) error {
	switch a.Target {
	case registry.TargetGo:
		return GoModuleZip(body, a.Name, a.Version)
	case registry.TargetPython:
		return Wheel(body, a.Name, a.Version)
	case registry.TargetComposer:
		return ComposerZip(body, a.Name)
	case registry.TargetNPM:
		return NPMTarball(body, a.Name, a.Version)
	default:
		return fmt.Errorf("unknown target %q", a.Target)
	}
}

// GoModuleZip checks the layout the go command requires: every path under
// <module>@<version>/, and a go.mod whose module directive matches. Without
// this, `go get` fails after download with a confusing mismatch error.
func GoModuleZip(body []byte, modPath, version string) error {
	if major := registry.MajorOf(version); major != "v0" && major != "v1" &&
		!strings.HasSuffix(modPath, "/"+major) {
		return fmt.Errorf("go module zip: %s publishes %s but does not end in /%s; "+
			"Go requires the major version in the import path from v2 on", modPath, version, major)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return fmt.Errorf("go module zip: %w", err)
	}
	prefix := modPath + "@" + version + "/"
	var gomod []byte
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, prefix) {
			return fmt.Errorf("go module zip: entry %q is not under %q", f.Name, prefix)
		}
		if f.Name == prefix+"go.mod" {
			gomod, err = readZipEntry(f)
			if err != nil {
				return fmt.Errorf("go module zip: read go.mod: %w", err)
			}
		}
	}
	if gomod == nil {
		return fmt.Errorf("go module zip: %sgo.mod is missing", prefix)
	}
	declared := modfile.ModulePath(gomod)
	if declared == "" {
		return fmt.Errorf("go module zip: go.mod has no module directive")
	}
	if declared != modPath {
		return fmt.Errorf("go module zip: go.mod declares module %q, artifact says %q", declared, modPath)
	}
	return nil
}

// Wheel checks the dist-info directory pip looks for, and that the metadata
// agrees with the artifact name and version.
func Wheel(body []byte, name, version string) error {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return fmt.Errorf("wheel: %w", err)
	}
	distInfo := strings.ReplaceAll(pack.NormalizePythonName(name), "-", "_") + "-" + version + ".dist-info"
	found := map[string][]byte{}
	for _, f := range zr.File {
		dir, file := path.Split(f.Name)
		if strings.TrimSuffix(dir, "/") != distInfo {
			continue
		}
		switch file {
		case "METADATA", "WHEEL", "RECORD":
			body, err := readZipEntry(f)
			if err != nil {
				return fmt.Errorf("wheel: read %s: %w", f.Name, err)
			}
			found[file] = body
		}
	}
	for _, want := range []string{"METADATA", "WHEEL", "RECORD"} {
		if _, ok := found[want]; !ok {
			return fmt.Errorf("wheel: %s/%s is missing", distInfo, want)
		}
	}
	metadata := string(found["METADATA"])
	if !strings.Contains(metadata, "Name: "+name+"\n") {
		return fmt.Errorf("wheel: METADATA does not declare Name: %s", name)
	}
	if !strings.Contains(metadata, "Version: "+version+"\n") {
		return fmt.Errorf("wheel: METADATA does not declare Version: %s", version)
	}
	return nil
}

// ComposerZip checks that the archive has a single root directory, which is
// what Composer strips when installing, and a composer.json naming the package.
func ComposerZip(body []byte, name string) error {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return fmt.Errorf("composer zip: %w", err)
	}
	roots := map[string]bool{}
	var manifest []byte
	for _, f := range zr.File {
		root, rest, ok := strings.Cut(f.Name, "/")
		if !ok {
			return fmt.Errorf("composer zip: entry %q is not inside a root directory", f.Name)
		}
		roots[root] = true
		if rest == "composer.json" {
			manifest, err = readZipEntry(f)
			if err != nil {
				return fmt.Errorf("composer zip: read composer.json: %w", err)
			}
		}
	}
	if len(roots) != 1 {
		return fmt.Errorf("composer zip: want exactly one root directory, got %d", len(roots))
	}
	if manifest == nil {
		return fmt.Errorf("composer zip: composer.json is missing")
	}
	var doc struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(manifest, &doc); err != nil {
		return fmt.Errorf("composer zip: composer.json: %w", err)
	}
	if doc.Name != name {
		return fmt.Errorf("composer zip: composer.json declares %q, artifact says %q", doc.Name, name)
	}
	return nil
}

// NPMTarball checks the package/ prefix npm requires and the package.json
// identity.
func NPMTarball(body []byte, name, version string) error {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("npm tarball: %w", err)
	}
	defer gz.Close() //nolint:errcheck // read-only reader

	tr := tar.NewReader(gz)
	var manifest []byte
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("npm tarball: %w", err)
		}
		if !strings.HasPrefix(h.Name, "package/") {
			return fmt.Errorf("npm tarball: entry %q is not under package/", h.Name)
		}
		if h.Name == "package/package.json" {
			manifest, err = io.ReadAll(io.LimitReader(tr, 1<<20))
			if err != nil {
				return fmt.Errorf("npm tarball: read package.json: %w", err)
			}
		}
	}
	if manifest == nil {
		return fmt.Errorf("npm tarball: package/package.json is missing")
	}
	var doc struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(manifest, &doc); err != nil {
		return fmt.Errorf("npm tarball: package.json: %w", err)
	}
	if doc.Name != name || doc.Version != version {
		return fmt.Errorf("npm tarball: package.json declares %s@%s, artifact says %s@%s",
			doc.Name, doc.Version, name, version)
	}
	return nil
}

func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 4<<20))
}
