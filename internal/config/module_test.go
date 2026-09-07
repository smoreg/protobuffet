package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ModuleFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const validModule = `
version: v1
module: acme/catalog
proto: proto
targets:
  go:
    module: registry.example.com/gen/go/acme/catalog
    template: buf.gen.go.yaml
    out: gen/go
    require:
      - google.golang.org/protobuf v1.36.12
`

func TestLoadModuleAppliesDefaults(t *testing.T) {
	dir := writeConfig(t, validModule)
	m, err := LoadModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Module != "acme/catalog" || m.Proto != "proto" {
		t.Fatalf("module = %+v", m)
	}
	if m.Targets.Go.GoVersion != "1.21" {
		t.Errorf("go_version default = %q", m.Targets.Go.GoVersion)
	}
	if got := m.Path("gen/go"); got != filepath.Join(dir, "gen/go") {
		t.Errorf("Path = %q", got)
	}
	if got := m.Path("/abs/path"); got != "/abs/path" {
		t.Errorf("absolute Path was rewritten to %q", got)
	}
	// Loading by file path must work the same as by directory.
	if _, err := LoadModule(filepath.Join(dir, ModuleFile)); err != nil {
		t.Errorf("load by file: %v", err)
	}
}

func TestLoadModuleDefaultsPerTarget(t *testing.T) {
	dir := writeConfig(t, `
version: v1
module: acme/catalog
targets:
  composer:
    package: acme/catalog-proto
    template: buf.gen.php.yaml
    out: gen/php
  python:
    package: acme-catalog-proto
    template: buf.gen.py.yaml
    out: gen/python
`)
	m, err := LoadModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Proto != "." {
		t.Errorf("proto default = %q", m.Proto)
	}
	// Without an autoload rule Composer installs a package that autoloads
	// nothing, and the failure surfaces as a missing class at runtime.
	if m.Targets.Composer.Autoload == nil {
		t.Error("composer autoload default missing")
	}
	if m.Targets.Python.RequiresPython != ">=3.8" {
		t.Errorf("requires_python default = %q", m.Targets.Python.RequiresPython)
	}
}

func TestLoadModuleRejectsBadConfigs(t *testing.T) {
	cases := map[string]string{
		"wrong version":   "version: v2\nmodule: acme/catalog\ntargets:\n  go: {module: m, template: t, out: o}\n",
		"no module":       "version: v1\ntargets:\n  go: {module: m, template: t, out: o}\n",
		"no targets":      "version: v1\nmodule: acme/catalog\n",
		"go without out":  "version: v1\nmodule: acme/catalog\ntargets:\n  go: {module: m, template: t}\n",
		"composer no pkg": "version: v1\nmodule: acme/catalog\ntargets:\n  composer: {template: t, out: o}\n",
		"npm no template": "version: v1\nmodule: acme/catalog\ntargets:\n  npm: {package: p, out: o}\n",
		"python no out":   "version: v1\nmodule: acme/catalog\ntargets:\n  python: {package: p, template: t}\n",
		// A typo in a field name silently produced a zero value before
		// KnownFields was enabled.
		"unknown field": "version: v1\nmodule: acme/catalog\nprotos: proto\ntargets:\n  go: {module: m, template: t, out: o}\n",
		"broken yaml":   "version: v1\nmodule: [\n",
	}
	for name, body := range cases {
		if _, err := LoadModule(writeConfig(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadModule(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestLoadModuleErrorNamesTheFile(t *testing.T) {
	dir := writeConfig(t, "version: v1\nmodule: acme/catalog\n")
	_, err := LoadModule(dir)
	if err == nil || !strings.Contains(err.Error(), ModuleFile) {
		t.Errorf("err = %v, want it to name %s", err, ModuleFile)
	}
}

func TestValidateRejectsOverlappingOutputs(t *testing.T) {
	cases := map[string]string{
		"same directory": `
version: v1
module: acme/catalog
proto: proto
targets:
  go: {module: m, template: t, out: gen}
  python: {package: p, template: t, out: gen}
`,
		"nested directory": `
version: v1
module: acme/catalog
proto: proto
targets:
  go: {module: m, template: t, out: gen}
  composer: {package: acme/p, template: t, out: gen/php}
`,
		"inside proto": `
version: v1
module: acme/catalog
proto: proto
targets:
  go: {module: m, template: t, out: proto/gen}
`,
	}
	for name, body := range cases {
		if _, err := LoadModule(writeConfig(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Distinct directories are fine.
	ok := `
version: v1
module: acme/catalog
proto: proto
targets:
  go: {module: m, template: t, out: gen/go}
  python: {package: p, template: t, out: gen/python}
`
	if _, err := LoadModule(writeConfig(t, ok)); err != nil {
		t.Errorf("distinct outputs rejected: %v", err)
	}
}
