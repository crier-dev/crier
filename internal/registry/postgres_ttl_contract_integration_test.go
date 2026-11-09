//go:build integration

package registry

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// INBOX-CRIER-001 against a REAL PostgreSQL (the repo's testcontainers
// harness): an unacknowledged entry past its agent's consumer contract is
// promoted into the dead-letter destination AND its sender receives exactly one
// MESSAGE_EXPIRED receipt, while the delivery record (id, payload, sender,
// creation instant) survives; an entry still inside the contract is untouched.
//
// This file deliberately uses ONLY APIs that existed before the change
// (store.Register/Deliver/Retrieve/ListDeadLetters, Handler.PurgeExpired, raw
// SQL) and writes the contract with SQL rather than a new Go field. That is what
// makes the RED arm runnable against the pre-change tree: there the `agents.
// consumer` column does not exist, the contract write fails (logged, not fatal),
// the never-expiring message is never promoted, and the sweep reports ZERO
// removals — the exact defect the row was filed from.
func TestInboxTTL_ExpiresAndReceipts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	store := newTestStore(t)
	db := openTestDB(ctx, t)
	defer db.Close()

	sender := newTestAgent(t, store, "ttl-sender")
	target := newTestAgent(t, store, "ttl-target")

	// The consumer contract: a harness reads this inbox and promises to keep
	// unacknowledged work for 10 minutes. Written via SQL so this exact test
	// compiles on the pre-change tree; there the column is absent, which is
	// logged and then demonstrated by the failing assertions below.
	contractJSON := `{"reader":"harness","read_cadence_secs":1,"ttl_seconds":600}`
	if _, err := db.ExecContext(ctx,
		`UPDATE agents SET consumer = $1::jsonb WHERE id = $2`, contractJSON, target.ID); err != nil {
		t.Logf("PRE-CHANGE SCHEMA: agents.consumer is absent (%v); continuing so the RED arm shows the defect at the assertions", err)
	}

	// The overdue message: delivered eleven days ago and NEVER expires by its
	// own TTL (the live census shape — a default 24h message TTL would already
	// have removed it).
	overdue := &InboxEntry{
		ID:         "ttl-msg-overdue",
		Payload:    []byte(`{"task":"build","attempt":1}`),
		Sender:     sender.ID,
		TTLSeconds: ptrInt(0),
	}
	require.NoError(t, store.Deliver(target.ID, overdue))
	_, err := db.ExecContext(ctx,
		`UPDATE inbox_entries SET created_at = now() - interval '11 days' WHERE id = $1`, overdue.ID)
	require.NoError(t, err)

	// The under-TTL message: fresh, also never-expiring, must survive.
	fresh := &InboxEntry{
		ID:         "ttl-msg-fresh",
		Payload:    []byte(`{"task":"fresh"}`),
		Sender:     sender.ID,
		TTLSeconds: ptrInt(0),
	}
	require.NoError(t, store.Deliver(target.ID, fresh))

	handler := NewHandler(store)
	removed := handler.PurgeExpired()
	if removed != 1 {
		// This is the RED arm's evidence: pre-change, the sweep has no notion
		// of a contract, so the eleven-day entry stays and no receipt exists.
		var inboxCount, deadCount int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FROM inbox_entries WHERE agent_id = $1`, target.ID).Scan(&inboxCount))
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FROM dead_letters WHERE agent_id = $1`, target.ID).Scan(&deadCount))
		t.Fatalf("sweep removed %d entries, want 1 (inbox still holds %d, dead letters %d): "+
			"the entry past its consumer contract stayed in the inbox and no MESSAGE_EXPIRED receipt was produced",
			removed, inboxCount, deadCount)
	}

	// The inbox keeps ONLY the message still inside its contract.
	msgs, _, err := store.Retrieve(target.ID, 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "ttl-msg-fresh", msgs[0].ID)

	// The delivery record survives, cross-referenced by the ORIGINAL id.
	dead, err := store.ListDeadLetters(target.ID, 10)
	require.NoError(t, err)
	require.Len(t, dead, 1)
	require.Equal(t, "ttl-msg-overdue", dead[0].MessageID)
	require.Equal(t, target.ID, dead[0].AgentID)
	require.Equal(t, sender.ID, dead[0].Sender)
	// The literal is deliberate: the constant is new and this file must compile
	// on the pre-change tree. The SQLite unit test pins the constant's value.
	require.Equal(t, "consumer_contract_expired", dead[0].Reason)
	require.JSONEq(t, `{"task":"build","attempt":1}`, string(dead[0].Payload))
	require.True(t, dead[0].ExpiredAt.After(dead[0].CreatedAt),
		"the recorded expiry is the contract deadline that elapsed (created_at + 600s)")
	require.WithinDuration(t, dead[0].CreatedAt.Add(10*time.Minute), dead[0].ExpiredAt, 2*time.Second)

	// The sender receives exactly one MESSAGE_EXPIRED receipt naming the id and
	// the dead-letter path it can retrieve the body from.
	receipts, _, err := store.Retrieve(sender.ID, 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, receipts, 1, "exactly one receipt")
	var body struct {
		Kind           string `json:"kind"`
		Code           string `json:"code"`
		MessageID      string `json:"message_id"`
		Target         string `json:"target"`
		DeadLettered   bool   `json:"dead_lettered"`
		DeadLetterPath string `json:"dead_letter_path"`
		Reason         string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(receipts[0].Payload, &body))
	require.Equal(t, CodeMessageExpired, body.Code)
	require.Equal(t, "error", body.Kind)
	require.Equal(t, "ttl-msg-overdue", body.MessageID)
	require.Equal(t, target.ID, body.Target)
	require.True(t, body.DeadLettered)
	require.Equal(t, DeadLettersPath(target.ID), body.DeadLetterPath)
	require.Equal(t, "consumer_contract_expired", body.Reason)

	// A second sweep is a no-op: exactly-once.
	require.Equal(t, 0, handler.PurgeExpired())
	deadAgain, err := store.ListDeadLetters(target.ID, 10)
	require.NoError(t, err)
	require.Len(t, deadAgain, 1)
}
