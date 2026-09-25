package registry

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/require"
)

// ttl_test.go pins the delivery-TTL contract wired by DF-CRIER-37: the
// `ttl_seconds` field documented on POST /agents/{id}/inbox in openapi.yaml
// was parsed nowhere and every message got a hard-coded 24h expiry.
//
// The contract now:
//
//   - absent           → 24h default (unchanged pre-existing behavior);
//   - ttl_seconds = 0  → never expires: internally ExpiresAt stays the zero
//     time (0001-01-01T00:00:00Z), which every consumption path treats as
//     "no expiry" — retrieve, stats and purge must all skip the expiry check —
//     while the WIRE renders it as JSON null (DF-CRIER-182): the key is
//     present and its value is null, never the zero time;
//   - ttl_seconds > 0  → CreatedAt + n seconds, RFC 3339 on the wire;
//   - ttl_seconds < 0, or so large it would overflow time.Duration → 400.
//
// The expiry is read back through the SAME observable surface a client uses
// (deliver response `expires_at`, then GET /agents/{id}/inbox), so a fix that
// set the store field but left the response at the old default would fail.

// zeroTimeRFC3339 is the OLD never-expires wire representation (openapi.yaml
// documented InboxEntry.expires_at as "0001-01-01T00:00:00Z when ttl_seconds
// was 0"). Since DF-CRIER-182 it must not appear anywhere in a response body:
// the never-expires state is JSON null instead.
const zeroTimeRFC3339 = "0001-01-01T00:00:00Z"

// deliverBody builds a deliver request body, omitting ttl_seconds entirely
// when ttl is nil (absent ≠ 0).
func deliverBody(t *testing.T, payload string, ttl any) string {
	t.Helper()
	body := map[string]any{"payload": json.RawMessage(payload)}
	if ttl != nil {
		body["ttl_seconds"] = ttl
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return string(raw)
}

// deliverResponseBody is the wire shape of a 201 delivery. ExpiresAt decodes
// to nil for BOTH the absent (webhook) form and the present-and-null
// (never-expires) form — a *time.Time cannot tell them apart, so the tests that
// care read the raw body (wireField).
type deliverResponseBody struct {
	ID        string     `json:"id"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// deliverAndDecode posts a delivery and returns the recorder plus the decoded
// 201 body.
func deliverAndDecode(t *testing.T, router http.Handler, agentID, body string) (*httptest.ResponseRecorder, deliverResponseBody) {
	t.Helper()
	rec := doInboxRequest(t, router, http.MethodPost, "/agents/"+agentID+"/inbox", body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp deliverResponseBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body: %s", rec.Body.String())
	require.NotEmpty(t, resp.ID)
	return rec, resp
}

// ---------------------------------------------------------------------------
// Handler → store: the requested TTL reaches the stored message
// ---------------------------------------------------------------------------

func TestDeliver_TTLSeconds_HourAppliesToStoredMessage(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	_, resp := deliverAndDecode(t, router, "agent-1", deliverBody(t, `{"n":1}`, 3600))

	// The deliver response reports the resolved expiry, not the 24h default.
	require.NotNil(t, resp.ExpiresAt, "a stored delivery must report expires_at")
	require.InDelta(t, 3600, time.Until(*resp.ExpiresAt).Seconds(), 5,
		"expires_at = %s, want ≈ now+1h (the 24h default means ttl_seconds is still ignored)", resp.ExpiresAt)

	// The stored message carries the same expiry, and its LIFETIME (not just
	// its instant) is the requested hour.
	msgs, _ := decodeRetrieve(t, doInboxRequest(t, router, http.MethodGet, "/agents/agent-1/inbox?max=10", ""))
	require.Len(t, msgs, 1)
	require.InDelta(t, 3600, msgs[0].ExpiresAt.Sub(msgs[0].CreatedAt).Seconds(), 5,
		"stored lifetime = %s, want 1h", msgs[0].ExpiresAt.Sub(msgs[0].CreatedAt))
	require.InDelta(t, 0, msgs[0].ExpiresAt.Sub(*resp.ExpiresAt).Seconds(), 2,
		"the response expiry and the stored expiry must agree")
}

func TestDeliver_TTLSeconds_AbsentKeeps24hDefault(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	_, resp := deliverAndDecode(t, router, "agent-1", deliverBody(t, `{"n":1}`, nil))

	require.NotNil(t, resp.ExpiresAt)
	require.InDelta(t, DefaultMessageTTL.Seconds(), time.Until(*resp.ExpiresAt).Seconds(), 30,
		"absent ttl_seconds must keep the 24h default, got %s", resp.ExpiresAt)
	require.Equal(t, 24*time.Hour, DefaultMessageTTL, "the documented default is 24h")

	msgs, _ := decodeRetrieve(t, doInboxRequest(t, router, http.MethodGet, "/agents/agent-1/inbox?max=10", ""))
	require.Len(t, msgs, 1)
	require.InDelta(t, (24 * time.Hour).Seconds(), msgs[0].ExpiresAt.Sub(msgs[0].CreatedAt).Seconds(), 5)
}

func TestDeliver_TTLSeconds_ZeroNeverExpires(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	rec, resp := deliverAndDecode(t, router, "agent-1", deliverBody(t, `{"n":1}`, 0))

	// DF-CRIER-182: the never-expires state is JSON null on the wire — the key
	// is PRESENT and its value is null (a *time.Time decodes null to nil, which
	// is exactly why the raw form is asserted below it).
	require.Nil(t, resp.ExpiresAt, "expires_at = %v, want null for ttl_seconds=0", resp.ExpiresAt)
	raw, present := wireField(t, rec.Body.Bytes(), "expires_at")
	require.True(t, present, "expires_at is present (explicitly) for ttl_seconds=0 — body: %s", rec.Body.String())
	require.JSONEq(t, `null`, string(raw), "expires_at = %s, want null", raw)
	require.NotContains(t, rec.Body.String(), zeroTimeRFC3339,
		"the zero time is no longer a wire value — body: %s", rec.Body.String())

	// The internal representation is unchanged: retrieve decodes the null back
	// to the zero ExpiresAt (the same round trip RemoteStore depends on).
	msgs, _ := decodeRetrieve(t, doInboxRequest(t, router, http.MethodGet, "/agents/agent-1/inbox?max=10", ""))
	require.Len(t, msgs, 1, "a never-expiring message must be retrievable")
	require.True(t, msgs[0].ExpiresAt.IsZero(), "in-process ExpiresAt = %s, want the zero time", msgs[0].ExpiresAt)

	// Premise for the store-side guards: the zero time is before now, so a
	// plain `ExpiresAt.Before(now)` check would call this message expired —
	// which is exactly why it must never reach a client as a timestamp.
	require.True(t, msgs[0].ExpiresAt.Before(time.Now()))

	stats := doInboxRequest(t, router, http.MethodGet, "/agents/agent-1/inbox/stats", "")
	require.Equal(t, http.StatusOK, stats.Code)
	require.Contains(t, stats.Body.String(), `"queue_depth":1`,
		"a never-expiring message must count as queued, not as expired: %s", stats.Body.String())
	require.Equal(t, 0, store.PurgeExpired(), "purge must not remove a never-expiring message")
}

func TestDeliver_TTLSeconds_ZeroSurvivesPurgeWhileExpiredSiblingIsRemoved(t *testing.T) {
	store := NewMemoryStore()
	registerTestAgent(t, store)

	neverTTL := 0
	never := &InboxEntry{Payload: json.RawMessage(`{"n":"never"}`), TTLSeconds: &neverTTL}
	require.NoError(t, store.Deliver("agent-1", never))
	require.True(t, never.ExpiresAt.IsZero())

	// A sibling that really has expired — the purge must still collect it, so
	// the never-expiry guard cannot be a blanket "skip everything".
	expiring := &InboxEntry{
		Payload:   json.RawMessage(`{"n":"soon"}`),
		ExpiresAt: time.Now().Add(20 * time.Millisecond),
	}
	require.NoError(t, store.Deliver("agent-1", expiring))

	time.Sleep(60 * time.Millisecond)
	require.Equal(t, 1, store.PurgeExpired(), "exactly the expired message is removed")

	depth, _, _, err := store.Stats("agent-1")
	require.NoError(t, err)
	require.Equal(t, 1, depth)

	msgs, _, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, never.ID, msgs[0].ID, "the surviving message is the never-expiring one")
	require.True(t, msgs[0].ExpiresAt.IsZero())
}

func TestDeliver_TTLSeconds_RejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		ttl  int
		want string
	}{
		{name: "negative", ttl: -1, want: "ttl_seconds must not be negative"},
		// writeJSON HTML-escapes, so the bound renders as \u003c= — assert on
		// the field name and the bound itself.
		{name: "overflow", ttl: int(maxTTLSeconds) + 1, want: "9223372036"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := setupTestStore(t)
			registerTestAgent(t, store)
			router := setupRouter(store)

			rec := doInboxRequest(t, router, http.MethodPost, "/agents/agent-1/inbox", deliverBody(t, `{"n":1}`, tc.ttl))
			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			require.Contains(t, rec.Body.String(), tc.want)

			// A rejected delivery must not be stored.
			depth, _, _, err := store.Stats("agent-1")
			require.NoError(t, err)
			require.Zero(t, depth, "a 400 delivery must not enqueue a message")
		})
	}
}

// ---------------------------------------------------------------------------
// resolveMessageExpiry — the one place the requested lifetime becomes an instant
// ---------------------------------------------------------------------------

func TestResolveMessageExpiry_Contract(t *testing.T) {
	base := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	zero, hour, negative := 0, 3600, -1
	explicit := base.Add(72 * time.Hour)

	cases := []struct {
		name      string
		entry     *InboxEntry
		want      time.Time
		wantError bool
	}{
		{name: "absent -> 24h default", entry: &InboxEntry{CreatedAt: base}, want: base.Add(24 * time.Hour)},
		{name: "zero -> never", entry: &InboxEntry{CreatedAt: base, TTLSeconds: &zero}, want: time.Time{}},
		{name: "positive -> that many seconds", entry: &InboxEntry{CreatedAt: base, TTLSeconds: &hour}, want: base.Add(time.Hour)},
		{name: "negative -> error", entry: &InboxEntry{CreatedAt: base, TTLSeconds: &negative}, wantError: true},
		{name: "explicit ExpiresAt wins over zero ttl", entry: &InboxEntry{CreatedAt: base, ExpiresAt: explicit, TTLSeconds: &zero}, want: explicit},
		{name: "explicit ExpiresAt wins over absent ttl", entry: &InboxEntry{CreatedAt: base, ExpiresAt: explicit}, want: explicit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := resolveMessageExpiry(tc.entry)
			if tc.wantError {
				require.Error(t, err)
				require.True(t, errors.Is(err, ErrInvalidStoreInput))
				return
			}
			require.NoError(t, err)
			require.True(t, tc.want.Equal(tc.entry.ExpiresAt),
				"ExpiresAt = %s, want %s", tc.entry.ExpiresAt, tc.want)

			// Idempotent: the store re-resolves the entry the handler already
			// resolved, and that must not shift the expiry.
			require.NoError(t, resolveMessageExpiry(tc.entry))
			require.True(t, tc.want.Equal(tc.entry.ExpiresAt))
		})
	}
}

func TestDeliverRequest_TTLSeconds_AbsentStaysAbsentInForwardedBody(t *testing.T) {
	// A federation forward re-marshals the decoded request, so an absent
	// ttl_seconds must not be materialized as 0 (which would mean "never
	// expire" downstream) or as null.
	absent, err := json.Marshal(deliverRequest{Payload: json.RawMessage(`{}`)})
	require.NoError(t, err)
	require.NotContains(t, string(absent), "ttl_seconds",
		"absent ttl_seconds must stay absent when the request is forwarded")

	zero := 0
	present, err := json.Marshal(deliverRequest{Payload: json.RawMessage(`{}`), TTLSeconds: &zero})
	require.NoError(t, err)
	require.Contains(t, string(present), `"ttl_seconds":0`)
}

// ---------------------------------------------------------------------------
// MemoryStore — delivery-level validation
// ---------------------------------------------------------------------------

func TestMemoryStore_Deliver_NegativeTTLRejected(t *testing.T) {
	store := NewMemoryStore()
	registerTestAgent(t, store)

	negative := -5
	err := store.Deliver("agent-1", &InboxEntry{Payload: json.RawMessage(`{}`), TTLSeconds: &negative})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput), "got %v", err)

	depth, _, _, serr := store.Stats("agent-1")
	require.NoError(t, serr)
	require.Zero(t, depth)
}

// ---------------------------------------------------------------------------
// RemoteStore — a proxied delivery keeps the requested lifetime
// ---------------------------------------------------------------------------

func TestRemoteStore_Deliver_ForwardsTTLSeconds(t *testing.T) {
	cases := []struct {
		name  string
		ttl   *int
		want  string
		avoid string
	}{
		{name: "zero (never) is forwarded", ttl: func() *int { v := 0; return &v }(), want: `"ttl_seconds":0`},
		{name: "positive is forwarded", ttl: func() *int { v := 900; return &v }(), want: `"ttl_seconds":900`},
		{name: "absent is not sent", ttl: nil, avoid: "ttl_seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				gotBody = string(raw)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":"msg-1"}`))
			}))
			defer srv.Close()

			rs := NewRemoteStore(srv.URL, "bob", "token")
			entry := &InboxEntry{Payload: json.RawMessage(`{"n":1}`), TTLSeconds: tc.ttl}
			require.NoError(t, rs.Deliver("bob", entry))
			require.Equal(t, "msg-1", entry.ID, "the server-assigned id still lands on the entry")

			if tc.want != "" {
				require.Contains(t, gotBody, tc.want, "forwarded body: %s", gotBody)
			}
			if tc.avoid != "" {
				require.NotContains(t, gotBody, tc.avoid, "forwarded body: %s", gotBody)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PostgresStore — never-expiry is written as `infinity` and read back as zero
// ---------------------------------------------------------------------------

// infinityArg matches the expiry argument the store writes for a
// never-expiring message: a pgtype.Timestamptz carrying Infinity (encoded by
// pgx as the timestamptz literal `infinity`). Kept tolerant about the value
// shape so it fails loudly only when the encoding is wrong, not when pgxmock
// hands back a different representation of the same value.
type infinityArg struct{}

func (infinityArg) Match(v any) bool {
	switch got := v.(type) {
	case pgtype.Timestamptz:
		return got.Valid && got.InfinityModifier == pgtype.Infinity
	case *pgtype.Timestamptz:
		return got != nil && got.Valid && got.InfinityModifier == pgtype.Infinity
	case string:
		return got == "infinity"
	default:
		return false
	}
}

func TestPostgresStoreUnit_Deliver_NeverExpiresWritesInfinity(t *testing.T) {
	s, mock := newMockStore(t)
	zero := 0
	entry := &InboxEntry{Payload: []byte(`{"n":1}`), TTLSeconds: &zero}

	mock.ExpectExec(`INSERT INTO inbox_entries`).
		WithArgs(pgxmock.AnyArg(), "agent", entry.Payload, nil, nil,
			pgxmock.AnyArg(), infinityArg{}, nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	require.NoError(t, s.Deliver("agent", entry))
	require.True(t, entry.ExpiresAt.IsZero(), "a never-expiring message keeps the zero ExpiresAt")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Deliver_TTLSeconds_HourIsWritten(t *testing.T) {
	s, mock := newMockStore(t)
	hour := 3600
	entry := &InboxEntry{Payload: []byte(`{"n":1}`), TTLSeconds: &hour}

	mock.ExpectExec(`INSERT INTO inbox_entries`).
		WithArgs(pgxmock.AnyArg(), "agent", entry.Payload, nil, nil,
			pgxmock.AnyArg(), pgxmock.AnyArg(), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	require.NoError(t, s.Deliver("agent", entry))
	require.InDelta(t, 3600, entry.ExpiresAt.Sub(entry.CreatedAt).Seconds(), 5,
		"stored lifetime = %s, want 1h", entry.ExpiresAt.Sub(entry.CreatedAt))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Deliver_NegativeTTLRejectedBeforeSQL(t *testing.T) {
	s, mock := newMockStore(t)
	negative := -1

	err := s.Deliver("agent", &InboxEntry{Payload: []byte(`{}`), TTLSeconds: &negative})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput), "got %v", err)
	// No expectation was registered: any INSERT attempt fails this assertion.
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPgTimestamptz_RoundTrip(t *testing.T) {
	never := pgTimestamptz(time.Time{})
	require.True(t, never.Valid)
	require.Equal(t, pgtype.Infinity, never.InfinityModifier)
	value, err := never.Value()
	require.NoError(t, err)
	require.Equal(t, "infinity", value, "pgx must encode never-expiry as the timestamptz literal infinity")

	finite := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	got := pgTimestamptz(finite)
	require.Equal(t, pgtype.Finite, got.InfinityModifier)
	require.True(t, finite.Equal(got.Time))

	// Read side: infinity decodes to the zero time (never), a finite value
	// round-trips, and -infinity (never written by crier) reads as expired.
	require.True(t, expiryFromTimestamptz(never).IsZero())
	require.True(t, finite.Equal(expiryFromTimestamptz(got)))
	require.False(t, expiryFromTimestamptz(pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.NegativeInfinity}).IsZero())
}

func TestPostgresStoreUnit_Retrieve_InfinityExpiryScansAsZero(t *testing.T) {
	s, mock := newMockStore(t)
	now := time.Now().UTC()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM agents`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"?2"}).AddRow(1))
	// A never-expiring message comes back from Postgres as timestamptz
	// `infinity`; scanning it must not error and must normalize to zero.
	rows := pgxmock.NewRows([]string{"id", "agent_id", "payload", "sender", "idempotency_key", "created_at", "expires_at", "namespace"}).
		AddRow("msg-never", "agent", []byte(`{}`), "", "", now, pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.Infinity}, "")
	mock.ExpectQuery(`FOR UPDATE SKIP LOCKED`).
		WithArgs("agent", pgxmock.AnyArg(), 10).
		WillReturnRows(rows)
	mock.ExpectExec(`UPDATE inbox_entries`).
		WithArgs("agent", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), []string{"msg-never"}).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	msgs, leaseID, err := s.Retrieve("agent", 30*time.Second, 10)
	require.NoError(t, err)
	require.NotEmpty(t, leaseID)
	require.Len(t, msgs, 1)
	require.True(t, msgs[0].ExpiresAt.IsZero(),
		"infinity must decode to the never-expires zero time, got %s", msgs[0].ExpiresAt)
	require.NoError(t, mock.ExpectationsWereMet())
}
