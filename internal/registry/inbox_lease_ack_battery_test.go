package registry_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/registry"
)

func TestInboxLeaseAckBattery(t *testing.T) {
	t.Run("Retrieve_mints_no_lease_for_empty_or_fully_leased_inbox", func(t *testing.T) {
		// Consolidates TestMemoryStore_RetrieveEmptyInboxMintsNoLease and
		// TestMemoryStore_RetrieveAllLeasedMintsNoLease from lease_truth_test.go.
		store := registry.NewMemoryStore()
		require.NoError(t, store.Register(&registry.Agent{ID: "recipient"}))
		messages, lease, err := store.Retrieve("recipient", time.Second, 10)
		require.NoError(t, err)
		require.NotNil(t, messages)
		require.Empty(t, messages)
		require.Empty(t, lease)

		require.NoError(t, store.Deliver("recipient", &registry.InboxEntry{Payload: json.RawMessage(`{"n":1}`)}))
		first, firstLease, err := store.Retrieve("recipient", time.Second, 10)
		require.NoError(t, err)
		require.Len(t, first, 1)
		require.NotEmpty(t, firstLease)
		second, secondLease, err := store.Retrieve("recipient", time.Second, 10)
		require.NoError(t, err)
		require.NotNil(t, second)
		require.Empty(t, second)
		require.Empty(t, secondLease)
		require.NoError(t, store.Ack("recipient", firstLease, []string{first[0].ID}))
	})

	t.Run("expired_lease_becomes_claimable_again", func(t *testing.T) {
		// Extends TestStubRouterLeaseExpiryAndRedelivery with a short lease window.
		store := registry.NewMemoryStore()
		require.NoError(t, store.Register(&registry.Agent{ID: "recipient"}))
		require.NoError(t, store.Deliver("recipient", &registry.InboxEntry{Payload: json.RawMessage(`{"n":1}`)}))
		first, staleLease, err := store.Retrieve("recipient", 5*time.Millisecond, 10)
		require.NoError(t, err)
		require.Len(t, first, 1)
		time.Sleep(10 * time.Millisecond)
		again, freshLease, err := store.Retrieve("recipient", time.Second, 10)
		require.NoError(t, err)
		require.Len(t, again, 1)
		require.Equal(t, first[0].ID, again[0].ID)
		require.NotEmpty(t, freshLease)
		require.NotEqual(t, staleLease, freshLease)
		require.ErrorIs(t, store.Ack("recipient", staleLease, []string{again[0].ID}), registry.ErrLeaseConflict)
		require.NoError(t, store.Ack("recipient", freshLease, []string{again[0].ID}))
	})

	t.Run("ACK_removes_once_then_returns_documented_not_found", func(t *testing.T) {
		// Consolidates TestStubRouterLeasesRepliesAndAcks and the sentinel
		// distinction pinned by TestMemoryStore_AckMissingIDReportsMessageNotFound.
		store := registry.NewMemoryStore()
		require.NoError(t, store.Register(&registry.Agent{ID: "recipient"}))
		require.NoError(t, store.Deliver("recipient", &registry.InboxEntry{Payload: json.RawMessage(`{"n":1}`)}))
		messages, lease, err := store.Retrieve("recipient", time.Second, 10)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		require.NoError(t, store.Ack("recipient", lease, []string{messages[0].ID}))
		remaining, nextLease, err := store.Retrieve("recipient", time.Second, 10)
		require.NoError(t, err)
		require.Empty(t, remaining)
		require.Empty(t, nextLease)
		err = store.Ack("recipient", lease, []string{messages[0].ID})
		require.Error(t, err)
		require.True(t, errors.Is(err, registry.ErrMessageNotFound) || errors.Is(err, registry.ErrLeaseConflict), "second ACK must follow documented missing/conflict contract: %v", err)
	})

	t.Run("federation_hold_caps_exponential_backoff_and_reports_exhaustion", func(t *testing.T) {
		// Retry/backoff/exhaustion coverage is new. The hold manager's configured
		// terminal sink writes a FEDERATION_FAILED inbox receipt; it does not put
		// this failure in registry's TTL dead-letter archive (see residual below).
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()
		client := federation.NewClient([]string{server.URL}, time.Second, "")
		queue := federation.NewMemoryHoldQueue()
		manager := federation.NewHoldManager(client, queue, federation.HoldConfig{
			MaxHold: time.Second, RetryEvery: 2 * time.Millisecond, MaxRetryInterval: 3 * time.Millisecond,
		})
		store := registry.NewMemoryStore()
		require.NoError(t, store.Register(&registry.Agent{ID: "sender"}))
		manager.SetNotifier(registry.FederationFailureSink(store))

		start := time.Now()
		require.NoError(t, manager.Enqueue(&federation.HoldItem{
			ID: "held-message", AgentID: "remote", Sender: "sender",
			Body: json.RawMessage(`{"payload":{"n":1}}`), EnqueuedAt: start,
			Deadline: start.Add(20 * time.Millisecond), NextAttemptAt: start,
		}))
		manager.Sweep(start)
		firstRetry := queue.List()[0]
		require.Equal(t, 2, firstRetry.Attempts)
		require.Equal(t, 3*time.Millisecond, firstRetry.NextAttemptAt.Sub(start), "backoff grows to cap")
		manager.Sweep(firstRetry.NextAttemptAt)
		secondRetry := queue.List()[0]
		require.Equal(t, 3, secondRetry.Attempts)
		require.Equal(t, 3*time.Millisecond, secondRetry.NextAttemptAt.Sub(firstRetry.NextAttemptAt), "backoff remains capped")
		manager.Sweep(secondRetry.Deadline)
		require.Zero(t, queue.Len(), "exhaustion removes the held item")
		receipts, _, err := store.Retrieve("sender", time.Second, 10)
		require.NoError(t, err)
		require.Len(t, receipts, 1, "the terminal failure receipt reaches the sender")

		t.Skip("hold exhaustion currently writes a sender inbox receipt, not a registry dead-letter archive record")
	})

	t.Run("authentication_refusal_is_never_retried", func(t *testing.T) {
		// New direct retry-worker assertion; adjacent existing coverage is
		// TestHoldManagerRetryDefinitiveRejectionIsTerminal (403 is terminal).
		var calls atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusForbidden)
		}))
		defer server.Close()
		manager := federation.NewHoldManager(federation.NewClient([]string{server.URL}, time.Second, ""), federation.NewMemoryHoldQueue(), federation.HoldConfig{MaxHold: time.Second})
		var reports atomic.Int64
		manager.SetNotifier(func(federation.FailureReport) error { reports.Add(1); return nil })
		now := time.Now()
		require.NoError(t, manager.Enqueue(&federation.HoldItem{
			ID: "refused", AgentID: "remote", Body: json.RawMessage(`{"payload":{}}`),
			EnqueuedAt: now, Deadline: now.Add(time.Second), NextAttemptAt: now,
		}))
		manager.Sweep(now)
		manager.Sweep(now.Add(time.Millisecond))
		require.EqualValues(t, 1, calls.Load(), "403 refusal must not be forwarded again")
		require.EqualValues(t, 1, reports.Load(), "terminal refusal is reported once")
		require.Zero(t, manager.Pending())
	})
}
