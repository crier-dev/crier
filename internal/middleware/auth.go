package middleware

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
)

// Auth returns middleware that validates Bearer tokens against the configured
// shared secret. When authToken is empty, authentication is disabled and all
// requests pass through (development mode). When set, requests missing the
// Authorization header or bearing an incorrect token receive 401.
//
// /health, the build-identity endpoint (/version) and the OpenAPI spec
// endpoints (/openapi.json, /openapi.yaml, /docs) are always exempt from
// authentication: an operator must be able to ask a live server what it is and
// what it serves without holding a token.
//
// Auth is AuthTokens with no permissions-management token: presenting a single
// secret to every non-exempt path (DF-CRIER-297 added the second one).
func Auth(authToken string) func(http.Handler) http.Handler {
	return AuthTokens(authToken, "")
}

// AuthTokens is Auth plus the permissions-management admin token.
//
// The management surface (POST /principals, /bindings, /grants,
// /grants/{grantid}/revoke and /agents/{agentid}/class) is gated by a SECOND
// secret — CR_PERMISSIONS_ADMIN_TOKEN, checked by the handlers in constant time
// (registry.Handler.managementAuthorized). Before DF-CRIER-297 this middleware
// demanded CR_AUTH_TOKEN of every non-exempt path while those handlers demanded
// the admin token from the SAME header, so with both env vars set the surface
// was unreachable: one Authorization header cannot equal two secrets.
//
// The resolution keeps the handler's admin gate the sole authority for the
// writes and gives the middleware a per-surface rule:
//
//   - on a management path, EITHER secret is accepted. The message token is
//     passed straight through so the handler answers its named 403
//     MANAGEMENT_FORBIDDEN — a reach credential is not an admin credential —
//     and the admin token reaches the handler, which re-checks it.
//   - on every other path, only the message token is accepted. Presenting the
//     admin token there is answered 403 (the credential is recognized, it is
//     simply not a credential for that surface) rather than 401.
//
// An empty managementToken reproduces the pre-DF-CRIER-297 single-secret
// behaviour exactly, so an unconfigured deployment is byte-identical.
func AuthTokens(authToken, managementToken string) func(http.Handler) http.Handler {
	if authToken == "" {
		// No auth configured — pass through all requests. Development mode is
		// unchanged by DF-CRIER-297: the management writes still need the
		// admin token, and the handler is where that gate lives.
		slog.Warn("auth disabled, all requests pass through (development mode)")
		return func(next http.Handler) http.Handler {
			return next
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health check, build identity and OpenAPI spec endpoints are
			// always public so a token-less curl of the spec works
			// (CR-GAP-049) and an operator can ask a running server what
			// build it is (DF-CRIER-101).
			switch r.URL.Path {
			case "/health", "/version", "/openapi.json", "/openapi.yaml", "/docs":
				next.ServeHTTP(w, r)
				return
			}

			management := isManagementPath(r.URL.Path)

			header := r.Header.Get("Authorization")
			if header == "" {
				writeAuthJSON(w, http.StatusUnauthorized, `{"error":"missing Authorization header"}`)
				return
			}

			if !strings.HasPrefix(header, "Bearer ") {
				writeAuthJSON(w, http.StatusUnauthorized, `{"error":"authorization scheme must be Bearer"}`)
				return
			}

			token := strings.TrimPrefix(header, "Bearer ")

			// The message token is a reach credential for every path. On a
			// management path it is deliberately handed to the handler, whose
			// admin gate answers the named 403 MANAGEMENT_FORBIDDEN — the
			// refusal stays the endpoint's decision, never this middleware's.
			if subtle.ConstantTimeCompare([]byte(token), []byte(authToken)) == 1 {
				next.ServeHTTP(w, r)
				return
			}

			if managementToken != "" &&
				subtle.ConstantTimeCompare([]byte(token), []byte(managementToken)) == 1 {
				if management {
					next.ServeHTTP(w, r)
					return
				}
				// A recognized admin credential on a non-management path:
				// forbidden, not unauthorized.
				writeAuthJSON(w, http.StatusForbidden,
					`{"error":"the admin token is accepted on the permissions management surface only"}`)
				return
			}

			writeAuthJSON(w, http.StatusUnauthorized, `{"error":"invalid token"}`)
		})
	}
}

// isManagementPath reports whether path addresses the permissions management
// surface whose authority is the handler's admin gate (registry/principals_api.go).
// It is the one place the middleware knows about those routes; the route
// registrations live in cmd/server/main.go, and the end-to-end gate in
// cmd/server/principals_auth_test.go fails if the two ever drift apart.
func isManagementPath(path string) bool {
	switch path {
	case "/principals", "/bindings", "/grants":
		return true
	}
	// /grants/{grantid}/revoke and /agents/{agentid}/class are the two
	// parameterised routes: exactly three segments, the middle one the id mux
	// binds. Counting segments (instead of a prefix/suffix test) keeps a
	// traversing or over-long path from being exempted by accident.
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segs) != 3 || segs[1] == "" {
		return false
	}
	return (segs[0] == "grants" && segs[2] == "revoke") ||
		(segs[0] == "agents" && segs[2] == "class")
}

// writeAuthJSON writes one of the middleware's JSON refusals.
func writeAuthJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
