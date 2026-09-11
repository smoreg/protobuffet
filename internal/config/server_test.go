package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeServerConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "buffetd.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadServerFromEnvironmentAlone(t *testing.T) {
	t.Setenv("BUFFET_ADMIN_TOKEN", "secret-token")
	cfg, err := LoadServer("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Tokens) != 1 || cfg.Tokens[0].Subject != "env-admin" {
		t.Fatalf("tokens = %+v", cfg.Tokens)
	}
	tok, ok := cfg.Authenticate("secret-token")
	if !ok || !tok.Can("write") || !tok.Can("admin") {
		t.Errorf("admin token cannot write: %+v %v", tok, ok)
	}
	if _, ok := cfg.Authenticate("wrong"); ok {
		t.Error("wrong token accepted")
	}
	if _, ok := cfg.Authenticate(""); ok {
		t.Error("empty token accepted")
	}
	// A registry that cannot be published to is a mistake, not a mode.
	os.Unsetenv("BUFFET_ADMIN_TOKEN")
	if _, err := LoadServer(""); err == nil {
		t.Error("config without tokens accepted")
	}
}

func TestLoadServerEnvironmentOverrides(t *testing.T) {
	path := writeServerConfig(t, `
listen: ":9000"
data: /var/data
base_url: https://from-file.example
public_read: false
breaking_check: best-effort
tokens:
  - subject: ci
    sha256: `+HashToken("ci-token")+`
    scopes: [write]
`)
	t.Setenv("BUFFET_BASE_URL", "https://from-env.example")
	t.Setenv("BUFFET_LISTEN", ":9100")
	t.Setenv("BUFFET_DATA", "/env/data")
	t.Setenv("BUFFET_BREAKING_CHECK", "required")

	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "https://from-env.example" || cfg.Listen != ":9100" || cfg.Data != "/env/data" {
		t.Errorf("env overrides not applied: %+v", cfg)
	}
	if cfg.BreakingCheck != BreakingRequired {
		t.Errorf("breaking_check = %q", cfg.BreakingCheck)
	}
	if cfg.PublicRead {
		t.Error("public_read from file was lost")
	}
}

func TestLoadServerRejectsBadConfigs(t *testing.T) {
	digest := HashToken("t")
	cases := map[string]string{
		"short digest":    "tokens:\n  - {subject: a, sha256: abc, scopes: [read]}\n",
		"non-hex digest":  "tokens:\n  - {subject: a, sha256: " + strings.Repeat("z", 64) + ", scopes: [read]}\n",
		"no scopes":       "tokens:\n  - {subject: a, sha256: " + digest + ", scopes: []}\n",
		"unknown scope":   "tokens:\n  - {subject: a, sha256: " + digest + ", scopes: [wirte]}\n",
		"no subject":      "tokens:\n  - {sha256: " + digest + ", scopes: [read]}\n",
		"duplicate":       "tokens:\n  - {subject: a, sha256: " + digest + ", scopes: [read]}\n  - {subject: a, sha256: " + digest + ", scopes: [write]}\n",
		"bad listen":      "listen: nonsense\ntokens:\n  - {subject: a, sha256: " + digest + ", scopes: [read]}\n",
		"relative base":   "base_url: not-a-url\ntokens:\n  - {subject: a, sha256: " + digest + ", scopes: [read]}\n",
		"base with path":  "base_url: https://x.example/registry\ntokens:\n  - {subject: a, sha256: " + digest + ", scopes: [read]}\n",
		"bad policy":      "breaking_policy: whatever\ntokens:\n  - {subject: a, sha256: " + digest + ", scopes: [read]}\n",
		"bad breaking":    "breaking_check: sometimes\ntokens:\n  - {subject: a, sha256: " + digest + ", scopes: [read]}\n",
		"unknown top key": "listn: ':8080'\ntokens:\n  - {subject: a, sha256: " + digest + ", scopes: [read]}\n",
	}
	for name, body := range cases {
		if _, err := LoadServer(writeServerConfig(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadServer(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("missing config file accepted")
	}
}

func TestTokenScopes(t *testing.T) {
	read := Token{Scopes: []string{"read"}}
	write := Token{Scopes: []string{"write"}}
	admin := Token{Scopes: []string{"admin"}}

	if read.Can("write") || read.Can("admin") {
		t.Error("read token has write or admin")
	}
	if !read.Can("read") {
		t.Error("read token cannot read")
	}
	// Publishing without being able to read your own module would surprise
	// everyone, and the distinction protects nothing.
	if !write.Can("write") || !write.Can("read") || write.Can("admin") {
		t.Error("write token scopes wrong")
	}
	// admin implies everything, which is what the API relies on.
	if !admin.Can("read") || !admin.Can("write") || !admin.Can("admin") {
		t.Error("admin does not imply the other scopes")
	}
}

func TestBreakingModeHelpers(t *testing.T) {
	if BreakingOff.Enabled() {
		t.Error("off is enabled")
	}
	if !BreakingBestEffort.Enabled() || !BreakingRequired.Enabled() {
		t.Error("enabled modes report disabled")
	}
}

func TestTokenModuleScopes(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		module   string
		want     bool
	}{
		{"empty means every module", nil, "acme/catalog", true},
		{"explicit wildcard", []string{"*"}, "acme/catalog", true},
		{"owner wildcard", []string{"team1/*"}, "team1/catalog", true},
		{"owner wildcard does not cross owners", []string{"team1/*"}, "team2/catalog", false},
		{"exact module", []string{"team1/catalog"}, "team1/catalog", true},
		{"exact module is exact", []string{"team1/catalog"}, "team1/orders", false},
		{"several patterns", []string{"team1/*", "shared/common"}, "shared/common", true},
		{"name wildcard across owners", []string{"*/catalog"}, "team9/catalog", true},
		{"no pattern matches", []string{"team1/*"}, "acme/catalog", false},
	}
	for _, tc := range cases {
		tok := Token{Subject: "t", Modules: tc.patterns}
		if got := tok.AllowsModule(tc.module); got != tc.want {
			t.Errorf("%s: AllowsModule(%q) with %v = %v, want %v", tc.name, tc.module, tc.patterns, got, tc.want)
		}
	}
}

func TestLoadServerRejectsMalformedModulePatterns(t *testing.T) {
	digest := HashToken("t")
	for name, pattern := range map[string]string{
		"no slash":    "team1",
		"two slashes": "team1/sub/thing",
		"bad glob":    "team1/[",
	} {
		body := "tokens:\n  - {subject: a, sha256: " + digest + ", scopes: [write], modules: [\"" + pattern + "\"]}\n"
		if _, err := LoadServer(writeServerConfig(t, body)); err == nil {
			t.Errorf("%s: pattern %q accepted", name, pattern)
		}
	}
	ok := "tokens:\n  - {subject: a, sha256: " + digest + ", scopes: [write], modules: [\"team1/*\", \"*\"]}\n"
	if _, err := LoadServer(writeServerConfig(t, ok)); err != nil {
		t.Errorf("valid patterns rejected: %v", err)
	}
}
