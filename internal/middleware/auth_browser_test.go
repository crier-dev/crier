package middleware

// DF-CRIER-444 — the browser-detection arm is pure header logic, unit-tested
// here without the real server (the end-to-end browser/page contract lives in
// cmd/server/auth_browser_refusal_test.go). The three arms pin, respectively:
//
//   wantsHTMLRefusal: Accept "text/html…" → page; no Accept, "application/json",
//   "*/*" and any non-HTML Accept → bare JSON, byte-identical to the
//   pre-change middleware;
//
//   refuses: the page arm fires ONLY on the missing-header refusal — a wrong
//   scheme or a wrong token keeps the exact JSON bodies every API client
//   asserts (accept/refuse decision untouched; exempt list untouched);
//
//   disabled: a nil page (or any hook state) reproduces the old middleware
//   exactly, so every other consumer and test is byte-identical (AC 4).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// df444Middleware wraps a next handler that must never be reached in a
// refusal arm.
func df444Middleware(t *testing.T, token, page string, withPage bool) http.Handler {
	t.Helper()
	if withPage {
		h := df444BrowserAuth(t, token, page)
		return h(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("next handler reached in a refusal arm")
		}))
	}
	return Auth(token)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("next handler reached in a refusal arm")
	}))
}

// df444BrowserAuth returns the middleware built the way main.go builds it:
// arm the page on /chat, then wrap.
func df444BrowserAuth(t *testing.T, token, page string) func(http.Handler) http.Handler {
	t.Helper()
	SetBrowserRefusalPage("/chat", page)
	t.Cleanup(func() { SetBrowserRefusalPage("", "") })
	return Auth(token)
}

// TestBrowserWantsHTMLRefusalArms pins the Accept-header decision table.
func TestBrowserWantsHTMLRefusalArms(t *testing.T) {
	const page = "<!doctype html><html><body>REFUSAL-PAGE-SENTINEL</body></html>"
	handler := df444Middleware(t, "secret-token", page, true)

	cases := []struct {
		name     string
		accept   string // "" sends no Accept header at all
		wantHTML bool
	}{
		{"real browser navigation", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", true},
		{"explicit text/html only", "text/html", true},
		{"html with q-weighting", "text/html;q=0.9", true},
		{"agent curl, no accept header", "", false},
		{"json accept (the page's own fetch calls)", "application/json", false},
		{"wildcard only", "*/*", false},
		{"event-stream (sse clients)", "text/event-stream", false},
		{"plain text", "text/plain", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/chat", nil)
			if tc.accept != "" {
				request.Header.Set("Accept", tc.accept)
			}
			handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (legibility never becomes a new verdict)", recorder.Code)
			}
			ct := recorder.Header().Get("Content-Type")
			body := recorder.Body.String()
			if tc.wantHTML {
				if !strings.HasPrefix(ct, "text/html") {
					t.Fatalf("Content-Type = %q, want text/html (body: %s)", ct, body)
				}
				if !strings.Contains(body, "REFUSAL-PAGE-SENTINEL") {
					t.Fatalf("body is not the installed refusal page: %s", body)
				}
			} else {
				if ct != "application/json" {
					t.Fatalf("Content-Type = %q, want application/json (body: %s)", ct, body)
				}
				var parsed map[string]string
				if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
					t.Fatalf("body is not JSON: %v (%s)", err, body)
				}
				if parsed["error"] != "missing Authorization header" {
					t.Fatalf("error = %q, want the unchanged middleware wording", parsed["error"])
				}
				if strings.Contains(body, "REFUSAL-PAGE-SENTINEL") {
					t.Fatal("bare-JSON arm must never carry the refusal page")
				}
			}
		})
	}
}

// TestPageArmFiresOnlyOnMissingHeader pins scope: only the missing-header
// refusal on a browser Accept is legible, and only on the armed path;
// wrong-scheme and wrong-token refusals keep their exact JSON bodies (the
// accept/refuse decision and the API contract are untouched — NOT IN SCOPE).
func TestPageArmFiresOnlyOnMissingHeader(t *testing.T) {
	const page = "REFUSAL-PAGE-SENTINEL"
	handler := df444Middleware(t, "secret-token", page, true)

	// Wrong scheme on a browser request → JSON (the middleware's own wording).
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/chat", nil)
	request.Header.Set("Accept", "text/html")
	request.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized ||
		recorder.Header().Get("Content-Type") != "application/json" ||
		!strings.Contains(recorder.Body.String(), "authorization scheme must be Bearer") {
		t.Fatalf("wrong-scheme browser request = %d %s %s, want unchanged JSON 401",
			recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}

	// Wrong token on a browser request → JSON "invalid token".
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/chat", nil)
	request.Header.Set("Accept", "text/html")
	request.Header.Set("Authorization", "Bearer wrong-token")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized ||
		recorder.Header().Get("Content-Type") != "application/json" ||
		!strings.Contains(recorder.Body.String(), "invalid token") {
		t.Fatalf("wrong-token browser request = %d %s %s, want unchanged JSON 401",
			recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}

	// Correct token still passes with a browser Accept header — the page arm
	// must never intercept an authorized request.
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/chat", nil)
	request.Header.Set("Accept", "text/html")
	request.Header.Set("Authorization", "Bearer secret-token")
	var reached bool
	authorized := df444BrowserAuth(t, "secret-token", page)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	authorized.ServeHTTP(recorder, request)
	if !reached || recorder.Code != http.StatusOK {
		t.Fatalf("authorized browser request = reached %v status %d, want the handler reached with 200", reached, recorder.Code)
	}
}

// TestPageArmServesOnlyOnTheArmedPath pins the path scope: the page is bound
// to /chat by the server; an API path with a browser Accept header keeps the
// bare JSON refusal.
func TestPageArmServesOnlyOnTheArmedPath(t *testing.T) {
	const page = "REFUSAL-PAGE-SENTINEL"
	handler := df444Middleware(t, "secret-token", page, true)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/agents", nil)
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized ||
		recorder.Header().Get("Content-Type") != "application/json" ||
		!strings.Contains(recorder.Body.String(), "missing Authorization header") {
		t.Fatalf("browser Accept on /agents = %d %s %s, want the unchanged JSON refusal",
			recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), page) {
		t.Fatal("API path must never serve the refusal page")
	}
}

// TestNilPageReproducesPreChangeMiddleware pins AC 4: with no page installed
// (the default for every other consumer and every existing test), the
// middleware answers the byte-identical JSON 401 on a browser Accept header.
func TestNilPageReproducesPreChangeMiddleware(t *testing.T) {
	handler := df444Middleware(t, "secret-token", "", false)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/chat", nil)
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if ct := recorder.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if body := recorder.Body.String(); body != `{"error":"missing Authorization header"}` {
		t.Fatalf("body = %q, want the byte-identical pre-change JSON", body)
	}
}
