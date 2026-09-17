package registry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/webhook"
)

// DF-CRIER-182 — the never-expires WIRE encoding.
//
// Internally the zero time IS "never expires" (DF-CRIER-37: resolveMessageExpiry
// plus the retrieve/stats/purge guards) and that does not change — this is a
// wire-encoding task, not a storage or TTL-semantics one. On the wire the
// expiry is tri-state:
//
//   - ABSENT  — no inbox entry exists (webhook deliveries): the key is not
//     present at all. Unchanged behaviour.
//   - null    — the message never expires (ttl_seconds=0): the key IS present
//     and its value is JSON null.
//   - RFC3339 — a resolved finite instant (ttl_seconds>0, or the 24h default).
//     Unchanged behaviour.
//
// Before this fix the never-expires state rendered as
// "0001-01-01T00:00:00Z" on BOTH the 201 accept and the retrieve body — a
// syntactically valid RFC 3339 instant that any date-parsing client reads as
// broken or long-expired.

// wireField returns the RAW JSON value of key in body together with whether the
// key is present at all. Decoding into a pointer/struct field cannot tell
// "absent" from "present and null" — the distinction this contract is about —
// so these tests read the raw object.
func wireField(t *testing.T, body []byte, key string) (json.RawMessage, bool) {
	t.Helper()
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &raw), "body is not a JSON object: %s", body)
	v, present := raw[key]
	return v, present
}

// ---------------------------------------------------------------------------
// 201 accept: null for a never-expiring message
// ---------------------------------------------------------------------------

func TestDeliver_TTLSeconds_ZeroAcceptReportsExpiresAtNull(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	rec := doInboxRequest(t, router, http.MethodPost, "/agents/agent-1/inbox", deliverBody(t, `{"n":1}`, 0))
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	raw, present := wireField(t, rec.Body.Bytes(), "expires_at")
	require.True(t, present,
		"expires_at key must be PRESENT on a stored delivery (null is the never-expires value) — body: %s", rec.Body.String())
	require.JSONEq(t, `null`, string(raw),
		"expires_at = %s, want null for ttl_seconds=0", raw)

	// The zero time was the old wire value; it must be gone from the body.
	require.NotContains(t, rec.Body.String(), zeroTimeRFC3339,
		"the zero time is no longer the never-expires wire value — body: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// retrieve body: the same message reports null
// ---------------------------------------------------------------------------

func TestRetrieve_TTLSeconds_ZeroMessageReportsExpiresAtNull(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	rec := doInboxRequest(t, router, http.MethodPost, "/agents/agent-1/inbox", deliverBody(t, `{"n":1}`, 0))
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	rec = doInboxRequest(t, router, http.MethodGet, "/agents/agent-1/inbox?max=10", "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var body struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body: %s", rec.Body.String())
	require.Len(t, body.Messages, 1, "a never-expiring message must be retrievable — body: %s", rec.Body.String())

	raw, present := body.Messages[0]["expires_at"]
	require.True(t, present,
		"retrieve body omits expires_at for a never-expiring message — body: %s", rec.Body.String())
	require.JSONEq(t, `null`, string(raw),
		"retrieved expires_at = %s, want null", raw)

	// The rest of the entry still renders: the custom encoder did not drop
	// fields on the way out.
	for _, key := range []string{"id", "agent_id", "payload", "created_at", "acked"} {
		if _, ok := body.Messages[0][key]; !ok {
			t.Errorf("retrieved entry lost the %q field — body: %s", key, rec.Body.String())
		}
	}

	require.NotContains(t, rec.Body.String(), zeroTimeRFC3339,
		"the zero time is no longer the never-expires wire value — body: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// finite lifetimes (and the omitted-ttl default) keep RFC 3339
// ---------------------------------------------------------------------------

func TestDeliver_ExpiresAtStaysRFC3339ForFiniteLifetimes(t *testing.T) {
	cases := []struct {
		name     string
		ttl      any
		wantLife time.Duration
	}{
		{name: "ttl_seconds=60", ttl: 60, wantLife: time.Minute},
		{name: "absent ttl keeps the 24h default", ttl: nil, wantLife: DefaultMessageTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := setupTestStore(t)
			registerTestAgent(t, store)
			router := setupRouter(store)

			start := time.Now()
			rec := doInboxRequest(t, router, http.MethodPost, "/agents/agent-1/inbox", deliverBody(t, `{"n":1}`, tc.ttl))
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

			raw, present := wireField(t, rec.Body.Bytes(), "expires_at")
			require.True(t, present, "expires_at must be present — body: %s", rec.Body.String())

			var iso string
			require.NoError(t, json.Unmarshal(raw, &iso),
				"expires_at = %s, want an RFC 3339 string", raw)
			parsed, err := time.Parse(time.RFC3339, iso)
			require.NoError(t, err, "expires_at %q is not RFC 3339", iso)
			require.InDelta(t, tc.wantLife.Seconds(), parsed.Sub(start).Seconds(), 30,
				"expires_at = %s, want ≈ now+%s", parsed, tc.wantLife)

			// The retrieve surface agrees with the accept.
			rec = doInboxRequest(t, router, http.MethodGet, "/agents/agent-1/inbox?max=10", "")
			require.Equal(t, http.StatusOK, rec.Code)

			var body struct {
				Messages []map[string]json.RawMessage `json:"messages"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Len(t, body.Messages, 1)
			var retrieved string
			require.NoError(t, json.Unmarshal(body.Messages[0]["expires_at"], &retrieved))
			got, err := time.Parse(time.RFC3339, retrieved)
			require.NoError(t, err, "retrieved expires_at %q is not RFC 3339", retrieved)
			require.WithinDuration(t, parsed, got, time.Second,
				"retrieved expiry %s must equal the accept expiry %s", got, parsed)
		})
	}
}

// ---------------------------------------------------------------------------
// webhook accept paths: the key stays ABSENT (no inbox entry was created)
// ---------------------------------------------------------------------------

func TestDeliver_WebhookAcceptsOmitExpiresAtKeyEntirely(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"echo":true}`))
	}))
	defer ts.Close()

	cases := []struct {
		mode string
		want int
		cfg  *webhook.Config
	}{
		{mode: "blocking", want: http.StatusOK, cfg: &webhook.Config{URL: ts.URL, DeliveryMode: "blocking", TimeoutMs: 2000}},
		{mode: "async", want: http.StatusAccepted, cfg: &webhook.Config{URL: ts.URL, DeliveryMode: "async", TimeoutMs: 2000}},
		{mode: "batch", want: http.StatusAccepted, cfg: &webhook.Config{
			URL: ts.URL, DeliveryMode: "batch",
			Batch: &webhook.BatchConfig{MaxMessages: 1, FlushIntervalS: 0}, TimeoutMs: 2000,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			agentID := "agent-null-expiry-" + tc.mode
			h, _ := deliverHarness(t, agentID, tc.cfg)

			rec, _ := postDeliver(t, h, agentID, `{"payload":{"x":1}}`)
			require.Equal(t, tc.want, rec.Code, "body: %s", rec.Body.String())

			_, present := wireField(t, rec.Body.Bytes(), "expires_at")
			require.False(t, present,
				"a webhook delivery creates no inbox entry, so expires_at must be ABSENT (not null) — body: %s", rec.Body.String())
		})
	}
}

// ---------------------------------------------------------------------------
// the shared seam: every surface that marshals an InboxEntry agrees
// ---------------------------------------------------------------------------

func TestInboxEntry_MarshalJSON_TriStateExpiry(t *testing.T) {
	base := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	t.Run("zero time is null", func(t *testing.T) {
		raw, err := json.Marshal(&InboxEntry{ID: "m-never", AgentID: "a", CreatedAt: base})
		require.NoError(t, err)
		got, present := wireField(t, raw, "expires_at")
		require.True(t, present, "expires_at must be present — json: %s", raw)
		require.JSONEq(t, `null`, string(got), "expires_at = %s, want null", got)
		require.NotContains(t, string(raw), zeroTimeRFC3339, "json: %s", raw)
	})

	t.Run("finite instant is RFC 3339", func(t *testing.T) {
		expiry := base.Add(90 * time.Minute)
		raw, err := json.Marshal(&InboxEntry{ID: "m-finite", AgentID: "a", CreatedAt: base, ExpiresAt: expiry})
		require.NoError(t, err)
		got, present := wireField(t, raw, "expires_at")
		require.True(t, present, "json: %s", raw)
		var iso string
		require.NoError(t, json.Unmarshal(got, &iso))
		parsed, err := time.Parse(time.RFC3339, iso)
		require.NoError(t, err, "expires_at %q is not RFC 3339", iso)
		require.True(t, expiry.Equal(parsed), "expires_at = %s, want %s", parsed, expiry)
	})

	t.Run("every other field survives the custom encoder", func(t *testing.T) {
		leased := base.Add(time.Minute)
		entry := &InboxEntry{
			ID: "m-full", AgentID: "agent-1", Payload: json.RawMessage(`{"hello":"world"}`),
			CreatedAt: base, LeasedAt: &leased, LeaseID: "lease-1", ACKed: true,
		}
		raw, err := json.Marshal(entry)
		require.NoError(t, err)

		var back InboxEntry
		require.NoError(t, json.Unmarshal(raw, &back), "json: %s", raw)
		require.Equal(t, entry.ID, back.ID)
		require.Equal(t, entry.AgentID, back.AgentID)
		require.JSONEq(t, string(entry.Payload), string(back.Payload))
		require.True(t, base.Equal(back.CreatedAt))
		require.NotNil(t, back.LeasedAt)
		require.True(t, leased.Equal(*back.LeasedAt))
		require.Equal(t, "lease-1", back.LeaseID)
		require.True(t, back.ACKed)
	})

	t.Run("null decodes back to the never-expires zero time", func(t *testing.T) {
		// The RemoteStore / any JSON reader path: a null expiry must land on
		// the in-process never-expires representation, not an error and not a
		// spuriously-expired instant.
		var back InboxEntry
		require.NoError(t, json.Unmarshal([]byte(`{"id":"m","expires_at":null}`), &back))
		require.True(t, back.ExpiresAt.IsZero(),
			"null must decode to the zero time, got %s", back.ExpiresAt)
		require.Equal(t, "m", back.ID, "the null expiry must not abort the rest of the decode")
	})
}

// TestMessageExpiry_TriState pins the encoding itself: null for the zero time,
// the untouched time.Time JSON encoding for anything else.
func TestMessageExpiry_TriState(t *testing.T) {
	never, err := json.Marshal(MessageExpiry{})
	require.NoError(t, err)
	require.Equal(t, "null", string(never))

	at := time.Date(2026, 9, 17, 12, 47, 50, 0, time.UTC)
	finite, err := json.Marshal(MessageExpiry(at))
	require.NoError(t, err)
	require.Equal(t, `"2026-09-17T12:47:50Z"`, string(finite))

	// What time.Time itself would emit for a finite instant — the >0 wire
	// bytes must be exactly this (no reformatting regression).
	want, err := json.Marshal(at)
	require.NoError(t, err)
	require.Equal(t, string(want), string(finite))

	var decoded MessageExpiry
	require.NoError(t, json.Unmarshal([]byte("null"), &decoded))
	require.True(t, time.Time(decoded).IsZero(), "null must decode to the zero time, got %s", time.Time(decoded))
	require.NoError(t, json.Unmarshal(finite, &decoded))
	require.True(t, at.Equal(time.Time(decoded)), "round trip = %s, want %s", time.Time(decoded), at)
}
