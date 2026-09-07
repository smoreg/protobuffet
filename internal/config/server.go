package config

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Server is the buffetd configuration.
type Server struct {
	Listen string `yaml:"listen"`
	Data   string `yaml:"data"`
	// BaseURL is the externally reachable root, used to build download URLs
	// inside Composer and npm metadata. Without it those clients cannot fetch.
	BaseURL string `yaml:"base_url"`
	// PublicRead serves schemas and SDKs without a token. Publishing always
	// requires one.
	PublicRead bool `yaml:"public_read"`
	// BreakingCheck controls the compatibility gate. See BreakingMode.
	BreakingCheck BreakingMode `yaml:"breaking_check"`
	// BreakingPolicy decides whether a major version may break compatibility.
	BreakingPolicy BreakingPolicy `yaml:"breaking_policy"`
	// BufPath overrides the buf binary location.
	BufPath string  `yaml:"buf_path"`
	Tokens  []Token `yaml:"tokens"`
}

// BreakingMode says what happens when a push would break consumers, and what
// happens when the check itself cannot run.
type BreakingMode string

const (
	// BreakingOff disables the gate.
	BreakingOff BreakingMode = "off"
	// BreakingBestEffort checks when it can and publishes anyway when buf is
	// unavailable. Convenient, and it means a broken toolchain silently
	// removes the only protection consumers have.
	BreakingBestEffort BreakingMode = "best-effort"
	// BreakingRequired refuses to start without buf and refuses a publish it
	// could not verify. This is the default.
	BreakingRequired BreakingMode = "required"
)

// UnmarshalYAML accepts the historical boolean spelling: true means required,
// false means off.
func (m *BreakingMode) UnmarshalYAML(value *yaml.Node) error {
	var b bool
	if err := value.Decode(&b); err == nil {
		if b {
			*m = BreakingRequired
		} else {
			*m = BreakingOff
		}
		return nil
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("breaking_check: want off, best-effort or required")
	}
	switch BreakingMode(s) {
	case BreakingOff, BreakingBestEffort, BreakingRequired:
		*m = BreakingMode(s)
		return nil
	default:
		return fmt.Errorf("breaking_check %q: want off, best-effort or required", s)
	}
}

// Enabled reports whether the check runs at all.
func (m BreakingMode) Enabled() bool { return m != BreakingOff }

// BreakingPolicy says whether a major version bump may carry breaking changes.
type BreakingPolicy string

const (
	// PolicyStrict refuses breaking changes regardless of the version number.
	// Correct for protobuf, where a new major belongs in a new package
	// (catalog.v1 -> catalog.v2) and old consumers keep using the old one.
	PolicyStrict BreakingPolicy = "strict"
	// PolicyAllowMajor permits breaking changes when the major version
	// increases, for teams that version the module rather than the package.
	PolicyAllowMajor BreakingPolicy = "allow-major"
)

// Token is a credential for the API. Only the digest is stored.
type Token struct {
	Subject string   `yaml:"subject"`
	SHA256  string   `yaml:"sha256"`
	Scopes  []string `yaml:"scopes"`
}

// Can reports whether the token carries a scope.
func (t Token) Can(scope string) bool {
	for _, s := range t.Scopes {
		if s == scope || s == "admin" {
			return true
		}
	}
	return false
}

// DefaultServer returns the configuration used when no file is given.
func DefaultServer() Server {
	return Server{
		Listen: ":8080",
		Data:   "./data",
		// An empty base URL means "derive it from the request", which is what
		// makes the server work behind any hostname without configuration.
		// Set it explicitly when a proxy rewrites the Host header.
		BaseURL:        "",
		PublicRead:     true,
		BreakingCheck:  BreakingRequired,
		BreakingPolicy: PolicyStrict,
	}
}

// LoadServer reads a server config, applying defaults for missing fields.
// An empty path returns the defaults, so buffetd runs with no config at all.
func LoadServer(path string) (Server, error) {
	cfg := DefaultServer()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read %s: %w", path, err)
		}
		dec := yaml.NewDecoder(bytesReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	// BUFFET_ADMIN_TOKEN is the zero-config path: one admin credential from
	// the environment, which is what container deployments want.
	if t := os.Getenv("BUFFET_ADMIN_TOKEN"); t != "" {
		cfg.Tokens = append(cfg.Tokens, Token{
			Subject: "env-admin",
			SHA256:  HashToken(t),
			Scopes:  []string{"admin"},
		})
	}
	if v := os.Getenv("BUFFET_BASE_URL"); v != "" {
		cfg.BaseURL = v
	}
	if v := os.Getenv("BUFFET_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("BUFFET_DATA"); v != "" {
		cfg.Data = v
	}
	if v := os.Getenv("BUFFET_BREAKING_CHECK"); v != "" {
		var mode BreakingMode
		if err := mode.UnmarshalYAML(&yaml.Node{Kind: yaml.ScalarNode, Value: v}); err != nil {
			return cfg, err
		}
		cfg.BreakingCheck = mode
	}
	if cfg.BreakingCheck == "" {
		cfg.BreakingCheck = BreakingRequired
	}
	switch cfg.BreakingPolicy {
	case "":
		cfg.BreakingPolicy = PolicyStrict
	case PolicyStrict, PolicyAllowMajor:
	default:
		return cfg, fmt.Errorf("breaking_policy %q: want strict or allow-major", cfg.BreakingPolicy)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if len(cfg.Tokens) == 0 {
		return cfg, fmt.Errorf("no tokens configured: set BUFFET_ADMIN_TOKEN or list tokens in the config")
	}
	if err := cfg.validateTokens(); err != nil {
		return cfg, err
	}
	if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
		return cfg, fmt.Errorf("listen %q: %w", cfg.Listen, err)
	}
	if cfg.BaseURL != "" {
		u, err := url.Parse(cfg.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return cfg, fmt.Errorf("base_url %q: want an absolute URL such as https://schemas.example.com", cfg.BaseURL)
		}
		// Every route is mounted at the root, and package metadata is built
		// from base_url; a path prefix here would produce URLs nothing serves.
		if u.Path != "" {
			return cfg, fmt.Errorf("base_url %q: a path prefix is not supported, use the host only", cfg.BaseURL)
		}
	}
	return cfg, nil
}

var validScopes = map[string]bool{"read": true, "write": true, "admin": true}

func (s Server) validateTokens() error {
	seen := map[string]bool{}
	for i, t := range s.Tokens {
		if t.Subject == "" {
			return fmt.Errorf("tokens[%d]: subject is required", i)
		}
		if seen[t.Subject] {
			return fmt.Errorf("tokens[%d]: duplicate subject %q", i, t.Subject)
		}
		seen[t.Subject] = true

		// A digest of the wrong shape never matches anything, which looks
		// exactly like a wrong token to whoever is debugging it.
		if len(t.SHA256) != 64 {
			return fmt.Errorf("tokens[%d] (%s): sha256 must be 64 hex characters, got %d", i, t.Subject, len(t.SHA256))
		}
		if _, err := hex.DecodeString(t.SHA256); err != nil {
			return fmt.Errorf("tokens[%d] (%s): sha256 is not hexadecimal", i, t.Subject)
		}
		if len(t.Scopes) == 0 {
			return fmt.Errorf("tokens[%d] (%s): at least one scope is required", i, t.Subject)
		}
		for _, scope := range t.Scopes {
			// A typo here silently produces a token that can do nothing.
			if !validScopes[scope] {
				return fmt.Errorf("tokens[%d] (%s): unknown scope %q, want read, write or admin", i, t.Subject, scope)
			}
		}
	}
	return nil
}

// HashToken returns the hex sha256 of a token. Tokens are high-entropy random
// strings, so a plain digest is the right primitive here: no salt to leak and
// no password-stretching cost on every request.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Authenticate matches a presented token against the configured ones in
// constant time.
func (s Server) Authenticate(token string) (Token, bool) {
	if token == "" {
		return Token{}, false
	}
	want := HashToken(token)
	for _, t := range s.Tokens {
		if subtle.ConstantTimeCompare([]byte(t.SHA256), []byte(want)) == 1 {
			return t, true
		}
	}
	return Token{}, false
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
