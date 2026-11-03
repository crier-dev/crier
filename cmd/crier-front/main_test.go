package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

const testToken = "front-test-token-2b7f"

// newTestFront builds the proxy under test against a recording upstream.
func newTestFront(t *testing.T, token string) (front *httptest.Server, seenAuth *string, seenPath *string) {
	t.Helper()
	auth := new(string)
	path := new(string)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*auth = r.Header.Get("Authorization")
		*path = r.URL.Path
		if r.URL.RawQuery != "" {
			*path += "?" + r.URL.RawQuery
		}
		w.Header().Set("X-Upstream", "crier")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`+"\n")
	}))
	t.Cleanup(upstream.Close)

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	proxy, err := newProxy(target, token)
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	front = httptest.NewServer(proxy)
	t.Cleanup(front.Close)
	return front, auth, path
}

// The front must carry the credential for a browser that has none.
func TestFrontInjectsBearerWhenClientHasNone(t *testing.T) {
	front, seenAuth, seenPath := newTestFront(t, testToken)
	resp, err := http.Get(front.URL + "/chat")
	if err != nil {
		t.Fatalf("GET through front: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got, want := *seenAuth, "Bearer "+testToken; got != want {
		t.Fatalf("upstream Authorization = %q, want %q", got, want)
	}
	if *seenPath != "/chat" {
		t.Fatalf("upstream path = %q, want /chat", *seenPath)
	}
}

// A caller must not be able to present a different credential through the front.
func TestFrontReplacesClientSuppliedAuthorization(t *testing.T) {
	front, seenAuth, _ := newTestFront(t, testToken)
	req, err := http.NewRequest(http.MethodGet, front.URL+"/sessions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer attacker-supplied")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET through front: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got, want := *seenAuth, "Bearer "+testToken; got != want {
		t.Fatalf("upstream Authorization = %q, want the injected %q", got, want)
	}
}

// Method, body and query survive the hop: the front must be transparent apart
// from the credential.
func TestFrontPreservesRequestShape(t *testing.T) {
	front, seenAuth, seenPath := newTestFront(t, testToken)
	body := `{"title":"front probe","kind":"channel","created_by":{"agent":"alpha"}}`
	resp, err := http.Post(front.URL+"/sessions?x=1", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST through front: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.HasPrefix(*seenAuth, "Bearer ") {
		t.Fatalf("upstream Authorization = %q, want a bearer", *seenAuth)
	}
	if *seenPath != "/sessions?x=1" {
		t.Fatalf("upstream path = %q, want /sessions?x=1", *seenPath)
	}
}

// The token must never be handed back to the client in a response header.
func TestFrontNeverEchoesTheToken(t *testing.T) {
	front, _, _ := newTestFront(t, testToken)
	resp, err := http.Get(front.URL + "/chat")
	if err != nil {
		t.Fatalf("GET through front: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	for name, values := range resp.Header {
		for _, v := range values {
			if strings.Contains(v, testToken) {
				t.Fatalf("response header %s leaks the token: %q", name, v)
			}
		}
	}
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), testToken) {
		t.Fatal("response body leaks the token")
	}
}

// An unreachable upstream is reported as a named front error, not a hang.
func TestFrontReportsUpstreamFailure(t *testing.T) {
	target, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	proxy, err := newProxy(target, testToken)
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	front := httptest.NewServer(proxy)
	defer front.Close()

	resp, err := http.Get(front.URL + "/chat")
	if err != nil {
		t.Fatalf("GET through front: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "FRONT_UPSTREAM_UNAVAILABLE") {
		t.Fatalf("body = %q, want the named front error", string(b))
	}
}

// Refusing to start without a token is what keeps an auth-enabled relay from
// being fronted by an unauthenticated hop by accident.
func TestRunRefusesWithoutToken(t *testing.T) {
	_ = os.Unsetenv("CR_AUTH_TOKEN")
	err := run([]string{"-listen", "127.0.0.1:0", "-upstream", "http://127.0.0.1:8765"})
	if err == nil {
		t.Fatal("run() succeeded with no token; want refusal")
	}
	if !strings.Contains(err.Error(), "CR_AUTH_TOKEN") {
		t.Fatalf("error = %v, want it to name CR_AUTH_TOKEN", err)
	}
}

// A malformed upstream is refused rather than proxied somewhere unexpected.
func TestRunRefusesMalformedUpstream(t *testing.T) {
	t.Setenv("CR_AUTH_TOKEN", testToken)
	if err := run([]string{"-listen", "127.0.0.1:0", "-upstream", "not-a-url"}); err == nil {
		t.Fatal("run() accepted a malformed upstream")
	}
}
