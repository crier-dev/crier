package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/permissions"
)

func TestDispatchPeekSnapshotsDoNotMutateHints(t *testing.T) {
	for _, surface := range []string{"peek", "list"} {
		t.Run(surface, func(t *testing.T) {
			h := newCR36Harness(t, nil)
			h.register(t, cr36Router)
			accept := h.deliver(t, cr36Router, dispatchBody(cr36ExplicitModel, cr36ExplicitProvider, "original"), http.StatusCreated)
			var sent struct {
				ID string `json:"id"`
			}
			require.NoError(t, json.Unmarshal([]byte(accept), &sent))
			var entry *InboxEntry
			if surface == "peek" {
				var err error
				entry, err = h.store.(InboxPeeker).Peek(cr36Router, sent.ID)
				require.NoError(t, err)
			} else {
				entries, err := h.store.(InboxLister).PeekInbox(cr36Router)
				require.NoError(t, err)
				require.Len(t, entries, 1)
				entry = entries[0]
			}
			entry.Model.Value = "observer-changed-model"
			entry.Provider.Value = "observer-changed-provider"
			entry.Model.Form = HintBlank
			got := h.poll(t, cr36Router, 30)
			require.Len(t, got.Messages, 1)
			require.JSONEq(t, cr36ExplicitModel, string(got.Messages[0]["model"]))
			require.JSONEq(t, cr36ExplicitProvider, string(got.Messages[0]["provider"]))
		})
	}
}

func TestDispatchServicePermissionMatrix(t *testing.T) {
	identities := []struct{ agent, principal string }{
		{cr36Scheduler, "prin_sched"}, {cr36Router, "prin_router"}, {cr36Outsider, "prin_outsider"},
	}
	for _, sender := range identities {
		for _, recipient := range identities {
			t.Run(sender.agent+"_to_"+recipient.agent, func(t *testing.T) {
				a := newCR36ACL(t)
				status, body := a.sendAs(t, recipient.agent, sender.principal, sender.agent,
					dispatchBody(cr36ExplicitModel, cr36ExplicitProvider, "original"), nil)
				allowed := sender.agent == cr36Scheduler && recipient.agent == cr36Router || sender.agent == cr36Router && recipient.agent == cr36Scheduler
				if allowed {
					require.Equal(t, http.StatusCreated, status, body)
				} else {
					require.Equal(t, http.StatusForbidden, status, body)
					require.Contains(t, body, permissions.ErrorDeliveryForbidden)
				}
			})
		}
	}
	for _, body := range []string{
		`{"payload":{},"as_agent":"lane-scheduler"}`,
		`{"payload":{},"sender":"lane-scheduler","as_agent":"lane-scheduler","reply_to":"original"}`,
		`{"payload":{},"principal_id":"prin_outsider","as_agent":"lane-scheduler"}`,
		`{"payload":{},"principal_id":"prin_sched","as_agent":"unrelated-agent"}`,
	} {
		a := newCR36ACL(t)
		a.deliver(t, cr36Router, body, http.StatusForbidden)
		depth, _, _, err := a.store.Stats(cr36Router)
		require.NoError(t, err)
		require.Zero(t, depth)
	}
	// The same body stops being authoritative when its stored grant is revoked.
	a := newCR36ACL(t)
	require.NoError(t, permissions.RevokeGrant(context.Background(), a.perms, "grant_dispatch", "prin_owner", time.Now()))
	status, body := a.sendAs(t, cr36Router, "prin_sched", cr36Scheduler, `{"payload":{}}`, nil)
	require.Equal(t, http.StatusForbidden, status, body)
}

// Real HTTP delivery -> signed RemoteStore poll -> correlated result -> signed
// ack. Only completed work is acked; its unfinished sibling must redeliver.
func TestDispatchAuthorizedRouterCompletesOnlyOneMessage(t *testing.T) {
	a := newCR36ACL(t)
	routerPub, routerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	schedulerPub, schedulerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	mem := a.store.(*MemoryStore)
	mem.agents[cr36Router].PublicKey = HexKey(routerPub)
	mem.agents[cr36Scheduler].PublicKey = HexKey(schedulerPub)
	a.handler.SetRequireAgentSig(true)
	for i := 0; i < 2; i++ {
		status, body := a.sendAs(t, cr36Router, "prin_sched", cr36Scheduler, dispatchBody(cr36ExplicitModel, cr36ExplicitProvider, ""), nil)
		require.Equal(t, http.StatusCreated, status, body)
	}
	router := NewRemoteStore(a.server.URL, cr36Router, "", WithSigningKey(routerKey))
	scheduler := NewRemoteStore(a.server.URL, cr36Scheduler, "", WithSigningKey(schedulerKey))
	batch, lease, err := router.Retrieve(cr36Router, time.Second, 10)
	require.NoError(t, err)
	require.Len(t, batch, 2)
	reply, err := json.Marshal(map[string]any{"payload": map[string]string{"result": "completed"}, "reply_to": batch[0].ID})
	require.NoError(t, err)
	status, body := a.sendAs(t, cr36Scheduler, "prin_router", cr36Router, string(reply), nil)
	require.Equal(t, http.StatusCreated, status, body)
	answers, _, err := scheduler.Retrieve(cr36Scheduler, time.Minute, 10)
	require.NoError(t, err)
	require.Len(t, answers, 1)
	require.Equal(t, batch[0].ID, answers[0].ReplyTo)
	require.NoError(t, router.Ack(cr36Router, lease, []string{batch[0].ID}))
	time.Sleep(1100 * time.Millisecond)
	again, freshLease, err := router.Retrieve(cr36Router, time.Minute, 10)
	require.NoError(t, err)
	require.Len(t, again, 1)
	require.Equal(t, batch[1].ID, again[0].ID)
	require.Equal(t, "gpt-5.3", again[0].Model.String())
	require.Equal(t, "zai-glm-default", again[0].Provider.String())
	require.ErrorIs(t, router.Ack(cr36Router, lease, []string{again[0].ID}), ErrLeaseConflict)
	require.NoError(t, router.Ack(cr36Router, freshLease, []string{again[0].ID}))
}
