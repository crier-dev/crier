package ingress

// harness_bind_test.go — CRIER-HARNESS §4.3's create half: a NEW session may
// name the harness adapter it is bound to, so the delivery hook has somewhere
// to record the harness-side id. Before this row the field did not exist in the
// strict-decoded body at all, so `target.harness` was INGRESS_INVALID_JSON and
// nothing in the product could produce a bound session from a push.

import (
	"context"
	"net/http"
	"testing"

	"github.com/crier-dev/crier/internal/session"
)

// loadSession reads a session straight out of the repository, so an assertion
// is about the RECORD and not about the response echoing the request.
func (e *testEnv) loadSession(t *testing.T, id string) *session.State {
	t.Helper()
	st, err := e.repo.Load(context.Background(), id)
	if err != nil {
		t.Fatalf("load session %q: %v", id, err)
	}
	return st
}

// newSessionBody is the minimal mode=new_session body, with the harness
// spelling the test is about spliced in by the caller.
func newSessionBody(author, title string) map[string]any {
	return map[string]any{
		"mode":   "new_session",
		"target": map[string]any{"template": map[string]any{"title": title}},
		"author": map[string]string{"agent": author},
		"text":   "harness push",
	}
}

// TestNewSessionIngressBindsHarness is AC1: `target.harness` is accepted by the
// strict decoder, the session is created, and the binding is recorded on the
// session (not merely echoed) — the state the dispatch hook needs to reach an
// adapter at all.
func TestNewSessionIngressBindsHarness(t *testing.T) {
	env := newTestEnv(t)
	body := newSessionBody("ci-bot", "harness bound")
	body["target"].(map[string]any)["harness"] = "pi"

	rec := env.post(t, body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	res := decodeResult(t, rec)
	if res.SessionID == "" {
		t.Fatal("no session id in the result")
	}

	st := env.loadSession(t, res.SessionID)
	if got := st.Session.Harness; got != "pi" {
		t.Fatalf("session harness = %q, want %q — the binding is what makes push delivery reachable", got, "pi")
	}
	if st.Session.HarnessSessionID != "" {
		t.Fatalf("harness_session_id = %q, want empty: no adapter has answered yet", st.Session.HarnessSessionID)
	}
	// No dispatch hook is wired on this fixture, so the honest report is
	// store/stored — the create must not invent a delivery it cannot make.
	if res.Delivery.Mode != "store" || res.Delivery.State != "stored" {
		t.Fatalf("delivery = %+v, want store/stored", res.Delivery)
	}
}

// TestNewSessionIngressAcceptsTemplateHarness covers the second accepted
// spelling: the same field inside the template, which is where a caller who
// reads "template" as the whole new-session description will put it.
func TestNewSessionIngressAcceptsTemplateHarness(t *testing.T) {
	env := newTestEnv(t)
	body := map[string]any{
		"mode": "new_session",
		"target": map[string]any{"template": map[string]any{
			"title":   "template harness",
			"harness": "opencode",
		}},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "harness push",
	}

	rec := env.post(t, body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	res := decodeResult(t, rec)
	st := env.loadSession(t, res.SessionID)
	if st.Session.Harness != "opencode" {
		t.Fatalf("session harness = %q, want opencode", st.Session.Harness)
	}
}

// TestNewSessionIngressRefusesDisagreeingHarness: both spellings mean one
// thing, so naming two different adapters is refused rather than resolved by
// precedence — picking one would execute a choice the caller never made.
func TestNewSessionIngressRefusesDisagreeingHarness(t *testing.T) {
	env := newTestEnv(t)
	body := map[string]any{
		"mode": "new_session",
		"target": map[string]any{
			"harness": "pi",
			"template": map[string]any{
				"title":   "conflict",
				"harness": "opencode",
			},
		},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "harness push",
	}

	rec := env.post(t, body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec); code != CodeTemplateInvalid {
		t.Fatalf("error = %q, want %q", code, CodeTemplateInvalid)
	}
}

// TestHarnessFieldIsRefusedOnOtherModes: §4.3 gives the binding to a NEW
// session. On the other two destinations the field would be accepted and
// dropped — the silent no-op the strict decoder exists to prevent — so it is a
// named refusal.
func TestHarnessFieldIsRefusedOnOtherModes(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{
			name: "session",
			body: map[string]any{
				"mode":   "session",
				"target": map[string]any{"session_id": "s_any", "harness": "pi"},
				"author": map[string]string{"agent": "ci-bot"},
				"text":   "hi",
			},
		},
		{
			name: "inbox",
			body: map[string]any{
				"mode":   "inbox",
				"target": map[string]any{"agent_id": "alpha", "harness": "pi"},
				"author": map[string]string{"agent": "ci-bot"},
				"text":   "hi",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			rec := env.post(t, tc.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if code := decodeErrorCode(t, rec); code != CodeHarnessNotAllowed {
				t.Fatalf("error = %q, want %q", code, CodeHarnessNotAllowed)
			}
		})
	}
}

// TestHarnessFieldIsOptional is AC3 (backward compatibility): a new_session
// with no harness anywhere creates exactly the store-only session it always
// did, and the response shape is unchanged.
func TestHarnessFieldIsOptional(t *testing.T) {
	env := newTestEnv(t)
	rec := env.post(t, newSessionBody("ci-bot", "store only"), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	res := decodeResult(t, rec)
	st := env.loadSession(t, res.SessionID)
	if st.Session.Harness != "" || st.Session.HarnessSessionID != "" {
		t.Fatalf("an absent target.harness bound the session: harness=%q id=%q",
			st.Session.Harness, st.Session.HarnessSessionID)
	}
	if res.Delivery.Mode != "store" || res.Delivery.State != "stored" {
		t.Fatalf("delivery = %+v, want store/stored", res.Delivery)
	}
}

// TestRequestHashTracksHarness keeps the idempotency ledger honest: two calls
// that differ only in the harness they bind are DIFFERENT requests, so one
// cannot be replayed as the answer to the other.
func TestRequestHashTracksHarness(t *testing.T) {
	base := &Request{
		Mode:     ModeNewSession,
		Template: &SessionTemplate{Title: "h", Kind: string(session.KindChannel)},
		Author:   session.AuthorRef{Agent: "ci"},
		Text:     "hi",
		Harness:  "pi",
	}
	h1, err := requestHash(base, "")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	changed := *base
	changed.Harness = "opencode"
	h2, err := requestHash(&changed, "")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if h1 == h2 {
		t.Fatal("a changed harness hashed alike: the ledger would replay one request for the other")
	}
}
