package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/smoreg/protobuffet/internal/config"
)

type ctxKey int

const (
	tokenKey     ctxKey = 1
	requestIDKey ctxKey = 2
)

// requestIDFrom returns the id assigned by the logging middleware.
func requestIDFrom(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey).(string)
	return id
}

// bearerToken extracts a credential from either an Authorization: Bearer
// header or HTTP basic auth. Basic is required because that is how the go
// command (via .netrc) and Composer send credentials.
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if v, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(v)
		}
	}
	if _, pass, ok := r.BasicAuth(); ok {
		return pass
	}
	return ""
}

// ReloadTokens swaps the credential set without dropping connections, so a
// rotation does not need a restart and a window of failed publishes.
func (s *Server) ReloadTokens() error {
	if s.cfg.TokensFile == "" {
		return fmt.Errorf("no tokens_file configured")
	}
	tokens, err := config.LoadTokens(s.cfg.TokensFile)
	if err != nil {
		return err
	}
	cfg := s.cfg
	cfg.Tokens = tokens
	if err := cfg.ValidateTokens(); err != nil {
		return err
	}
	s.tokens.Store(&tokens)
	s.log.Info("tokens reloaded", "count", len(tokens), "file", s.cfg.TokensFile)
	return nil
}

// authenticate matches a presented token against the live set.
func (s *Server) authenticate(token string) (config.Token, bool) {
	if tokens := s.tokens.Load(); tokens != nil {
		cfg := s.cfg
		cfg.Tokens = *tokens
		return cfg.Authenticate(token)
	}
	return s.cfg.Authenticate(token)
}

// requireScope wraps a handler with token authentication.
func (s *Server) requireScope(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if scope == "read" && s.cfg.PublicRead {
			next.ServeHTTP(w, r)
			return
		}
		tok, ok := s.authenticate(bearerToken(r))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="protobuffet", Bearer`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !tok.Can(scope) {
			http.Error(w, "forbidden: missing scope "+scope, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKey, tok)))
	})
}

// tokenFrom returns the authenticated token, if any.
func tokenFrom(r *http.Request) (config.Token, bool) {
	t, ok := r.Context().Value(tokenKey).(config.Token)
	return t, ok
}
