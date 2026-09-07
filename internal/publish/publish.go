// Package publish turns a schema directory into the artifacts a registry
// serves: the schema archive itself plus one SDK package per target.
package publish

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/smoreg/protobuffet/internal/buftool"
	"github.com/smoreg/protobuffet/internal/config"
	"github.com/smoreg/protobuffet/internal/pack"
	"github.com/smoreg/protobuffet/internal/registry"
)

// Build is the full set of files for one publish.
type Build struct {
	Module    string
	Version   string
	SchemaZip []byte
	Artifacts []registry.Artifact
	Files     map[string][]byte
}

// SchemaArchive packs the schema module: the buf configuration plus every
// .proto below the configured proto directory. Nothing else is included, so
// the digest changes only when the contract changes.
func SchemaArchive(m *config.Module) ([]byte, error) {
	proto := filepath.ToSlash(strings.TrimPrefix(filepath.Clean(m.Proto), "./"))
	keep := func(rel string) bool {
		if !pack.SkipHidden(rel) {
			return false
		}
		switch rel {
		case "buf.yaml", "buf.lock", "buf.work.yaml":
			return true
		}
		if proto == "." {
			return strings.HasSuffix(rel, ".proto")
		}
		return strings.HasPrefix(rel, proto+"/") && strings.HasSuffix(rel, ".proto")
	}
	return pack.Zip(m.Dir, "", keep)
}

// ResolveVersion picks the version for a publish: an explicit one wins, then
// an exact git tag on HEAD, and otherwise a pseudo-version derived from the
// schema digest so that every revision is addressable.
func ResolveVersion(explicit, dir, schemaDigest string, now time.Time) (string, error) {
	if explicit != "" {
		return registry.NormalizeVersion(explicit)
	}
	if tag, err := gitTag(dir); err == nil && tag != "" {
		return registry.NormalizeVersion(tag)
	}
	return registry.PseudoVersion(now, schemaDigest)
}

func gitTag(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "describe", "--tags", "--exact-match", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Options controls a build.
type Options struct {
	Version string
	Now     time.Time
	// SkipGenerate reuses whatever is already in the target output
	// directories, which keeps CI fast when generation ran in an earlier step.
	SkipGenerate bool
}

// Prepare generates code and packs every configured target.
func Prepare(ctx context.Context, buf *buftool.Buf, m *config.Module, opts Options) (*Build, error) {
	schema, err := SchemaArchive(m)
	if err != nil {
		return nil, fmt.Errorf("pack schema: %w", err)
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	version, err := ResolveVersion(opts.Version, m.Dir, registry.SHA256Hex(schema), now)
	if err != nil {
		return nil, err
	}

	b := &Build{
		Module:    m.Module,
		Version:   version,
		SchemaZip: schema,
		Files:     map[string][]byte{"schema.zip": schema},
	}

	if t := m.Targets.Go; t != nil {
		art, body, err := buildGo(ctx, buf, m, t, version, opts.SkipGenerate)
		if err != nil {
			return nil, fmt.Errorf("target go: %w", err)
		}
		b.Artifacts = append(b.Artifacts, art)
		b.Files[art.File] = body
	}
	if t := m.Targets.Composer; t != nil {
		art, body, err := buildComposer(ctx, buf, m, t, version, opts.SkipGenerate)
		if err != nil {
			return nil, fmt.Errorf("target composer: %w", err)
		}
		b.Artifacts = append(b.Artifacts, art)
		b.Files[art.File] = body
	}
	if t := m.Targets.NPM; t != nil {
		art, body, err := buildNPM(ctx, buf, m, t, version, opts.SkipGenerate)
		if err != nil {
			return nil, fmt.Errorf("target npm: %w", err)
		}
		b.Artifacts = append(b.Artifacts, art)
		b.Files[art.File] = body
	}
	if t := m.Targets.Python; t != nil {
		art, body, err := buildPython(ctx, buf, m, t, version, opts.SkipGenerate)
		if err != nil {
			return nil, fmt.Errorf("target python: %w", err)
		}
		b.Artifacts = append(b.Artifacts, art)
		b.Files[art.File] = body
	}
	return b, nil
}

func generate(ctx context.Context, buf *buftool.Buf, m *config.Module, template string, skip bool) error {
	if skip {
		return nil
	}
	if buf == nil {
		return fmt.Errorf("buf is required to generate code (use --skip-generate to publish pre-generated output)")
	}
	return buf.Generate(ctx, m.Dir, template)
}

func buildGo(ctx context.Context, buf *buftool.Buf, m *config.Module, t *config.GoTarget, version string, skip bool) (registry.Artifact, []byte, error) {
	if err := generate(ctx, buf, m, t.Template, skip); err != nil {
		return registry.Artifact{}, nil, err
	}
	out := m.Path(t.Out)
	if err := writeGoMod(out, t); err != nil {
		return registry.Artifact{}, nil, err
	}
	body, err := pack.GoModuleZip(out, t.Module, version)
	if err != nil {
		return registry.Artifact{}, nil, err
	}
	return registry.Artifact{
		Target:  registry.TargetGo,
		Name:    t.Module,
		Version: version,
		File:    "go.zip",
		SHA256:  registry.SHA256Hex(body),
		SHA1:    sha1hex(body),
		Meta:    map[string]any{"require": t.Require},
	}, body, nil
}

// writeGoMod generates the go.mod that ships inside the module zip. It is
// generated rather than hand-written because the module path and version have
// to agree with what the proxy serves.
func writeGoMod(dir string, t *config.GoTarget) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "module %s\n\ngo %s\n", t.Module, t.GoVersion)
	if len(t.Require) > 0 {
		sb.WriteString("\nrequire (\n")
		for _, r := range t.Require {
			fields := strings.Fields(r)
			if len(fields) != 2 {
				return fmt.Errorf("require %q: want \"path version\"", r)
			}
			fmt.Fprintf(&sb, "\t%s %s\n", fields[0], fields[1])
		}
		sb.WriteString(")\n")
	}
	return os.WriteFile(filepath.Join(dir, "go.mod"), []byte(sb.String()), 0o644)
}

func buildComposer(ctx context.Context, buf *buftool.Buf, m *config.Module, t *config.ComposerTarget, version string, skip bool) (registry.Artifact, []byte, error) {
	if err := generate(ctx, buf, m, t.Template, skip); err != nil {
		return registry.Artifact{}, nil, err
	}
	out := m.Path(t.Out)
	pkgVersion := registry.EcosystemVersion(version)

	composerJSON := map[string]any{
		"name":        t.Package,
		"description": fmt.Sprintf("Generated protobuf classes for %s", m.Module),
		"type":        "library",
		"autoload":    t.Autoload,
	}
	if len(t.Require) > 0 {
		composerJSON["require"] = t.Require
	}
	if t.License != "" {
		composerJSON["license"] = t.License
	}
	body, err := json.MarshalIndent(composerJSON, "", "    ")
	if err != nil {
		return registry.Artifact{}, nil, err
	}
	if err := os.WriteFile(filepath.Join(out, "composer.json"), append(body, '\n'), 0o644); err != nil {
		return registry.Artifact{}, nil, err
	}

	prefix := strings.ReplaceAll(t.Package, "/", "-") + "-" + pkgVersion
	zipBody, err := pack.Zip(out, prefix, pack.SkipHidden)
	if err != nil {
		return registry.Artifact{}, nil, err
	}
	meta := map[string]any{"autoload": t.Autoload}
	if len(t.Require) > 0 {
		meta["require"] = toAnyMap(t.Require)
	}
	if t.License != "" {
		meta["license"] = t.License
	}
	return registry.Artifact{
		Target:  registry.TargetComposer,
		Name:    t.Package,
		Version: pkgVersion,
		File:    "composer.zip",
		SHA256:  registry.SHA256Hex(zipBody),
		SHA1:    sha1hex(zipBody),
		Meta:    meta,
	}, zipBody, nil
}

func buildNPM(ctx context.Context, buf *buftool.Buf, m *config.Module, t *config.NPMTarget, version string, skip bool) (registry.Artifact, []byte, error) {
	if err := generate(ctx, buf, m, t.Template, skip); err != nil {
		return registry.Artifact{}, nil, err
	}
	out := m.Path(t.Out)
	pkgVersion := registry.EcosystemVersion(version)

	packageJSON := map[string]any{
		"name":        t.Package,
		"version":     pkgVersion,
		"description": fmt.Sprintf("Generated protobuf code for %s", m.Module),
	}
	if t.Type != "" {
		packageJSON["type"] = t.Type
	}
	if t.License != "" {
		packageJSON["license"] = t.License
	}
	if len(t.Dependencies) > 0 {
		packageJSON["dependencies"] = t.Dependencies
	}
	body, err := json.MarshalIndent(packageJSON, "", "  ")
	if err != nil {
		return registry.Artifact{}, nil, err
	}
	if err := os.WriteFile(filepath.Join(out, "package.json"), append(body, '\n'), 0o644); err != nil {
		return registry.Artifact{}, nil, err
	}

	tgz, err := pack.TarGz(out, "package", pack.SkipHidden)
	if err != nil {
		return registry.Artifact{}, nil, err
	}
	meta := map[string]any{}
	if len(t.Dependencies) > 0 {
		meta["dependencies"] = toAnyMap(t.Dependencies)
	}
	if t.Type != "" {
		meta["type"] = t.Type
	}
	if t.License != "" {
		meta["license"] = t.License
	}
	return registry.Artifact{
		Target:  registry.TargetNPM,
		Name:    t.Package,
		Version: pkgVersion,
		File:    "npm.tgz",
		SHA256:  registry.SHA256Hex(tgz),
		SHA1:    sha1hex(tgz),
		Meta:    meta,
	}, tgz, nil
}

func buildPython(ctx context.Context, buf *buftool.Buf, m *config.Module, t *config.PythonTarget, version string, skip bool) (registry.Artifact, []byte, error) {
	if err := generate(ctx, buf, m, t.Template, skip); err != nil {
		return registry.Artifact{}, nil, err
	}
	pkgVersion := registry.PythonVersion(version)
	wheel, err := pack.Wheel(m.Path(t.Out), pack.WheelMeta{
		Name:           t.Package,
		Version:        pkgVersion,
		Summary:        fmt.Sprintf("Generated protobuf code for %s", m.Module),
		RequiresDist:   t.Requires,
		RequiresPython: t.RequiresPython,
		License:        t.License,
	}, pack.SkipHidden)
	if err != nil {
		return registry.Artifact{}, nil, err
	}
	meta := map[string]any{"requires_python": t.RequiresPython}
	if len(t.Requires) > 0 {
		meta["requires"] = toAnySlice(t.Requires)
	}
	if t.License != "" {
		meta["license"] = t.License
	}
	return registry.Artifact{
		Target:  registry.TargetPython,
		Name:    t.Package,
		Version: pkgVersion,
		File:    "python.whl",
		SHA256:  registry.SHA256Hex(wheel),
		SHA1:    sha1hex(wheel),
		Meta:    meta,
	}, wheel, nil
}

func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// sha1hex is what Composer and npm use to verify a downloaded archive. It is
// an integrity check against corrupted transfers, not a security boundary:
// authenticity comes from serving the registry over TLS.
func sha1hex(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}
