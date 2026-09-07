// Package config reads the two configuration files protobuffet uses: the
// module config that lives next to the schemas, and the server config.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ModuleFile is the default name of the module config.
const ModuleFile = "buffet.yaml"

// Module describes one schema module and the SDKs published from it.
type Module struct {
	Version string  `yaml:"version"`
	Module  string  `yaml:"module"`
	Proto   string  `yaml:"proto"`
	Targets Targets `yaml:"targets"`

	// Dir is the directory the config was loaded from, not part of the file.
	Dir string `yaml:"-"`
}

// Targets holds the per-ecosystem publishing settings. A nil target is simply
// not published.
type Targets struct {
	Go       *GoTarget       `yaml:"go"`
	Composer *ComposerTarget `yaml:"composer"`
	NPM      *NPMTarget      `yaml:"npm"`
	Python   *PythonTarget   `yaml:"python"`
}

// GoTarget publishes a Go module through the registry's module proxy.
type GoTarget struct {
	// Module is the import path consumers will use. It must match the
	// go_package prefix produced by the generation template.
	Module string `yaml:"module"`
	// Template is the buf.gen.yaml used to generate the code.
	Template string `yaml:"template"`
	// Out is the directory the template writes to, and the root of the module.
	Out string `yaml:"out"`
	// GoVersion goes into the generated go.mod; defaults to 1.21.
	GoVersion string `yaml:"go_version"`
	// Require lists dependencies as "path version" pairs.
	Require []string `yaml:"require"`
}

// ComposerTarget publishes a Composer package.
type ComposerTarget struct {
	Package  string            `yaml:"package"`
	Template string            `yaml:"template"`
	Out      string            `yaml:"out"`
	Require  map[string]string `yaml:"require"`
	// Autoload is copied into composer.json verbatim; defaults to a classmap
	// over the package root, which is what the protobuf PHP plugin needs.
	Autoload map[string]any `yaml:"autoload"`
	License  string         `yaml:"license"`
}

// NPMTarget publishes an npm package.
type NPMTarget struct {
	Package      string            `yaml:"package"`
	Template     string            `yaml:"template"`
	Out          string            `yaml:"out"`
	Dependencies map[string]string `yaml:"dependencies"`
	License      string            `yaml:"license"`
	Type         string            `yaml:"type"`
}

// PythonTarget publishes a wheel through the registry's PyPI index.
type PythonTarget struct {
	Package  string `yaml:"package"`
	Template string `yaml:"template"`
	Out      string `yaml:"out"`
	// Requires lists dependencies in PEP 508 form, e.g. "protobuf>=5.27".
	Requires       []string `yaml:"requires"`
	RequiresPython string   `yaml:"requires_python"`
	License        string   `yaml:"license"`
}

// LoadModule reads a module config. Path may be a directory or a file.
func LoadModule(path string) (*Module, error) {
	if path == "" {
		path = "."
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	file := path
	if st.IsDir() {
		file = filepath.Join(path, ModuleFile)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	var m Module
	dec := yaml.NewDecoder(bytesReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	m.Dir = filepath.Dir(file)
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &m, nil
}

func (m *Module) validate() error {
	if m.Version != "v1" {
		return fmt.Errorf("version %q: only v1 is supported", m.Version)
	}
	if m.Module == "" {
		return fmt.Errorf("module is required (owner/name)")
	}
	if m.Proto == "" {
		m.Proto = "."
	}
	if m.Targets.Go == nil && m.Targets.Composer == nil && m.Targets.NPM == nil && m.Targets.Python == nil {
		return fmt.Errorf("at least one target is required")
	}
	if t := m.Targets.Go; t != nil {
		if t.Module == "" || t.Out == "" || t.Template == "" {
			return fmt.Errorf("targets.go: module, template and out are required")
		}
		if t.GoVersion == "" {
			t.GoVersion = "1.21"
		}
	}
	if t := m.Targets.Composer; t != nil {
		if t.Package == "" || t.Out == "" || t.Template == "" {
			return fmt.Errorf("targets.composer: package, template and out are required")
		}
		if t.Autoload == nil {
			t.Autoload = map[string]any{"classmap": []any{"."}}
		}
	}
	if t := m.Targets.NPM; t != nil {
		if t.Package == "" || t.Out == "" || t.Template == "" {
			return fmt.Errorf("targets.npm: package, template and out are required")
		}
	}
	if t := m.Targets.Python; t != nil {
		if t.Package == "" || t.Out == "" || t.Template == "" {
			return fmt.Errorf("targets.python: package, template and out are required")
		}
		if t.RequiresPython == "" {
			t.RequiresPython = ">=3.8"
		}
	}
	return nil
}

// Path resolves a config-relative path against the config directory.
func (m *Module) Path(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(m.Dir, p)
}
