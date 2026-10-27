package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/registry"
)

// GroupStore is a composition boundary: an imported/custom roster may contain
// remote-qualified members even though the built-in group's write API currently
// accepts local ident-shaped IDs only. Exercise that roster at the send boundary,
// not by broadening group creation's separate validation contract.
type remoteGroupRoster struct {
	GroupStore
	members []string
	reads   atomic.Int32
}

func (g *remoteGroupRoster) Get(_ context.Context, name string) (*Group, error) {
	g.reads.Add(1)
	if name != "team" {
		return nil, ErrGroupNotFound
	}
	return &Group{Name: name, Members: g.members}, nil
}

type remoteGroupLocalDispatch struct {
	*registry.SessionDeliverer
	localCalls  atomic.Int32
	remoteCalls atomic.Int32
}

func (d *remoteGroupLocalDispatch) DispatchDelivery(ctx context.Context, id string, req registry.DeliveryRequest) (registry.DeliveryResult, error) {
	if strings.HasPrefix(id, RemotePrefix) {
		d.remoteCalls.Add(1)
	} else {
		d.localCalls.Add(1)
	}
	return d.SessionDeliverer.DispatchDelivery(ctx, id, req)
}

// These tests intentionally use only pre-fix symbols so the RED control runs
// against origin/main rather than failing to compile on a new helper/signature.
func TestRemoteGroupRouting(t *testing.T) {
	t.Parallel()
	const remote = "remote:peer_b:atlas-b"
	for _, tc := range []struct {
		name        string
		members     []string
		targets     []AudienceTarget
		localCalls  int32
		remoteCalls int32
		rosterReads int32
		withoutFed  bool
		withoutJoin bool
		wantRefusal string
	}{
		{name: "group-only-remote", members: []string{remote}, targets: []AudienceTarget{{Kind: TargetGroup, ID: "team"}}, remoteCalls: 1, rosterReads: 1},
		{name: "direct-remote-unchanged", targets: []AudienceTarget{{Kind: TargetAgent, ID: remote}}, remoteCalls: 1},
		{name: "mixed-local-and-remote", members: []string{"local", remote}, targets: []AudienceTarget{{Kind: TargetGroup, ID: "team"}}, localCalls: 1, remoteCalls: 1, rosterReads: 1},
		{name: "local-group-does-not-address-remote", members: []string{"local"}, targets: []AudienceTarget{{Kind: TargetGroup, ID: "team"}}, localCalls: 1, rosterReads: 1},
		{name: "direct-plus-group-deduplicated", members: []string{remote, remote}, targets: []AudienceTarget{{Kind: TargetGroup, ID: "team"}, {Kind: TargetAgent, ID: remote}}, remoteCalls: 1, rosterReads: 1},
		{name: "unknown-roster-skipped", targets: []AudienceTarget{{Kind: TargetGroup, ID: "unknown"}}, rosterReads: 1},
		{name: "group-without-federation-refused", members: []string{remote}, targets: []AudienceTarget{{Kind: TargetGroup, ID: "team"}}, rosterReads: 1, withoutFed: true, wantRefusal: "FEDERATION_UNCONFIGURED"},
		{name: "group-without-active-membership-refused", members: []string{remote}, targets: []AudienceTarget{{Kind: TargetGroup, ID: "team"}}, rosterReads: 1, withoutJoin: true, wantRefusal: "not an active session participant"},
		{name: "malformed-remote-never-local", members: []string{"remote:peer_b:"}, targets: []AudienceTarget{{Kind: TargetGroup, ID: "team"}}, rosterReads: 1, wantRefusal: "invalid remote participant identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := newInstance(t), newInstance(t)
			require.NoError(t, b.reg.Register(&registry.Agent{ID: "atlas-b"}))
			require.NoError(t, a.reg.Register(&registry.Agent{ID: "local"}))
			var forwards atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forwards.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/agents/atlas-b/inbox" {
					t.Errorf("unexpected federation request: %s %s", r.Method, r.URL.Path)
				}
				b.srv.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(peer.Close)
			a.armPeerPolicy(federation.PeerPolicies{
				"peer_b": {Peer: "peer_b", URL: peer.URL, NamespacesAllow: []string{""}},
			})
			if tc.withoutFed {
				a.handler.SetFedClient(nil)
			}
			roster := &remoteGroupRoster{members: tc.members}
			a.handler.opts.Groups = roster
			dispatch := &remoteGroupLocalDispatch{SessionDeliverer: registry.NewSessionDeliverer(registry.NewHandler(a.reg))}
			a.handler.opts.Deliverer = dispatch
			room := createFederatedSession(t, a)
			if !tc.withoutJoin {
				status, body := a.do("POST", "/sessions/"+room+"/participants", `{"member_type":"agent","member_id":"remote:peer_b:atlas-b","actor":"atlas-a"}`, nil)
				require.Equal(t, http.StatusCreated, status, body)
			}
			request, err := json.Marshal(postMessageRequest{
				Sender: "atlas-a", Payload: json.RawMessage(`{"text":"remote roster probe"}`),
				Targets: tc.targets, IdempotencyKey: "remote-group-key",
			})
			require.NoError(t, err)
			status, body := a.do("POST", "/sessions/"+room+"/messages", string(request), nil)
			require.Equal(t, http.StatusCreated, status, body)
			var sent transcriptMessage
			require.NoError(t, json.Unmarshal([]byte(body), &sent))
			t.Logf("SEND outcomes=%+v; federation_calls=%d local_calls=%d remote_local_calls=%d", sent.Outcomes, forwards.Load(), dispatch.localCalls.Load(), dispatch.remoteCalls.Load())
			require.Equal(t, tc.remoteCalls, forwards.Load(), "group roster must reach the remote leg; response: %s", body)
			require.Zero(t, dispatch.remoteCalls.Load(), "remote-qualified IDs must never reach local DispatchDelivery; response: %s", body)
			require.Equal(t, tc.localCalls, dispatch.localCalls.Load())
			require.Equal(t, tc.rosterReads, roster.reads.Load(), "one roster snapshot feeds both legs")
			wantTargets := []string{}
			if tc.localCalls > 0 {
				wantTargets = append(wantTargets, "local")
			}
			if tc.remoteCalls > 0 || tc.wantRefusal != "" {
				target := remote
				if tc.name == "malformed-remote-never-local" {
					target = tc.members[0]
				}
				wantTargets = append(wantTargets, target)
			}
			require.Len(t, sent.Outcomes, len(wantTargets), "exactly one outcome per resolved target: %s", body)
			gotTargets := []string{}
			for _, outcome := range sent.Outcomes {
				gotTargets = append(gotTargets, outcome.Target)
				if tc.wantRefusal != "" {
					require.Equal(t, "refused", outcome.Outcome)
					require.Contains(t, outcome.Detail, tc.wantRefusal)
					require.NotEqual(t, "AGENT_NOT_FOUND", outcome.Code)
				} else {
					require.Equal(t, "accepted", outcome.Outcome)
				}
			}
			require.ElementsMatch(t, wantTargets, gotTargets)
			require.Len(t, sent.DispatchEvidence, 1)
			require.ElementsMatch(t, wantTargets, sent.DispatchEvidence[0].Targets)
			require.Len(t, sent.DispatchEvidence[0].Receipts, len(wantTargets))
			require.NotNil(t, sent.DispatchEvidence[0].CompletedAt)
			require.True(t, sent.DispatchEvidence[0].OutcomesRecorded)
			require.False(t, sent.OutcomesIncomplete)

			entries, _, err := b.reg.Retrieve("atlas-b", 0, 10)
			require.NoError(t, err)
			require.Len(t, entries, int(tc.remoteCalls), "the shipped federation path must actually store the remote delivery")
			if len(entries) > 0 {
				require.Contains(t, string(entries[0].Payload), "remote roster probe")
			}
			local, _, err := a.reg.Retrieve("local", 0, 10)
			require.NoError(t, err)
			require.Len(t, local, int(tc.localCalls))
			status, body = a.do("GET", "/sessions/"+room+"/messages", "", nil)
			require.Equal(t, http.StatusOK, status)
			var transcript struct {
				Messages []transcriptMessage `json:"messages"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &transcript))
			require.Len(t, transcript.Messages, 1)
			require.Equal(t, sent.Outcomes, transcript.Messages[0].Outcomes, "both legs survive the final durable write-back")
			if tc.wantRefusal == "" {
				status, body = a.do("POST", "/sessions/"+room+"/messages", string(request), nil)
				require.Equal(t, http.StatusCreated, status, body)
				require.Contains(t, body, `"replay_action":"replayed"`)
				require.Equal(t, tc.remoteCalls, forwards.Load(), "key replay cannot forward twice")
				require.Equal(t, tc.localCalls, dispatch.localCalls.Load())
				require.Zero(t, dispatch.remoteCalls.Load())
			}
		})
	}
}

func TestRemoteGroupUnknownRosterUsesAudienceSkip(t *testing.T) {
	t.Parallel()
	a := newInstance(t)
	roster := &remoteGroupRoster{}
	a.handler.opts.Groups = roster
	aud := Audience{Targets: []AudienceTarget{{Kind: TargetGroup, ID: "unknown"}}}
	recipients, skips := FanoutRecipients(aud, a.handler.resolveGroupRoster(context.Background(), aud))
	require.Empty(t, recipients)
	require.Equal(t, []AudienceSkip{{Target: aud.Targets[0], Reason: "group has no known roster (CR-CHAT-022)"}}, skips)
}
