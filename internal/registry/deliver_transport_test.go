package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/webhook"
)

// DF-CRIER-157 — accept-time transport signaling.
//
// A delivery to an agent that has a webhook endpoint goes to that endpoint
// and BYPASSES the durable inbox by design, so a later retrieve is empty.
// Before this change the sender could not tell the two apart from the accept
// alone (202 {id} on the webhook path vs 201 {id, expires_at} on the inbox
// path), and the vacuous-empty-retrieve trap made "I lost a message"
// un-diagnosable. The accept now names the transport ("webhook" | "inbox")
// and, on the async/batch webhook paths, the queue semantics
// (delivery_mode: async | batch).

// deliverWire is the wire shape of a POST /agents/{id}/inbox accept, decoded
// exactly as a curl-ing sender sees it.
type deliverWire struct {
	ID           string          `json:"id"`
	Transport    string          `json:"transport"`
	DeliveryMode string          `json:"delivery_mode"`
	ExpiresAt    *string         `json:"expires_at"`
	Reply        json.RawMessage `json:"reply"`
	Guard        json.RawMessage `json:"guard"`
}

// deliverHarness registers agentID with the given webhook config (nil =
// inbox-only agent) and returns a router serving POST /agents/{id}/inbox
// with an enabled webhook driver wired to the same store.
func deliverHarness(t *testing.T, agentID string, cfg *webhook.Config) (http.Handler, Store) {
	t.Helper()
	store := setupTestStore(t)
	_, pubKey := newTestPubKey(t)
	if err := store.Register(&Agent{
		ID:        agentID,
		PublicKey: HexKey(pubKey),
		Webhook:   cfg,
	}); err != nil {
		t.Fatalf("register %s: %v", agentID, err)
	}

	handler := NewHandler(store)
	driver := webhook.NewDriver(webhook.NewClient(2*time.Second, nil), webhook.NewMemoryQueue(), webhook.DriverConfig{
		MaxRetries:         5,
		RedeliverEvery:     100 * time.Millisecond,
		ProbeEvery:         200 * time.Millisecond,
		BatchMaxMessages:   10,
		BatchFlushInterval: 150 * time.Millisecond,
		BatchTick:          10 * time.Millisecond,
	})
	driver.Start()
	t.Cleanup(driver.Stop)
	handler.SetWebhookDriver(driver)
	// The queue drain resolves the agent's webhook config through this
	// resolver — without it items are silently dropped ("agent gone").
	driver.SetConfigResolver(func(id string) (*webhook.Config, error) {
		agent, err := store.Get(id)
		if err != nil {
			return nil, err
		}
		if agent.Webhook == nil {
			return nil, fmt.Errorf("agent %s has no webhook configured", id)
		}
		return agent.Webhook, nil
	})

	router := mux.NewRouter()
	router.HandleFunc("/agents/{id}/inbox", handler.HandleDeliver).Methods("POST")
	return router, store
}

// postDeliver performs the curl-shaped request and decodes the JSON accept,
// failing loudly with the raw body when the response is not JSON.
func postDeliver(t *testing.T, h http.Handler, agentID, body string) (*httptest.ResponseRecorder, deliverWire) {
	t.Helper()
	req := httptest.NewRequest("POST", "/agents/"+agentID+"/inbox", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var wire deliverWire
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatalf("deliver %s: status %d, body is not JSON: %v — raw: %s",
			agentID, rec.Code, err, rec.Body.String())
	}
	return rec, wire
}

// waitForEndpointPosts polls until the endpoint has seen at least n POSTs.
func waitForEndpointPosts(t *testing.T, got *atomic.Int32, n int32, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if got.Load() >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: endpoint never received the POST (posts=%d, want %d)", what, got.Load(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ----- A1: async webhook accept names the transport and the queue mode -----

func TestDeliver_WebhookAsync_AcceptNamesWebhookTransport(t *testing.T) {
	var posts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	h, store := deliverHarness(t, "agent-w", &webhook.Config{
		URL:          ts.URL,
		DeliveryMode: "async",
		Retries:      1,
		TimeoutMs:    5000,
	})

	rec, wire := postDeliver(t, h, "agent-w", `{"payload":{"msg":"fire-and-forget"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("async webhook accept: status = %d, want 202 — body: %s", rec.Code, rec.Body.String())
	}
	if wire.Transport != "webhook" {
		t.Errorf("transport = %q, want %q — body: %s", wire.Transport, "webhook", rec.Body.String())
	}
	if wire.DeliveryMode != "async" {
		t.Errorf("delivery_mode = %q, want %q — body: %s", wire.DeliveryMode, "async", rec.Body.String())
	}
	if wire.ExpiresAt != nil {
		t.Errorf("expires_at = %q, want absent on a webhook delivery (nothing was stored)", *wire.ExpiresAt)
	}
	if wire.ID == "" {
		t.Errorf("id is empty — body: %s", rec.Body.String())
	}

	// The message really went to the endpoint, not the inbox: the endpoint
	// sees the POST and the target's durable inbox stays EMPTY by design.
	waitForEndpointPosts(t, &posts, 1, "async POST")
	q, leased, _, err := store.Stats("agent-w")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if q != 0 || leased != 0 {
		t.Errorf("webhook delivery stored an inbox entry: queue=%d leased=%d (webhook must bypass the inbox)", q, leased)
	}
}

func TestDeliver_WebhookBatch_AcceptNamesBatchMode(t *testing.T) {
	var posts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	h, _ := deliverHarness(t, "agent-batch", &webhook.Config{
		URL:          ts.URL,
		DeliveryMode: "batch",
		Batch:        &webhook.BatchConfig{MaxMessages: 1, FlushIntervalS: 0},
		TimeoutMs:    5000,
	})

	rec, wire := postDeliver(t, h, "agent-batch", `{"payload":{"msg":"coalesce me"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("batch webhook accept: status = %d, want 202 — body: %s", rec.Code, rec.Body.String())
	}
	if wire.Transport != "webhook" {
		t.Errorf("transport = %q, want %q — body: %s", wire.Transport, "webhook", rec.Body.String())
	}
	if wire.DeliveryMode != "batch" {
		t.Errorf("delivery_mode = %q, want %q — body: %s", wire.DeliveryMode, "batch", rec.Body.String())
	}
	// The accept is not a lie: the coalesced batch POST does reach the sink.
	waitForEndpointPosts(t, &posts, 1, "batch POST")

	// A per-message delivery_mode override is echoed as RESOLVED, not as the
	// agent's configured default.
	rec, wire = postDeliver(t, h, "agent-batch", `{"payload":{"msg":"async override"},"delivery_mode":"async"}`)
	if rec.Code != http.StatusAccepted || wire.Transport != "webhook" {
		t.Fatalf("async override: status = %d transport = %q — body: %s", rec.Code, wire.Transport, rec.Body.String())
	}
	if wire.DeliveryMode != "async" {
		t.Errorf("delivery_mode = %q, want %q (the request override, not the agent default "+
			"%q) — body: %s", wire.DeliveryMode, "async", "batch", rec.Body.String())
	}
}

// ----- A1/A2: inbox accept names the inbox transport and still carries expiry -----

func TestDeliver_Inbox_AcceptNamesInboxTransport(t *testing.T) {
	h, _ := deliverHarness(t, "agent-inbox", nil /* no webhook → durable inbox */)

	rec, wire := postDeliver(t, h, "agent-inbox", `{"payload":{"msg":"durable"},"ttl_seconds":60}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("inbox deliver: status = %d, want 201 — body: %s", rec.Code, rec.Body.String())
	}
	if wire.Transport != "inbox" {
		t.Errorf("transport = %q, want %q — body: %s", wire.Transport, "inbox", rec.Body.String())
	}
	if wire.DeliveryMode != "" {
		t.Errorf("delivery_mode = %q, want absent for an inbox delivery (no webhook queue)", wire.DeliveryMode)
	}
	if wire.ExpiresAt == nil {
		t.Fatalf("expires_at absent on a stored inbox message — body: %s", rec.Body.String())
	}
	if _, err := time.Parse(time.RFC3339, *wire.ExpiresAt); err != nil {
		t.Errorf("expires_at %q is not RFC 3339: %v", *wire.ExpiresAt, err)
	}
}

// ----- blocking: the success body names the webhook transport -----

func TestDeliver_Blocking_AcceptNamesWebhookTransport(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"echo":true}`))
	}))
	defer ts.Close()

	h, _ := deliverHarness(t, "agent-block", &webhook.Config{
		URL:          ts.URL,
		DeliveryMode: "blocking",
		TimeoutMs:    2000,
	})

	rec, wire := postDeliver(t, h, "agent-block", `{"payload":{"ping":1},"timeout_ms":2000}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("blocking webhook deliver: status = %d, want 200 — body: %s", rec.Code, rec.Body.String())
	}
	if wire.Transport != "webhook" {
		t.Errorf("transport = %q, want %q — body: %s", wire.Transport, "webhook", rec.Body.String())
	}
	if string(wire.Reply) != `{"echo":true}` {
		t.Errorf("reply = %s, want the endpoint's body", wire.Reply)
	}
}

// ----- A3-ish: 502 for a permanent rejection, 504 for timeout/budget -----

func TestDeliver_Blocking_PermanentRejectIs502(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		schema       *webhook.CustomSchema
		wantContains string
	}{
		{name: "endpoint 4xx", status: http.StatusBadRequest, body: `{"error":"bad auth"}`,
			wantContains: "webhook: permanent failure: status 400"},
		{name: "unparseable 2xx reply", status: http.StatusOK, body: `{"nope":true}`,
			schema:       &webhook.CustomSchema{ResponseMap: "reply.text"},
			wantContains: "reply extraction"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer ts.Close()

			h, store := deliverHarness(t, "agent-perm", &webhook.Config{
				URL:          ts.URL,
				DeliveryMode: "blocking",
				CustomSchema: tc.schema,
				TimeoutMs:    2000,
			})

			rec, wire := postDeliver(t, h, "agent-perm", `{"payload":{"ping":1},"timeout_ms":2000}`)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("permanent rejection: status = %d, want 502 — body: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(string(rec.Body.Bytes()), tc.wantContains) {
				t.Errorf("error body = %s, want it to contain %q", rec.Body.String(), tc.wantContains)
			}
			if wire.ID != "" {
				t.Errorf("502 body carried id=%q (a rejected delivery is not accepted)", wire.ID)
			}
			// Nothing was stored: the endpoint rejected, the inbox is not a
			// fallback.
			if q, _, _, err := store.Stats("agent-perm"); err != nil || q != 0 {
				t.Errorf("inbox stats after a rejected blocking delivery: queue=%d err=%v, want 0", q, err)
			}
		})
	}
}

func TestDeliver_Blocking_TimeoutIs504(t *testing.T) {
	t.Run("budget exhausted by retryable failures", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer ts.Close()

		h, _ := deliverHarness(t, "agent-slow", &webhook.Config{
			URL:          ts.URL,
			DeliveryMode: "blocking",
			TimeoutMs:    120,
		})

		rec, _ := postDeliver(t, h, "agent-slow", `{"payload":{"ping":1},"timeout_ms":120}`)
		if rec.Code != http.StatusGatewayTimeout {
			t.Fatalf("budget exhaustion: status = %d, want 504 — body: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "timed out") {
			t.Errorf("error body = %s, want a timeout message", rec.Body.String())
		}
	})

	t.Run("endpoint hangs past the budget", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(2 * time.Second)
		}))
		defer ts.Close()

		h, _ := deliverHarness(t, "agent-hang", &webhook.Config{
			URL:          ts.URL,
			DeliveryMode: "blocking",
			TimeoutMs:    100,
		})

		rec, _ := postDeliver(t, h, "agent-hang", `{"payload":{"ping":1},"timeout_ms":100}`)
		if rec.Code != http.StatusGatewayTimeout {
			t.Fatalf("hanging endpoint: status = %d, want 504 — body: %s", rec.Code, rec.Body.String())
		}
	})
}

// ----- A2: agents WITHOUT webhooks are untouched -----

func TestDeliver_NoWebhook_UnchangedStatusAndBody(t *testing.T) {
	h, store := deliverHarness(t, "agent-plain", nil)

	rec, wire := postDeliver(t, h, "agent-plain", `{"payload":{"msg":"plain"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — body: %s", rec.Code, rec.Body.String())
	}
	if wire.Transport != "inbox" || wire.ExpiresAt == nil || wire.ID == "" {
		t.Fatalf("body = %s, want id + transport=inbox + expires_at", rec.Body.String())
	}
	// The message is really in the durable inbox and retrievable.
	entries, leaseID, err := store.Retrieve("agent-plain", time.Minute, 10)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(entries) != 1 || leaseID == "" {
		t.Fatalf("retrieve = %d entries (lease %q), want exactly 1 leased message", len(entries), leaseID)
	}
	if string(entries[0].Payload) != `{"msg":"plain"}` {
		t.Errorf("payload = %s, want the delivered payload", entries[0].Payload)
	}
	if wire.ID != entries[0].ID {
		t.Errorf("accept id = %q, retrieved id = %q — they must agree", wire.ID, entries[0].ID)
	}
}

// TestTransportField_IsAlwaysPresentOnBothAcceptPaths pins the JSON contract:
// transport is not omitempty, so a sender can branch on it without
// inferring the destination from the status code (DF-CRIER-157).
func TestTransportField_IsAlwaysPresentOnBothAcceptPaths(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	h, _ := deliverHarness(t, "agent-keys", &webhook.Config{
		URL:          ts.URL,
		DeliveryMode: "async",
		TimeoutMs:    5000,
	})
	rec, _ := postDeliver(t, h, "agent-keys", `{"payload":{"a":1}}`)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if _, ok := raw["transport"]; !ok {
		t.Errorf("202 webhook accept has no transport key: %s", rec.Body.String())
	}
	if _, ok := raw["delivery_mode"]; !ok {
		t.Errorf("202 webhook accept has no delivery_mode key: %s", rec.Body.String())
	}

	inboxH, _ := deliverHarness(t, "agent-keys-inbox", nil)
	rec, _ = postDeliver(t, inboxH, "agent-keys-inbox", `{"payload":{"a":1}}`)
	raw = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if _, ok := raw["transport"]; !ok {
		t.Errorf("201 inbox accept has no transport key: %s", rec.Body.String())
	}
	if _, ok := raw["delivery_mode"]; ok {
		t.Errorf("201 inbox accept carries delivery_mode: %s", rec.Body.String())
	}
}
