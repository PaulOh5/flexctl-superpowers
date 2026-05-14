package auth

import (
	"net/http"
	"slices"
)

// RequireSameOrigin returns a middleware that rejects state-changing requests
// (POST/PUT/PATCH/DELETE) when the Origin header does not match the allowed
// list. GET/HEAD/OPTIONS are always allowed. Requests without an Origin header
// (curl, flexctl agent, server-to-server) are also allowed — Origin is a
// browser-supplied hint, not an auth credential.
//
// Empty `allowed` slice → middleware is a no-op (dev/test mode).
func RequireSameOrigin(allowed []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(allowed) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !slices.Contains(allowed, origin) {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
