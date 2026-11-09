package registry

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// consumer_contract_test.go — INBOX-CRIER-001, the parts a fast unit run can
// prove without a database container:
//
//   - the contract resolution table (default / override / never / cleared);
//   - the stale-consumer detector against a REAL 11-day-old entry (the live
//     defect this row was filed from) and a fresh one;
//   - the exact /agents/{id}/inbox/stats key names;
//   - one end-to-end sweep on the SQLite backend: overdue entry → dead letter
//     + MESSAGE_EXPIRED receipt, under-TTL entry untouched, delivery record
//     (id, payload, sender, created_at) preserved in the dead letter.
//
// The Postgres arm of the same end-to-end proof lives in
// postgres_ttl_contract_integration_test.go (build tag `integration`).

func contractInt(n int) *int { return &n }

// TestInboxTTL_ContractTable pins the resolution rule and the validation
// boundary. The table is the whole contract in one place: an absent contract and
// an absent override both inherit the deployment default, an explicit override
// is honoured to the second, and an explicit 0 is "never" — an opt-in, never a
// default.
func TestInboxTTL_ContractTable(t *testing.T) {
	t.Parallel()

	def := 24 * time.Hour
	cases := []struct {
		name     string
		contract *ConsumerContract
		want     time.Duration
	}{
		{"no contract → deployment default", nil, def},
		{"contract without override → deployment default",
			&ConsumerContract{Reader: ConsumerReaderHarness}, def},
		{"per-agent override honoured",
			&ConsumerContract{Reader: ConsumerReaderAgent, TTLSeconds: contractInt(120)}, 120 * time.Second},
		{"explicit 0 → never expires (opt-in)",
			&ConsumerContract{Reader: ConsumerReaderHarness, TTLSeconds: contractInt(0)}, 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, tc.contract.EffectiveTTL(def))
		})
	}

	// The documented default is the message lifetime that already applied, so
	// introducing the contract changes no existing agent's observable expiry.
	require.Equal(t, DefaultMessageTTL, DefaultConsumerContractTTL,
		"the contract default must be the 24h message lifetime, not a new bound")

	// Validation: a contract that cannot be honored is refused, never clamped.
	require.Error(t, (&ConsumerContract{}).Validate(), "reader is required")
	require.Error(t, (&ConsumerContract{Reader: "robot"}).Validate(), "reader is a closed vocabulary")
	require.Error(t, (&ConsumerContract{Reader: ConsumerReaderAgent, ReadCadenceSecs: -1}).Validate())
	require.Error(t, (&ConsumerContract{Reader: ConsumerReaderAgent, TTLSeconds: contractInt(-1)}).Validate())
	require.NoError(t, (&ConsumerContract{
		Reader:          ConsumerReaderHarness,
		ReadCadenceSecs: 60,
		TTLSeconds:      contractInt(3600),
	}).Validate())

	// A Handler that was never configured uses the documented default; an
	// explicit zero is the deployment opt-out and must survive.
	require.Equal(t, DefaultConsumerContractTTL, NewHandler(NewMemoryStore()).resolvedConsumerContractTTL())
	optedOut := NewHandler(NewMemoryStore())
	optedOut.SetConsumerContractTTL(0)
	require.Equal(t, time.Duration(0), optedOut.resolvedConsumerContractTTL())
}

// TestStaleConsumerDetector reproduces the live defect's shape: an
// unacknowledged entry ELEVEN DAYS old under a one-hour contract must be
// flagged, a fresh one must not.
func TestStaleConsumerDetector(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	handler, router := setupRouterWithHandler(store)

	require.NoError(t, store.Register(&Agent{
		ID:       "mdp-worker",
		Consumer: &ConsumerContract{Reader: ConsumerReaderHarness, ReadCadenceSecs: 900, TTLSeconds: contractInt(3600)},
	}))
	require.NoError(t, store.Register(&Agent{
		ID:       "fresh-worker",
		Consumer: &ConsumerContract{Reader: ConsumerReaderAgent, ReadCadenceSecs: 900, TTLSeconds: contractInt(3600)},
	}))
	require.NoError(t, store.Register(&Agent{ID: "foreman"}))

	// The 11-day-old message: never-expiring (the census shape — a default
	// 24h TTL would already have failed to explain it), unacked.
	require.NoError(t, store.Deliver("mdp-worker", &InboxEntry{
		ID:         "msg-11-days",
		Payload:    []byte(`{"task":"build"}`),
		Sender:     "foreman",
		CreatedAt:  time.Now().UTC().Add(-11 * 24 * time.Hour),
		TTLSeconds: contractInt(0),
	}))
	require.NoError(t, store.Deliver("fresh-worker", &InboxEntry{
		ID:      "msg-fresh",
		Payload: []byte(`{"task":"build"}`),
		Sender:  "foreman",
	}))

	stale, err := store.StaleConsumers(time.Now().UTC(), handler.resolvedConsumerContractTTL())
	require.NoError(t, err)
	require.Len(t, stale, 1, "exactly the 11-day consumer is stale")
	require.Equal(t, "mdp-worker", stale[0].AgentID)
	require.Equal(t, ConsumerReaderHarness, stale[0].Reader)
	require.Equal(t, time.Hour, stale[0].ContractTTL)
	require.Equal(t, 1, stale[0].Unacked)
	require.Greater(t, time.Since(stale[0].Oldest), 10*24*time.Hour)

	// The same fact on the operator surface.
	staleStats := getStats(t, router, "/agents/mdp-worker/inbox/stats")
	require.True(t, staleStats.Stale, "the 11-day consumer must be flagged on stats")
	require.Equal(t, int64(3600), staleStats.ContractTTLSecs)
	require.Equal(t, 1, staleStats.UnackedCount)
	require.NotNil(t, staleStats.OldestUnacked)

	freshStats := getStats(t, router, "/agents/fresh-worker/inbox/stats")
	require.False(t, freshStats.Stale, "a fresh message is not a stale consumer")
	require.Equal(t, int64(3600), freshStats.ContractTTLSecs)
	require.Equal(t, 1, freshStats.UnackedCount)
}

// TestInboxStats_NamesContractAndOldest pins the exact wire key names the row
// requires, read from the raw JSON (a struct decode cannot tell "key absent"
// from "key zero").
func TestInboxStats_NamesContractAndOldest(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	_, router := setupRouterWithHandler(store)
	require.NoError(t, store.Register(&Agent{
		ID:       "stats-agent",
		Consumer: &ConsumerContract{Reader: ConsumerReaderHarness, TTLSeconds: contractInt(3600)},
	}))
	oldest := time.Now().UTC().Add(-11 * 24 * time.Hour).Truncate(time.Second)
	require.NoError(t, store.Deliver("stats-agent", &InboxEntry{
		ID:         "msg-old",
		Payload:    []byte(`{"n":1}`),
		CreatedAt:  oldest,
		TTLSeconds: contractInt(0),
	}))

	rec := doInboxRequest(t, router, http.MethodGet, "/agents/stats-agent/inbox/stats", "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	t.Logf("GET /agents/stats-agent/inbox/stats → %s", rec.Body.String())

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	for _, key := range []string{"queue_depth", "leased_count", "oldest_age_ms",
		"oldest_unacked", "contract_ttl_secs", "stale", "unacked_count"} {
		require.Contains(t, raw, key, "stats must name %q", key)
	}

	var gotOldest time.Time
	require.NoError(t, json.Unmarshal(raw["oldest_unacked"], &gotOldest))
	require.WithinDuration(t, oldest, gotOldest, time.Second,
		"oldest_unacked is the exact creation instant of the oldest unacked message")
	var contractTTL int64
	require.NoError(t, json.Unmarshal(raw["contract_ttl_secs"], &contractTTL))
	require.Equal(t, int64(3600), contractTTL)
	var stale bool
	require.NoError(t, json.Unmarshal(raw["stale"], &stale))
	require.True(t, stale)
	var unacked int
	require.NoError(t, json.Unmarshal(raw["unacked_count"], &unacked))
	require.Equal(t, 1, unacked)

	// An inbox with nothing unacknowledged reports oldest_unacked as JSON null
	// — never the zero time (DF-CRIER-182's rule, applied to this field).
	require.NoError(t, store.Register(&Agent{ID: "empty-agent"}))
	emptyRec := doInboxRequest(t, router, http.MethodGet, "/agents/empty-agent/inbox/stats", "")
	require.Equal(t, http.StatusOK, emptyRec.Code)
	require.Contains(t, emptyRec.Body.String(), `"oldest_unacked":null`)
	require.NotContains(t, emptyRec.Body.String(), zeroTimeRFC3339)

	// No contract declared → the deployment default is named, and the explicit
	// 0 opt-out is named as 0 (never "unset").
	require.Equal(t, int64(86400), getStats(t, router, "/agents/empty-agent/inbox/stats").ContractTTLSecs)
	require.NoError(t, store.Register(&Agent{
		ID:       "opted-out",
		Consumer: &ConsumerContract{Reader: ConsumerReaderAgent, TTLSeconds: contractInt(0)},
	}))
	require.Equal(t, int64(0), getStats(t, router, "/agents/opted-out/inbox/stats").ContractTTLSecs)
	require.False(t, getStats(t, router, "/agents/opted-out/inbox/stats").Stale)
}

// TestInboxTTL_DefaultContractBoundsNeverExpiringMessage is the LIVE defect at
// unit level: an agent that never declared a contract still gets the deployment
// default (24h), so the eleven-day, ttl_seconds=0 entry the census found is
// expired and receipted with no per-agent configuration at all.
func TestInboxTTL_DefaultContractBoundsNeverExpiringMessage(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore()
	handler, router := setupRouterWithHandler(store)
	require.NoError(t, store.Register(&Agent{ID: "mdp-worker"}))
	require.NoError(t, store.Register(&Agent{ID: "mdp-collector"}))

	require.NoError(t, store.Deliver("mdp-worker", &InboxEntry{
		ID:         "msg-live-defect",
		Payload:    []byte(`{"task":"build"}`),
		Sender:     "mdp-collector",
		CreatedAt:  time.Now().UTC().Add(-11 * 24 * time.Hour),
		TTLSeconds: contractInt(0), // the message itself never expires
	}))

	// The detector names it under the deployment default, not an override.
	stale, err := store.StaleConsumers(time.Now().UTC(), handler.resolvedConsumerContractTTL())
	require.NoError(t, err)
	require.Len(t, stale, 1)
	require.Equal(t, DefaultConsumerContractTTL, stale[0].ContractTTL)

	require.Equal(t, 1, handler.PurgeExpired(), "the default contract bounds the never-expiring message")

	dead, err := store.ListDeadLetters("mdp-worker", 10)
	require.NoError(t, err)
	require.Len(t, dead, 1)
	require.Equal(t, "msg-live-defect", dead[0].MessageID)
	require.Equal(t, DeadLetterReasonContractExpired, dead[0].Reason)

	receipts := senderInbox(t, store, "mdp-collector")
	require.Len(t, receipts, 1)
	require.Equal(t, CodeMessageExpired, receipts[0].Code)
	require.Equal(t, "msg-live-defect", receipts[0].MessageID)

	stats := getStats(t, router, "/agents/mdp-worker/inbox/stats")
	require.Equal(t, int64(DefaultConsumerContractTTL/time.Second), stats.ContractTTLSecs)
	require.Zero(t, stats.UnackedCount, "the overdue message left the inbox")
	require.False(t, stats.Stale)
}

// TestInboxTTL_SweepExpiresSQLite is the same end-to-end proof as the named
// Postgres test, on the backend that needs no container: deliver past the
// contract → the sweep dead-letters it, receipts the sender, removes it from
// the inbox, and leaves the delivery record's identity intact; an entry still
// inside its contract is untouched.
func TestInboxTTL_SweepExpiresSQLite(t *testing.T) {
	t.Parallel()

	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "registry.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	handler, router := setupRouterWithHandler(store)

	require.NoError(t, store.Register(&Agent{ID: "planner"}))
	require.NoError(t, store.Register(&Agent{
		ID:       "mdp-worker",
		Consumer: &ConsumerContract{Reader: ConsumerReaderHarness, ReadCadenceSecs: 1, TTLSeconds: contractInt(60)},
	}))

	deliveredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	require.NoError(t, store.Deliver("mdp-worker", &InboxEntry{
		ID:         "msg-overdue",
		Payload:    []byte(`{"task":"build","attempt":1}`),
		Sender:     "planner",
		CreatedAt:  deliveredAt,
		TTLSeconds: contractInt(0), // the message itself never expires
	}))
	require.NoError(t, store.Deliver("mdp-worker", &InboxEntry{
		ID:         "msg-under-ttl",
		Payload:    []byte(`{"task":"fresh"}`),
		Sender:     "planner",
		TTLSeconds: contractInt(0),
	}))

	require.Equal(t, 1, handler.PurgeExpired(), "one message is past the contract")

	// The inbox keeps only the under-TTL message.
	msgs, _, err := store.Retrieve("mdp-worker", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "msg-under-ttl", msgs[0].ID, "the entry under its contract is untouched")

	// The delivery record survives in the dead-letter destination, cross-
	// referenced by the ORIGINAL message id, with its payload and provenance.
	dead, err := store.ListDeadLetters("mdp-worker", 10)
	require.NoError(t, err)
	require.Len(t, dead, 1)
	require.Equal(t, "msg-overdue", dead[0].MessageID)
	require.Equal(t, "mdp-worker", dead[0].AgentID)
	require.Equal(t, "planner", dead[0].Sender)
	require.Equal(t, DeadLetterReasonContractExpired, dead[0].Reason)
	require.JSONEq(t, `{"task":"build","attempt":1}`, string(dead[0].Payload))
	require.WithinDuration(t, deliveredAt, dead[0].CreatedAt, time.Millisecond,
		"the delivery instant survives exactly")
	require.WithinDuration(t, deliveredAt.Add(60*time.Second), dead[0].ExpiredAt, time.Millisecond,
		"the contract deadline that elapsed is recorded, not the message's never-expiry")

	// The sender gets exactly one MESSAGE_EXPIRED receipt naming that id.
	receipts := senderInbox(t, store, "planner")
	require.Len(t, receipts, 1, "exactly one receipt")
	require.Equal(t, CodeMessageExpired, receipts[0].Code)
	require.Equal(t, "error", receipts[0].Kind)
	require.Equal(t, "msg-overdue", receipts[0].MessageID)
	require.Equal(t, "mdp-worker", receipts[0].Target)
	require.True(t, receipts[0].DeadLettered)
	require.Equal(t, DeadLettersPath("mdp-worker"), receipts[0].DeadLetterPath)
	require.Equal(t, DeadLetterReasonContractExpired, receipts[0].Reason)

	// The stats surface now reports the surviving population, not stale.
	stats := getStats(t, router, "/agents/mdp-worker/inbox/stats")
	require.Equal(t, int64(60), stats.ContractTTLSecs)
	require.Equal(t, 1, stats.UnackedCount)
	require.False(t, stats.Stale, "the overdue message is gone; the fresh one is inside the contract")

	// The dead-letter route answers the same record (the operator surface).
	require.Len(t, deadLetters(t, router, "mdp-worker"), 1)

	// A second sweep is a no-op: nothing is removed twice, so no second dead
	// letter and no second receipt can be produced.
	require.Equal(t, 0, handler.PurgeExpired())
	require.Len(t, deadLetters(t, router, "mdp-worker"), 1, "exactly one dead letter survives the second sweep")
}
