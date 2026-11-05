package hermesession

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGateway is an httptest stand-in for the Hermes gateway's three session
// routes. No real Hermes, no network: it is the contract this transport is
// written against — POST /api/sessions, GET /api/sessions/{id},
// POST /api/sessions/{id}/chat — including the gateway's bearer check.
type fakeGateway struct {
	t     *testing.T
	token string

	mu       sync.Mutex
	sessions map[string]*fakeSession
	created  int
	chats    []string
	lastChat map[string]any
	gorGet   int

	// forceStatus, when non-zero, overrides the answer of every route with
	// that status (used for the 401/403 and generic-rejection cells).
	forceStatus int
	// createDelay lets a test hold a create call open.
	createDelay time.Duration
}

func newFakeGateway(t *testing.T, token string) (*fakeGateway, *httptest.Server) {
	t.Helper()
	g := &fakeGateway{t: t, token: token, sessions: map[string]*fakeSession{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/sessions", g.handleCreate)
	mux.HandleFunc("/api/sessions/", g.handleSession)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return g, srv
}

func (g *fakeGateway) authOK(w http.ResponseWriter, r *http.Request) bool {
	if g.forceStatus != 0 {
		writeErr(w, g.forceStatus)
		return false
	}
	if strings.TrimSpace(g.token) == "" {
		return true
	}
	want := "Bearer " + g.token
	if r.Header.Get("Authorization") != want {
		writeErr(w, http.StatusUnauthorized)
		return false
	}
	return true
}

func writeErr(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": http.StatusText(status), "code": "gateway_refused"},
	})
}

func (g *fakeGateway) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !g.authOK(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed)
		return
	}
	if g.createDelay > 0 {
		time.Sleep(g.createDelay)
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	g.mu.Lock()
	id := "sess-" + strconv.Itoa(g.created+1)
	g.created++
	g.sessions[id] = &fakeSession{ID: id, Title: str(body["title"])}
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Hermes-Session-Id", id)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object":  "hermes.session",
		"session": map[string]any{"id": id, "source": str(body["source"]), "title": str(body["title"])},
	})
}

func (g *fakeGateway) handleSession(w http.ResponseWriter, r *http.Request) {
	if !g.authOK(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id := strings.SplitN(path, "/", 2)[0]
	if strings.HasSuffix(path, "/chat") {
		g.handleChat(w, r, id)
		return
	}
	g.mu.Lock()
	s, ok := g.sessions[id]
	g.gorGet++
	g.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "hermes.session", "session": s.row()})
}

func (g *fakeGateway) handleChat(w http.ResponseWriter, r *http.Request, id string) {
	g.mu.Lock()
	_, ok := g.sessions[id]
	g.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	g.mu.Lock()
	g.chats = append(g.chats, id)
	g.lastChat = body
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Hermes-Session-Id", id)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object":     "hermes.session.chat.completion",
		"status":     "ok",
		"session_id": id,
		"message":    map[string]any{"role": "assistant", "content": "echo: " + str(body["message"])},
	})
}

func (g *fakeGateway) counts() (created int, chats []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.created, append([]string(nil), g.chats...)
}

func (g *fakeGateway) setClosed(id, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[id]
	s.EndReason = reason
	s.EndedAt = 1720000000.5
}

func (g *fakeGateway) addSession(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessions[id] = &fakeSession{ID: id}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// fakeSession is the fake gateway's session row, in the SHAPE the real one
// answers with (ended_at is a unix number, end_reason a string).
type fakeSession struct {
	ID        string  `json:"id"`
	Title     string  `json:"title,omitempty"`
	Source    string  `json:"source,omitempty"`
	EndReason string  `json:"end_reason,omitempty"`
	EndedAt   float64 `json:"ended_at,omitempty"`
}

func (s *fakeSession) row() map[string]any {
	row := map[string]any{"id": s.ID, "source": "crier"}
	if s.Title != "" {
		row["title"] = s.Title
	}
	if s.EndReason != "" {
		row["end_reason"] = s.EndReason
	}
	if s.EndedAt != 0 {
		row["ended_at"] = s.EndedAt
	}
	return row
}

// tokenFile writes a 0600 credential file and returns its path.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod token file: %v", err)
	}
	return path
}

func cfgFor(srv *httptest.Server, token, policy string) *Config {
	return &Config{BaseURL: srv.URL, TokenFile: token, Policy: Policy(policy)}
}

func reqFor(messageID string) Request {
	return Request{AgentID: "agent-a", MessageID: messageID, Sender: "agent-b",
		Kind: "message", Body: []byte(`{"task":"summarise the log"}`)}
}

func errCode(t *testing.T, err error) *Failure {
	t.Helper()
	var f *Failure
	if !errors.As(err, &f) {
		t.Fatalf("error is not a *Failure: %v", err)
	}
	return f
}

// ----- happy paths: one per policy -----

func TestDeliverPolicyNewCreatesOneSessionPerMessage(t *testing.T) {
	g, srv := newFakeGateway(t, "s3cret")
	cfg := cfgFor(srv, tokenFile(t, "s3cret"), "new")
	client := NewClient(time.Second)

	first, err := client.Deliver(context.Background(), cfg, reqFor("msg-1"))
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	second, err := client.Deliver(context.Background(), cfg, reqFor("msg-2"))
	if err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if first.Replayed || second.Replayed {
		t.Fatalf("fresh deliveries must not report a replay: %+v %+v", first, second)
	}
	if first.SessionID == second.SessionID {
		t.Fatalf("policy new reused session %q for two messages", first.SessionID)
	}
	if first.CorrelationID != first.SessionID {
		t.Errorf("correlation id = %q, want the gateway's session id %q", first.CorrelationID, first.SessionID)
	}
	if first.Reply != `echo: {"task":"summarise the log"}` {
		t.Errorf("reply = %q, want the fake gateway's echo of the payload text", first.Reply)
	}
	created, chats := g.counts()
	if created != 2 || len(chats) != 2 {
		t.Fatalf("created=%d chats=%d, want 2 and 2", created, len(chats))
	}
	if rec, ok := client.Lookup("msg-1"); !ok || rec.SessionID != first.SessionID || rec.CorrelationID != first.CorrelationID {
		t.Fatalf("ledger does not record the correlation for msg-1: %+v ok=%v", rec, ok)
	}
}

func TestDeliverPolicyExistingAppendsToConfiguredSession(t *testing.T) {
	g, srv := newFakeGateway(t, "s3cret")
	g.addSession("carried-session")
	cfg := cfgFor(srv, tokenFile(t, "s3cret"), "existing")
	cfg.SessionID = "carried-session"
	client := NewClient(time.Second)

	first, err := client.Deliver(context.Background(), cfg, reqFor("msg-1"))
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	second, err := client.Deliver(context.Background(), cfg, reqFor("msg-2"))
	if err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if first.SessionID != "carried-session" || second.SessionID != "carried-session" {
		t.Fatalf("sessions = %q, %q, want the configured session both times", first.SessionID, second.SessionID)
	}
	created, chats := g.counts()
	if created != 0 {
		t.Fatalf("policy existing created %d session(s); it must create none", created)
	}
	if len(chats) != 2 {
		t.Fatalf("chats = %v, want two turns in the configured session", chats)
	}
}

func TestDeliverPolicyPerAgentCreatesOnceAndRecords(t *testing.T) {
	g, srv := newFakeGateway(t, "s3cret")
	cfg := cfgFor(srv, tokenFile(t, "s3cret"), "per-agent")
	client := NewClient(time.Second)

	first, err := client.Deliver(context.Background(), cfg, reqFor("msg-1"))
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	second, err := client.Deliver(context.Background(), cfg, reqFor("msg-2"))
	if err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if first.SessionID != second.SessionID {
		t.Fatalf("per-agent sessions differ: %q vs %q", first.SessionID, second.SessionID)
	}
	created, chats := g.counts()
	if created != 1 {
		t.Fatalf("per-agent created %d sessions, want exactly 1", created)
	}
	if len(chats) != 2 {
		t.Fatalf("chats = %v, want two turns in the one session", chats)
	}
	// A different agent gets its own session.
	g.mu.Lock()
	other := reqFor("msg-3")
	other.AgentID = "agent-c"
	g.mu.Unlock()
	third, err := client.Deliver(context.Background(), cfg, other)
	if err != nil {
		t.Fatalf("third delivery: %v", err)
	}
	if third.SessionID == first.SessionID {
		t.Fatalf("agent-c reused agent-a's session %q", third.SessionID)
	}
}

// ----- the four distinct named failures -----

func TestFailureGatewayUnreachable(t *testing.T) {
	// Reserve and immediately release a port so nothing is listening on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := &Config{BaseURL: "http://" + addr, TokenFile: tokenFile(t, "s3cret"), Policy: PolicyNew}
	_, err = NewClient(2*time.Second).Deliver(context.Background(), cfg, reqFor("msg-1"))
	f := errCode(t, err)
	if f.Code != CodeGatewayUnreachable || f.Status != http.StatusBadGateway {
		t.Fatalf("code/status = %s/%d, want %s/502", f.Code, f.Status, CodeGatewayUnreachable)
	}
	if f.GatewayStatus != 0 {
		t.Errorf("gateway status = %d, want 0 (no HTTP exchange happened)", f.GatewayStatus)
	}
}

func TestFailureGatewayUnauthorized(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		g, srv := newFakeGateway(t, "s3cret")
		g.forceStatus = status
		cfg := cfgFor(srv, tokenFile(t, "s3cret"), "new")
		_, err := NewClient(time.Second).Deliver(context.Background(), cfg, reqFor("msg-1"))
		f := errCode(t, err)
		if f.Code != CodeGatewayUnauthorized || f.Status != http.StatusBadGateway {
			t.Fatalf("%d: code/status = %s/%d, want %s/502", status, f.Code, f.Status, CodeGatewayUnauthorized)
		}
		if f.GatewayStatus != status {
			t.Fatalf("gateway status = %d, want %d verbatim", f.GatewayStatus, status)
		}
	}
}

func TestFailureSessionNotFound(t *testing.T) {
	_, srv := newFakeGateway(t, "s3cret")
	cfg := cfgFor(srv, tokenFile(t, "s3cret"), "existing")
	cfg.SessionID = "no-such-session"
	_, err := NewClient(time.Second).Deliver(context.Background(), cfg, reqFor("msg-1"))
	f := errCode(t, err)
	if f.Code != CodeSessionNotFound || f.Status != http.StatusNotFound {
		t.Fatalf("code/status = %s/%d, want %s/404", f.Code, f.Status, CodeSessionNotFound)
	}
	if f.GatewayStatus != http.StatusNotFound {
		t.Fatalf("gateway status = %d, want 404 verbatim", f.GatewayStatus)
	}
}

func TestFailureSessionClosed(t *testing.T) {
	g, srv := newFakeGateway(t, "s3cret")
	g.addSession("ended-session")
	g.setClosed("ended-session", "done")
	cfg := cfgFor(srv, tokenFile(t, "s3cret"), "existing")
	cfg.SessionID = "ended-session"
	_, err := NewClient(time.Second).Deliver(context.Background(), cfg, reqFor("msg-1"))
	f := errCode(t, err)
	if f.Code != CodeSessionClosed || f.Status != http.StatusConflict {
		t.Fatalf("code/status = %s/%d, want %s/409", f.Code, f.Status, CodeSessionClosed)
	}
	if f.GatewayStatus != http.StatusOK {
		t.Fatalf("gateway status = %d, want the 200 the GET answered", f.GatewayStatus)
	}
	if _, chats := g.counts(); len(chats) != 0 {
		t.Fatalf("a closed session was chatted into: %v", chats)
	}
}

// TestFailureCodesAreDistinct pins the requirement that the four conditions
// never collapse into one another.
func TestFailureCodesAreDistinct(t *testing.T) {
	codes := []string{CodeGatewayUnreachable, CodeGatewayUnauthorized, CodeSessionNotFound, CodeSessionClosed}
	seen := map[string]bool{}
	statuses := map[int]bool{}
	for _, c := range codes {
		if seen[c] {
			t.Fatalf("duplicate code %q", c)
		}
		seen[c] = true
		statuses[statusesOf(c)] = true
	}
	if !statuses[http.StatusBadGateway] || !statuses[http.StatusNotFound] || !statuses[http.StatusConflict] {
		t.Fatalf("the four failures do not span the documented statuses: %v", statuses)
	}
}

func statusesOf(code string) int { return statuses[code] }

// ----- idempotency: a replayed message id creates nothing -----

func TestReplaySameMessageIDCreatesNoSecondSession(t *testing.T) {
	g, srv := newFakeGateway(t, "s3cret")
	cfg := cfgFor(srv, tokenFile(t, "s3cret"), "new")
	client := NewClient(time.Second)

	first, err := client.Deliver(context.Background(), cfg, reqFor("msg-1"))
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	second, err := client.Deliver(context.Background(), cfg, reqFor("msg-1"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed {
		t.Fatal("the second delivery of the same message id is not reported as a replay")
	}
	if second.SessionID != first.SessionID || second.CorrelationID != first.CorrelationID {
		t.Fatalf("replay names a different session/correlation: %+v vs %+v", second, first)
	}
	if second.Reply != first.Reply {
		t.Fatalf("replay reply = %q, want the recorded %q", second.Reply, first.Reply)
	}
	created, chats := g.counts()
	if created != 1 || len(chats) != 1 {
		t.Fatalf("replay created=%d chats=%d, want 1 and 1 (nothing new was sent)", created, len(chats))
	}
}

func TestReplayWindowExpiryDeliversAgain(t *testing.T) {
	_, srv := newFakeGateway(t, "s3cret")
	cfg := cfgFor(srv, tokenFile(t, "s3cret"), "new")
	client := NewClient(time.Second)
	now := time.Now()
	client.SetClock(func() time.Time { return now })
	client.SetReplayWindow(time.Minute)

	if _, err := client.Deliver(context.Background(), cfg, reqFor("msg-1")); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	now = now.Add(2 * time.Minute)
	again, err := client.Deliver(context.Background(), cfg, reqFor("msg-1"))
	if err != nil {
		t.Fatalf("delivery after the window: %v", err)
	}
	if again.Replayed {
		t.Fatal("a delivery outside the replay window was still answered as a replay")
	}
}

// TestRetryAfterChatFailureReusesTheCreatedSession: a create that succeeded
// followed by a failed chat must not mint a second session on the retry.
func TestRetryAfterChatFailureReusesTheCreatedSession(t *testing.T) {
	g, srv := newFakeGateway(t, "s3cret")
	cfg := cfgFor(srv, tokenFile(t, "s3cret"), "new")
	client := NewClient(time.Second)

	// Fail the first chat with a 500, then let it through.
	failNext := true
	var mu sync.Mutex
	inner := g
	mux := http.NewServeMux()
	mux.HandleFunc("/api/sessions", inner.handleCreate)
	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fail := failNext
		failNext = false
		mu.Unlock()
		if fail && strings.HasSuffix(r.URL.Path, "/chat") {
			writeErr(w, http.StatusInternalServerError)
			return
		}
		inner.handleSession(w, r)
	})
	failing := httptest.NewServer(mux)
	t.Cleanup(failing.Close)
	cfg.BaseURL = failing.URL

	if _, err := client.Deliver(context.Background(), cfg, reqFor("msg-1")); err == nil {
		t.Fatal("expected the first turn to fail")
	}
	if _, err := client.Deliver(context.Background(), cfg, reqFor("msg-1")); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if created, chats := g.counts(); created != 1 || len(chats) != 1 {
		t.Fatalf("created=%d chats=%d, want 1 and 1 (the retry reused the session it made)", created, len(chats))
	}
}

// ----- credential handling -----

func TestCredentialFileDiscipline(t *testing.T) {
	secret := "do-not-leak-me"
	cases := []struct {
		name string
		path func(t *testing.T) string
		want string
	}{
		{"missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") }, "unavailable"},
		{"group-readable", func(t *testing.T) string {
			p := tokenFile(t, secret)
			if err := os.Chmod(p, 0o644); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			return p
		}, "group/world-accessible"},
		{"empty", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "empty")
			if err := os.WriteFile(p, []byte("\n"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			return p
		}, "empty"},
	}
	for _, tc := range cases {
		_, srv := newFakeGateway(t, secret)
		cfg := &Config{BaseURL: srv.URL, TokenFile: tc.path(t), Policy: PolicyNew}
		_, err := NewClient(time.Second).Deliver(context.Background(), cfg, reqFor("msg-1"))
		f := errCode(t, err)
		if f.Code != CodeCredentialUnavailable || f.Status != http.StatusBadGateway {
			t.Fatalf("%s: code/status = %s/%d, want %s/502", tc.name, f.Code, f.Status, CodeCredentialUnavailable)
		}
		if !strings.Contains(f.Error(), tc.want) {
			t.Errorf("%s: error %q does not name the condition %q", tc.name, f.Error(), tc.want)
		}
		if strings.Contains(f.Error(), secret) {
			t.Fatalf("%s: the error text carries the credential: %q", tc.name, f.Error())
		}
	}
}

// TestWaitForOneMessageHoldsOtherMessagesConcurrent: the per-message lock is
// per id, so an unrelated delivery is not serialized behind a slow one.
func TestDistinctMessagesDoNotSerialize(t *testing.T) {
	g, srv := newFakeGateway(t, "s3cret")
	g.createDelay = 300 * time.Millisecond
	cfg := cfgFor(srv, tokenFile(t, "s3cret"), "new")
	client := NewClient(5 * time.Second)

	start := time.Now()
	done := make(chan error, 2)
	for _, id := range []string{"msg-1", "msg-2"} {
		go func(id string) { _, err := client.Deliver(context.Background(), cfg, reqFor(id)); done <- err }(id)
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent delivery: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > 550*time.Millisecond {
		t.Fatalf("two distinct messages took %s; they were serialized behind one lock", elapsed)
	}
}

// TestConcurrentSameMessageIDCreatesOneSession: the replay is per id, so two
// simultaneous deliveries of one id must produce exactly one session.
func TestConcurrentSameMessageIDCreatesOneSession(t *testing.T) {
	g, srv := newFakeGateway(t, "s3cret")
	g.createDelay = 100 * time.Millisecond
	cfg := cfgFor(srv, tokenFile(t, "s3cret"), "new")
	client := NewClient(5 * time.Second)

	results := make([]Result, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = client.Deliver(context.Background(), cfg, reqFor("msg-1"))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	created, chats := g.counts()
	if created != 1 || len(chats) != 1 {
		t.Fatalf("created=%d chats=%d, want 1 and 1", created, len(chats))
	}
	if results[0].SessionID != results[1].SessionID {
		t.Fatalf("sessions differ: %q vs %q", results[0].SessionID, results[1].SessionID)
	}
	if !results[0].Replayed && !results[1].Replayed {
		t.Fatal("neither duplicate was reported as a replay")
	}
}

// ----- request shape -----

func TestRequestCarriesCorrelationHeaders(t *testing.T) {
	var seen http.Header
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions":
			w.Header().Set("X-Hermes-Session-Id", "sess-9")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"session":{"id":"sess-9"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/sessions/sess-9/chat":
			seen = r.Header.Clone()
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"session_id":"sess-9","message":{"content":"ok"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := &Config{BaseURL: srv.URL + "/", TokenFile: tokenFile(t, "s3cret"), Policy: PolicyNew}
	if _, err := NewClient(time.Second).Deliver(context.Background(), cfg, reqFor("msg-42")); err != nil {
		t.Fatalf("delivery: %v", err)
	}
	if got := seen.Get("X-Crier-Message-Id"); got != "msg-42" {
		t.Errorf("X-Crier-Message-Id = %q, want msg-42", got)
	}
	if got := seen.Get("X-Crier-Target"); got != "agent-a" {
		t.Errorf("X-Crier-Target = %q, want agent-a", got)
	}
	if got := seen.Get("Authorization"); got != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want the file's bearer token", got)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "summarise the log") {
		t.Errorf("chat message = %v, want the payload text", body["message"])
	}
}

func TestRedirectIsRefusedNotFollowed(t *testing.T) {
	var hit bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte(`{"session":{"id":"stolen"}}`))
	}))
	t.Cleanup(elsewhere.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/api/sessions", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	cfg := &Config{BaseURL: redirector.URL, TokenFile: tokenFile(t, "s3cret"), Policy: PolicyNew}
	_, err := NewClient(time.Second).Deliver(context.Background(), cfg, reqFor("msg-1"))
	f := errCode(t, err)
	if f.Code != CodeGatewayRejected || f.GatewayStatus != http.StatusFound {
		t.Fatalf("code/gateway status = %s/%d, want %s/302", f.Code, f.GatewayStatus, CodeGatewayRejected)
	}
	if hit {
		t.Fatal("the credential was replayed at a redirect target")
	}
}

func TestMessageText(t *testing.T) {
	cases := map[string]string{
		`"plain text"`:           "plain text",
		`{"a":1}`:                `{"a":1}`,
		`  ["x"]  `:              `["x"]`,
		`"a \"quoted\" message"`: `a "quoted" message`,
		`5`:                      `5`,
	}
	for in, want := range cases {
		if got := messageText([]byte(in)); got != want {
			t.Errorf("messageText(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	base := func() *Config {
		return &Config{BaseURL: "http://gw:8080", TokenFile: "/run/secrets/gw", Policy: PolicyNew}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	bad := []func(*Config){
		func(c *Config) { c.BaseURL = "" },
		func(c *Config) { c.BaseURL = "gw:8080" },
		func(c *Config) { c.TokenFile = "" },
		func(c *Config) { c.Policy = "sometimes" },
		func(c *Config) { c.Policy = PolicyExisting },
		func(c *Config) { c.Policy = PolicyPerAgent; c.SessionID = "pinned" },
		func(c *Config) { c.TimeoutMs = -1 },
	}
	for i, mutate := range bad {
		c := base()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("case %d: invalid config accepted: %+v", i, c)
		}
	}
}
