package server

import (
	"crypto/subtle"
	"net/http"
	"slices"
	"strings"
)

// CORS wraps next with CORS handling. allowedOrigins empty or containing
// "*" allows any origin; otherwise only listed origins are echoed back.
// OPTIONS preflights are answered here and never reach next (so auth is
// bypassed for preflight, per the CORS spec — preflights carry no creds).
func CORS(allowedOrigins []string, next http.Handler) http.Handler {
	allowAll := len(allowedOrigins) == 0 || slices.Contains(allowedOrigins, "*")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if allowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else {
				w.Header().Add("Vary", "Origin")
				if slices.Contains(allowedOrigins, origin) {
					w.Header().Set("Access-Control-Allow-Origin", origin)
				}
			}
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAPIKey gates next behind an API key. An empty key disables auth
// (next is returned unwrapped). The candidate is taken from X-API-Key, or
// failing that the Authorization header with a case-insensitive "Bearer "
// prefix stripped. Comparison is constant-time.
func RequireAPIKey(key string, next http.Handler) http.Handler {
	if key == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		candidate := r.Header.Get("X-API-Key")
		if candidate == "" {
			auth := r.Header.Get("Authorization")
			if len(auth) >= 7 && strings.EqualFold(auth[:7], "Bearer ") {
				candidate = auth[7:]
			}
		}
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(key)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"errors":[{"message":"invalid or missing API key","extensions":{"code":"UNAUTHENTICATED"}}]}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
