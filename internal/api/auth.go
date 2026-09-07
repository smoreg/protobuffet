package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/smoreg/protobuffet/internal/config"
)

type ctxKey int

const tokenKey ctxKey = 1

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

// requireScope wraps a handler with token authentication.
func (s *Server) requireScope(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if scope == "read" && s.cfg.PublicRead {
			next.ServeHTTP(w, r)
			return
		}
		tok, ok := s.cfg.Authenticate(bearerToken(r))
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
