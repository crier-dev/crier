package main

// harness_ingress_bind_test.go — the end-to-end row of DF-CRIER-500:
// POST /ingress/messages with `target.harness` creates a Crier session that is
// BOUND to a harness adapter, and every later delivery to that session routes
// through the adapter the hook in harnesswiring.go resolves.
//
// The registry carries the package's own fakeAdapter, so the test exercises the
// shipped dispatch path (ingress.finish → harnessDispatch → Registry.Deliver →
// adapter) rather than a stub of it.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/ingress"
	"github.com/crier-dev/crier/internal/session"
)

// testHarnessID is the adapter id these tests bind. It is deliberately NOT
// "pi": the harness dispatch counters are process-wide, and
// TestHarnessDispatchMetricsOnBootedServer asserts EXACT sample values on the
// "pi" label on the stated ground that no other test in this package dispatches
// under it. A test adapter using that id would bump those counters and red the
// OBS test for a reason that has nothing to do with observation. The id below
// reaches the registry through the same path — the session records it, the hook
// reads it, Registry.Get resolves it — so neither the binding nor the routing
// is weakened by the name.
const testHarnessID = "bindtest"

// testHarnessSessionID is the harness-side id the fake adapter mints.
const testHarnessSessionID = "hs-sess-1"

// ingressResult is the §2.1 response fields this row asserts on.
type ingressResult struct {
	Mode           string `json:"mode"`
	SessionID      string `json:"session_id"`
	MessageID      string `json:"message_id"`
	CreatedSession bool   `json:"created_session"`
	Delivery       struct {
		Mode    string `json:"mode"`
		Harness string `json:"harness"`
		State   string `json:"state"`
		Error   string `json:"error"`
	} `json:"delivery"`
}

// newIngressHarness builds the production wiring shape: one ingress handler
// over the real session store with the harness dispatch hook attached.
func newIngressHarness(t *testing.T, repo session.Repository) (*ingress.Handler, *fakeAdapter) {
	t.Helper()
	adapter := &fakeAdapter{name: testHarnessID, minted: testHarnessSessionID}
	reg := harness.NewRegistry(harness.RegistryOptions{
		Enabled: true, Default: testHarnessID, Timeout: 30 * time.Second, MaxConcurrent: 1,
	})
	reg.Register(adapter)
	return &ingress.Handler{
		Repo:     repo,
		Idem:     ingress.NewMemoryIdempotencyStore(),
		Dispatch: &harnessDispatch{reg: reg, repo: repo},
	}, adapter
}

// postIngress issues one POST /ingress/messages and returns the status plus the
// decoded body.
func postIngress(t *testing.T, h *ingress.Handler, body map[string]any) (int, ingressResult) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/ingress/messages", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var out ingressResult
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, out
}

// newHarnessSessionBody is the mode=new_session body, with the harness binding
// the test is about spliced in when a name is given.
func newHarnessSessionBody(harnessName string) map[string]any {
	target := map[string]any{
		"template": map[string]any{"title": "push into a harness"},
	}
	if harnessName != "" {
		target["harness"] = harnessName
	}
	return map[string]any{
		"mode":   "new_session",
		"target": target,
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "review the open PR",
	}
}

// TestIngressNewSessionBindsHarnessAndDelivers is AC(a): a new_session push
// that names a harness creates the session, records the binding on it, and the
// FIRST delivery goes through that adapter — recording the harness-side id the
// adapter minted (session.harness.bind), the state nothing in the product could
// previously reach through a public route.
func TestIngressNewSessionBindsHarnessAndDelivers(t *testing.T) {
	ctx := context.Background()
	repo := newJointRepo(t)
	h, adapter := newIngressHarness(t, repo)

	code, res := postIngress(t, h, newHarnessSessionBody(testHarnessID))
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", code)
	}
	if !res.CreatedSession || res.SessionID == "" {
		t.Fatalf("result = %+v, want a created session", res)
	}
	if res.Delivery.Mode != "harness" || res.Delivery.Harness != testHarnessID || res.Delivery.State != harness.StateDispatched {
		t.Fatalf("delivery = %+v, want harness/%s/%s", res.Delivery, testHarnessID, harness.StateDispatched)
	}
	if len(adapter.seen) != 1 {
		t.Fatalf("adapter saw %d deliveries, want 1", len(adapter.seen))
	}
	if adapter.seen[0].Harness != testHarnessID || adapter.seen[0].ID != "" {
		t.Fatalf("first delivery ref = %+v, want the bound adapter with no session id (a create)", adapter.seen[0])
	}

	st, err := repo.Load(ctx, res.SessionID)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if st.Session.Harness != testHarnessID {
		t.Fatalf("session harness = %q, want %q", st.Session.Harness, testHarnessID)
	}
	if st.Session.HarnessSessionID != testHarnessSessionID {
		t.Fatalf("harness_session_id = %q, want %q (the id the adapter minted)", st.Session.HarnessSessionID, testHarnessSessionID)
	}
	bound := false
	for _, rec := range st.Lifecycle {
		if rec.Type == session.RecordHarnessBind && rec.Harness == testHarnessID && rec.HarnessSessionID == testHarnessSessionID {
			bound = true
		}
	}
	if !bound {
		t.Fatalf("no session.harness.bind record in the lifecycle: %+v", st.Lifecycle)
	}
}

// TestIngressSessionModeContinuesTheBoundHarnessSession is AC(b): a delivery
// that targets the session (mode=session, no harness in the body) still routes
// through the bound adapter, continuing the id the first delivery minted.
func TestIngressSessionModeContinuesTheBoundHarnessSession(t *testing.T) {
	repo := newJointRepo(t)
	h, adapter := newIngressHarness(t, repo)

	code, created := postIngress(t, h, newHarnessSessionBody(testHarnessID))
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}

	code, res := postIngress(t, h, map[string]any{
		"mode":   "session",
		"target": map[string]any{"session_id": created.SessionID},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "and now the follow-up",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if res.Delivery.Mode != "harness" || res.Delivery.State != harness.StateDispatched {
		t.Fatalf("delivery = %+v, want a harness dispatch", res.Delivery)
	}
	if len(adapter.seen) != 2 {
		t.Fatalf("adapter saw %d deliveries, want 2", len(adapter.seen))
	}
	if got := adapter.seen[1].ID; got != testHarnessSessionID {
		t.Fatalf("second delivery continued %q, want %q (a silent re-create would split the transcript)", got, testHarnessSessionID)
	}
}

// TestIngressSessionWithoutHarnessStaysStoreOnly is AC(c): the field is
// optional. A session created with no harness is store-only, and a delivery to
// it never claims a dispatch.
func TestIngressSessionWithoutHarnessStaysStoreOnly(t *testing.T) {
	repo := newJointRepo(t)
	h, adapter := newIngressHarness(t, repo)

	code, created := postIngress(t, h, newHarnessSessionBody(""))
	if code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", code)
	}
	if created.Delivery.Mode != "store" || created.Delivery.State != "stored" {
		t.Fatalf("delivery = %+v, want store/stored", created.Delivery)
	}

	code, res := postIngress(t, h, map[string]any{
		"mode":   "session",
		"target": map[string]any{"session_id": created.SessionID},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "still store only",
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if res.Delivery.Mode != "store" || res.Delivery.State != "stored" {
		t.Fatalf("delivery = %+v, want store/stored", res.Delivery)
	}
	if len(adapter.seen) != 0 {
		t.Fatalf("an unbound session reached the adapter %d times; it must stay store-only", len(adapter.seen))
	}
}
