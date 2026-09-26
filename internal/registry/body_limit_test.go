package registry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInboxDeliveryRejectsOversizedBodiesBeforeSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  string
		setup func(t *testing.T, store Store)
	}{
		{
			name: "agent id content length",
			path: "/agents/agent-1/inbox",
			setup: func(t *testing.T, store Store) {
				registerBodyLimitAgent(t, store, "agent-1")
			},
		},
		{
			name: "capability route content length",
			path: "/capabilities/solver/inbox",
			setup: func(t *testing.T, store Store) {
				registerBodyLimitAgent(t, store, "worker-1", "solver")
			},
		},
		{
			name: "agent id chunked",
			path: "/agents/agent-1/inbox",
			setup: func(t *testing.T, store Store) {
				registerBodyLimitAgent(t, store, "agent-1")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := setupTestStore(t)
			handler, router := setupRouterWithHandler(store)
			handler.SetMaxInboxBodyBytes(64)
			tc.setup(t, store)

			body := `{"payload":{"text":"` + strings.Repeat("x", 80) + `"}}`
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if strings.Contains(tc.name, "chunked") {
				req.ContentLength = -1
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); got != `{"error":"REQUEST_BODY_TOO_LARGE"}
` {
				t.Fatalf("body = %q, want stable named 413 error", got)
			}

			for _, id := range []string{"agent-1", "worker-1"} {
				if _, err := store.Get(id); err == nil {
					if depth, leased, _, err := store.Stats(id); err != nil {
						t.Fatalf("stats %s: %v", id, err)
					} else if depth != 0 || leased != 0 {
						t.Errorf("%s changed after over-cap request: queue=%d leased=%d", id, depth, leased)
					}
				}
			}
		})
	}
}

func TestInboxDeliveryAtBodyLimitRemainsRetrievable(t *testing.T) {
	store := setupTestStore(t)
	handler, router := setupRouterWithHandler(store)
	const limit = 128
	handler.SetMaxInboxBodyBytes(limit)
	registerBodyLimitAgent(t, store, "agent-1")

	base := `{"payload":{}}`
	body := base + strings.Repeat(" ", limit-len(base))
	if len(body) != limit {
		t.Fatalf("test body length = %d, want %d", len(body), limit)
	}
	req := httptest.NewRequest(http.MethodPost, "/agents/agent-1/inbox", strings.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	messages, _, err := store.Retrieve("agent-1", time.Minute, 10)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("retrieved %d messages, want 1", len(messages))
	}
}

func registerBodyLimitAgent(t *testing.T, store Store, id string, capabilities ...string) {
	t.Helper()
	_, pub := newTestPubKey(t)
	if err := store.Register(&Agent{ID: id, PublicKey: HexKey(pub), Capabilities: capabilities}); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}
