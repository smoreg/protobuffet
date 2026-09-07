// Package pack builds the archive formats the registry stores and serves:
// deterministic zips for schemas and Composer, a module zip for the Go proxy
// and a gzipped tarball for npm.
//
// Determinism matters: the schema archive digest is what identifies a schema
// revision, so packing the same files twice has to produce the same bytes.
package pack

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

// epoch is the fixed timestamp written into every archive entry.
var epoch = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// Filter decides whether a file found under a directory is packed.
type Filter func(relPath string) bool

// SkipHidden rejects dotfiles and anything below a dot directory, plus the
// usual local junk that has no business in a published artifact.
func SkipHidden(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
		if part == "vendor" || part == "node_modules" {
			return false
		}
	}
	return true
}

// Files walks dir and returns the relative paths of regular files, sorted.
func Files(dir string, keep Filter) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if keep != nil && !keep(rel) {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// Zip packs dir into a deterministic zip. Every entry is prefixed with prefix
// (pass "" for none), which is how both Composer dists and Go module zips
// expect their contents to be laid out.
func Zip(dir, prefix string, keep Filter) ([]byte, error) {
	files, err := Files(dir, keep)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("pack %s: no files matched", dir)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, rel := range files {
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		name := rel
		if prefix != "" {
			name = path.Join(prefix, rel)
		}
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:     name,
			Method:   zip.Deflate,
			Modified: epoch,
		})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(body); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// GoModuleZip packs dir as a Go module zip for modPath@version. The heavy
// lifting is x/mod/zip, which enforces every rule the go command checks on
// download, so a zip that builds here will not be rejected later.
func GoModuleZip(dir, modPath, version string) ([]byte, error) {
	var buf bytes.Buffer
	err := modzip.CreateFromDir(&buf, module.Version{Path: modPath, Version: version}, dir)
	if err != nil {
		return nil, fmt.Errorf("pack go module %s@%s: %w", modPath, version, err)
	}
	return buf.Bytes(), nil
}

// TarGz packs dir into a gzipped tarball with every entry under prefix. npm
// requires the "package/" prefix, which is why prefix is not optional here.
func TarGz(dir, prefix string, keep Filter) ([]byte, error) {
	if prefix == "" {
		return nil, fmt.Errorf("pack tarball: prefix is required")
	}
	files, err := Files(dir, keep)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("pack %s: no files matched", dir)
	}
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(gz)
	for _, rel := range files {
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		hdr := &tar.Header{
			Name:    path.Join(prefix, rel),
			Mode:    0o644,
			Size:    int64(len(body)),
			ModTime: epoch,
			Format:  tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ExtractZip unpacks a zip into dir, stripping the first path element when
// strip is true. Used to unpack schema archives for compatibility checks.
func ExtractZip(data []byte, dir string, strip bool) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		name := f.Name
		if strip {
			_, rest, ok := strings.Cut(name, "/")
			if !ok {
				continue
			}
			name = rest
		}
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		// Reject zip slip outright rather than silently rewriting the path:
		// an archive that tries to escape is not one to unpack halfway.
		if path.IsAbs(name) || name != path.Clean(name) || strings.HasPrefix(name, "../") {
			return fmt.Errorf("zip entry %q escapes destination", f.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) {
			return fmt.Errorf("zip entry %q escapes destination", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
