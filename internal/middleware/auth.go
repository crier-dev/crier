package middleware

import (
	"log/slog"
	"net/http"
	"strings"
)

// Auth returns middleware that validates Bearer tokens against the configured
// shared secret. When authToken is empty, authentication is disabled and all
// requests pass through (development mode). When set, requests missing the
// Authorization header or bearing an incorrect token receive 401.
//
// The /health endpoint is always exempt from authentication.
func Auth(authToken string) func(http.Handler) http.Handler {
	if authToken == "" {
		// No auth configured — pass through all requests.
		slog.Warn("auth disabled, all requests pass through (development mode)")
		return func(next http.Handler) http.Handler {
			return next
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health check is always public.
			if r.URL.Path == "/health" {
				next.ServeHTTP(w, r)
				return
			}

			header := r.Header.Get("Authorization")
			if header == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"missing Authorization header"}`))
				return
			}

			if !strings.HasPrefix(header, "Bearer ") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"authorization scheme must be Bearer"}`))
				return
			}

			token := strings.TrimPrefix(header, "Bearer ")
			if token != authToken {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"invalid token"}`))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
