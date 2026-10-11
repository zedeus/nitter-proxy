package main

import (
	"net/http"
	"strings"
)

// authMiddleware requires a /{token}/ path prefix on every request.
// Matching requests have the prefix stripped; everything else gets 401.
func authMiddleware(token string, next http.Handler) http.Handler {
	prefix := "/" + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			r.URL.Path = strings.TrimPrefix(p, prefix)
			if r.URL.Path == "" {
				r.URL.Path = "/"
			}
			r.URL.RawPath = ""
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	})
}
