package webui

import (
	"crypto/subtle"
	"net/http"
)

// basicAuth wraps h with HTTP Basic Auth when user and pass are both non-empty.
// If either is empty the handler is returned as-is (trusted-LAN mode).
// Uses constant-time comparison to avoid timing attacks.
func basicAuth(h http.Handler, user, pass string) http.Handler {
	if user == "" || pass == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Lazarr"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}
