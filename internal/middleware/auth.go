package middleware

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
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
//
// Browser refusals (DF-CRIER-444): when SetBrowserRefusalPage has armed a
// page, a request with NO Authorization header whose Accept header advertises
// text/html (a human pointing a browser at the deployment) is answered with
// that page instead of the bare JSON — same 401, same Content-Length honesty,
// same accept/refuse decision. The page explains that the deployment needs a
// fronting arrangement (mechanism decision: DF-CRIER-333) because a browser
// cannot attach a Bearer header on navigation. No other refusal changes: a
// wrong scheme and a wrong token keep their exact JSON bodies, and a nil page
// (the default everywhere) reproduces the pre-DF-CRIER-444 bytes exactly.
func Auth(authToken string) func(http.Handler) http.Handler {
	return AuthTokens(authToken, "")
}

// browserRefusalPage holds the human-readable 401 page cmd/server installs at
// boot, and browserRefusalPath the single path it serves on (cmd/server binds
// the chat client page). They live in this package ONLY as inert bytes plus a
// path: the middleware has no import path into cmd/server (the page is
// authored in the embedded chat.html), so main.go hands it over through
// SetBrowserRefusalPage. An empty path means the feature is disarmed and every
// refusal is byte-identical to the pre-DF-CRIER-444 middleware.
var (
	browserRefusalPage atomic.Value // string
	browserRefusalPath atomic.Value // string
)

// SetBrowserRefusalPage arms (or, with an empty path, disarms) the
// human-readable 401 page served to browser navigations that carry no
// Authorization header. The page is served ONLY on the given path — the
// middleware stays route-agnostic; cmd/server binds it to the chat client
// page (DF-CRIER-444), so an API path answers the bare JSON even with a
// browser Accept header.
func SetBrowserRefusalPage(path, page string) {
	browserRefusalPath.Store(path)
	browserRefusalPage.Store(page)
}

// browserRefusal returns the armed page, its path, and whether one is armed.
func browserRefusal() (path, page string, armed bool) {
	path, _ = browserRefusalPath.Load().(string)
	page, _ = browserRefusalPage.Load().(string)
	return path, page, path != "" && page != ""
}

// wantsHTMLRefusal reports whether this request looks like a BROWSER
// NAVIGATION — an Accept header that advertises text/html. Deliberately
// narrow, so no API client changes what it reads:
//
//   - no Accept header at all (curl, every agent HTTP client) → false;
//   - "application/json" (the chat page's own fetch calls) → false;
//   - "*/*" alone (a client that accepts anything, i.e. not choosing HTML)
//     → false;
//   - "text/event-stream", "text/plain", any other non-HTML Accept → false;
//   - any Accept CONTAINING a "text/html" token (browsers send
//     "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
//     → true.
//
// The browser's own sub-request fetching (XHR/fetch from JS) prefers
// application/json or */* FIRST in its Accept list, so those calls keep
// reading the JSON refusal even from the same origin.
func wantsHTMLRefusal(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	for _, part := range strings.Split(accept, ",") {
		if strings.TrimSpace(strings.SplitN(part, ";", 2)[0]) == "text/html" {
			return true
		}
	}
	return false
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
//   - GET /status accepts either token so its viewer-specific management
//     posture can reflect the same admin credential as the read routes.
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
				// DF-CRIER-444: a browser navigation (Accept advertises
				// text/html) on the one armed path gets the human-readable
				// refusal page instead of the bare JSON — SAME status, same
				// Content-Type honesty discipline. Every other path, every
				// other refusal, and every request when no page is armed is
				// the pre-DF-CRIER-444 JSON, byte-identical.
				if path, page, armed := browserRefusal(); armed && r.URL.Path == path && wantsHTMLRefusal(r) {
					writeAuthHTML(w, http.StatusUnauthorized, page)
					return
				}
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
				if management || (r.Method == http.MethodGet && r.URL.Path == "/status") {
					// The read-only status projection uses this SAME request's
					// admin gate decision. No relay/session/registry reach granted.
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
	case "/principals", "/bindings", "/grants", "/trust/anchors", "/trust/delegations":
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

// writeAuthHTML writes the browser refusal page (DF-CRIER-444). The body is
// the exact bytes cmd/server extracted from the embedded chat.html at boot —
// this function never edits, wraps or re-encodes them.
func writeAuthHTML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
