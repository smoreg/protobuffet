package config

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
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
	// BreakingCheck refuses a push that breaks wire compatibility with the
	// latest published version. Requires the buf binary on PATH.
	BreakingCheck bool `yaml:"breaking_check"`
	// BufPath overrides the buf binary location.
	BufPath string  `yaml:"buf_path"`
	Tokens  []Token `yaml:"tokens"`
}

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
		BaseURL:       "",
		PublicRead:    true,
		BreakingCheck: true,
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
		if err := dec.Decode(&cfg); err != nil && err != io.EOF {
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
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if len(cfg.Tokens) == 0 {
		return cfg, fmt.Errorf("no tokens configured: set BUFFET_ADMIN_TOKEN or list tokens in the config")
	}
	for i, t := range cfg.Tokens {
		if t.SHA256 == "" {
			return cfg, fmt.Errorf("tokens[%d] (%s): sha256 is required", i, t.Subject)
		}
		if len(t.Scopes) == 0 {
			return cfg, fmt.Errorf("tokens[%d] (%s): at least one scope is required", i, t.Subject)
		}
	}
	return cfg, nil
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
