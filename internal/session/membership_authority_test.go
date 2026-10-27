package session

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crier-dev/crier/internal/permissions"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

// membershipWriteIDPredicates are the three shapes DF-CRIER-470 names, so the
// tests below can say which identity was attempted rather than only which
// status came back.
const (
	membershipAgentHeader  = registry.HeaderAgentID
	membershipSignedHeader = registry.HeaderAgentSig
)

// signedMembershipHandler wires the SHIPPED ingress verifier shape: a proof is
// only verified for the agent that owns the key, so a caller that NAMES one
// agent while presenting another's signature is refused. This is the real
// mismatch the registry detects (sender_identity.go), stubbed here because the
// registry's own signing fixture is exercised in internal/registry.
func signedMembershipHandler(t *testing.T, repo Repository, reg *registry.MemoryStore) *Handler {
	t.Helper()
	return NewHTTPHandler(repo, HTTPOptions{
		Deliverer:             registry.NewSessionDeliverer(registry.NewHandler(reg)),
		Agents:                reg,
		RequireAgentSignature: true,
		AuthorizeSender: func(w http.ResponseWriter, r *http.Request, required bool) (bool, bool) {
			require.True(t, required, "the deployment requires a proof on every signed route")
			claimed := r.Header.Get(registry.HeaderAgentID)
			if r.Header.Get(registry.HeaderAgentSig) != "key-of-"+claimed {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"SESSION_SENDER_SIGNATURE_INVALID"}`))
				return false, false
			}
			return true, true
		},
	})
}

// membershipCall drives ONE membership write against a handler directly, so the
// request can carry a raw body and arbitrary headers (the harness only sends
// well-formed payloads).
func membershipCall(t *testing.T, h *Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	r = mux.SetURLVars(r, map[string]string{"id": membershipRoomFromPath(path)})
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	if method == http.MethodPut {
		h.HandleSetMemberContext(w, r)
	} else {
		h.HandleAddParticipant(w, r)
	}
	return w
}

// membershipRoomFromPath pulls the session id out of a session path: the route is
// /sessions/{id}/participants[<more>].
func membershipRoomFromPath(path string) string {
	const prefix = "/sessions/"
	if len(path) <= len(prefix) {
		return ""
	}
	rest := path[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			return rest[:i]
		}
	}
	return ""
}

// TestSessionMembershipIngressProof is DF-CRIER-470's ingress arm: the add path
// must reach the same decision a message and a state transition reach, and an
// actor it cannot establish must be REFUSED rather than stored as `actor: {}`.
// Without this, `member added by agent X` was a claim about X that anyone could
// make.
func TestSessionMembershipIngressProof(t *testing.T) {
	t.Parallel()
	for _, b := range localBackends() {
		t.Run(b.name, func(t *testing.T) {
			repo := b.open(t, b.pathFn(t))
			t.Cleanup(func() {
				if closer, ok := repo.(interface{ Close() error }); ok {
					_ = closer.Close()
				}
			})
			harness := newAPIHarness(t, repo)
			room := harness.createRoom(t)
			handler := signedMembershipHandler(t, repo, harness.reg)

			addBody := func(actor string) string {
				if actor == "" {
					return `{"member_type":"agent","member_id":"nimbus-2"}`
				}
				return `{"member_type":"agent","member_id":"nimbus-2","actor":"` + actor + `"}`
			}

			for _, tc := range []struct {
				name      string
				body      string
				agentID   string
				signature string
				status    int
				code      string
				basis     string
			}{
				{
					// (a) A forged X-Agent-ID: the caller names atlas but presents a key
					// that is not atlas's. The verifier refuses before anything is read.
					name: "forged agent id", body: addBody("atlas"),
					agentID: "atlas", signature: "key-of-nimbus",
					status: http.StatusUnauthorized, code: "SESSION_SENDER_SIGNATURE_INVALID",
				},
				{
					// (b) The body names someone else while the caller is another agent:
					// a signature for nimbus cannot assert atlas as the actor.
					name: "body actor is not the caller", body: addBody("atlas"),
					agentID: "nimbus", signature: "key-of-nimbus",
					status: http.StatusForbidden, code: "SESSION_SENDER_IDENTITY_MISMATCH",
				},
				{
					// (c) An operator token with NO identity: nothing to record.
					name: "no identity at all", body: addBody(""),
					status: http.StatusBadRequest, code: "AUTHOR_REQUIRED",
				},
				{
					// A caller cannot mint its own assurance: the basis is server-stamped,
					// and the strict decoder refuses the field outright.
					name:    "forged basis in the body",
					body:    `{"member_type":"agent","member_id":"nimbus-2","actor":"atlas","sender_identity_basis":"sender-identity-verified"}`,
					agentID: "atlas", signature: "key-of-atlas",
					status: http.StatusBadRequest, code: "INVALID_REQUEST",
				},
				{
					name: "no proof offered", body: addBody("atlas"),
					agentID: "atlas",
					status:  http.StatusUnauthorized, code: "SESSION_SENDER_SIGNATURE_REQUIRED",
				},
				{
					name: "verified", body: addBody("atlas"),
					agentID: "atlas", signature: "key-of-atlas",
					status: http.StatusCreated, basis: SenderIdentityVerified,
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					st, err := repo.Load(context.Background(), room)
					require.NoError(t, err)
					membersBefore := len(st.Members)

					headers := map[string]string{}
					if tc.agentID != "" {
						headers[membershipAgentHeader] = tc.agentID
					}
					if tc.signature != "" {
						headers[membershipSignedHeader] = tc.signature
					}
					w := membershipCall(t, handler, http.MethodPost, "/sessions/"+room+"/participants", tc.body, headers)
					require.Equal(t, tc.status, w.Code, w.Body.String())

					st, err = repo.Load(context.Background(), room)
					require.NoError(t, err)
					if tc.status != http.StatusCreated {
						if tc.code != "" {
							require.Contains(t, w.Body.String(), tc.code)
						}
						require.Len(t, st.Members, membersBefore, "a refused membership write records nothing")
						return
					}

					var recorded addParticipantResponse
					require.NoError(t, json.Unmarshal(w.Body.Bytes(), &recorded))
					require.Equal(t, tc.basis, recorded.SenderIdentityBasis, "the 201 states the assurance it recorded")
					require.Len(t, st.Members, membersBefore+1)

					// The RECORD — not the response — is what a later reader trusts.
					recs, err := repo.Records(context.Background(), room)
					require.NoError(t, err)
					var add *Record
					for _, rec := range recs {
						if rec.Type == RecordMemberAdd && rec.MemberID == "nimbus-2" {
							add = rec
						}
					}
					require.NotNil(t, add, "the join is on the record")
					require.Equal(t, tc.basis, add.SenderIdentityBasis)
					require.NotNil(t, add.Actor)
					require.Equal(t, "atlas", add.Actor.Agent)
				})
			}
		})
	}
}

// TestSessionMembershipAdminRequired is DF-CRIER-470's gate arm: a membership
// write owes the session's `admin` action — the one a close/reopen enforces —
// not merely the send authority a message needs. A refusal writes nothing, and
// the admitted write records the basis it earned.
func TestSessionMembershipAdminRequired(t *testing.T) {
	t.Parallel()
	for _, b := range localBackends() {
		t.Run(b.name, func(t *testing.T) {
			repo := b.open(t, b.pathFn(t))
			t.Cleanup(func() { _ = repo.Close() })
			h := newTaskHarness(t, repo)
			room := h.createRoomAs(t)

			subject := permissions.Subject{Type: permissions.SubjectSession, Ref: room}
			before, err := repo.Load(context.Background(), room)
			require.NoError(t, err)
			contextBefore := len(before.ContextEvents)
			var refusal map[string]string
			require.Equal(t, http.StatusForbidden, h.do(t, http.MethodPost, "/sessions/"+room+"/participants",
				map[string]any{"member_type": "principal", "member_id": "prin_stranger", "actor": fixAgentA},
				&refusal, nil), "a member without admin cannot change who is in the room")
			require.Equal(t, "SESSION_MEMBERSHIP_FORBIDDEN", refusal["error"])

			var contextRefusal map[string]string
			require.Equal(t, http.StatusForbidden, h.do(t, http.MethodPut,
				"/sessions/"+room+"/participants/agent/"+fixAgentB+"/context",
				map[string]any{"mode": string(ShareFull), "actor": fixAgentA}, &contextRefusal, nil),
				"the same gate covers a context-share change")
			require.Equal(t, "SESSION_MEMBERSHIP_FORBIDDEN", contextRefusal["error"])

			st, err := repo.Load(context.Background(), room)
			require.NoError(t, err)
			require.Len(t, st.Members, 2, "a refused write appends no membership")
			require.Len(t, st.ContextEvents, contextBefore, "a refused write appends no context event")

			// The room's owner holds admin on it: the same two writes are admitted
			// and record the (claimed) assurance of this deployment's ingress.
			h.seedGrant(t, &permissions.Grant{
				ID: "g_admin_" + room, Kind: "grant", Principal: "prin_owner",
				Subject: subject, Actions: []permissions.Action{permissions.ActionAdmin},
				GrantedBy: "prin_owner",
			})
			var added addParticipantResponse
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/participants",
				map[string]any{"member_type": "principal", "member_id": "prin_stranger", "actor": fixAgentA},
				&added, nil))
			require.Equal(t, SenderIdentityClaimed, added.SenderIdentityBasis)

			var changed MemberContext
			require.Equal(t, http.StatusOK, h.do(t, http.MethodPut,
				"/sessions/"+room+"/participants/agent/"+fixAgentB+"/context",
				map[string]any{"mode": string(ShareFull), "actor": fixAgentA}, &changed, nil))
			require.Equal(t, SenderIdentityClaimed, changed.SenderIdentityBasis)
			require.Equal(t, fixAgentA, changed.SetBy)
		})
	}
}

// TestSessionMembershipBasisIsDurable pins that the assurance survives into the
// READ surfaces both backends serve: the projected share (the SQL view table has
// no column for it, so it is completed from the retained record) and the
// transcript's system events. Without it a reader could not tell a proven
// identity from a claim.
func TestSessionMembershipBasisIsDurable(t *testing.T) {
	t.Parallel()
	for _, b := range localBackends() {
		t.Run(b.name, func(t *testing.T) {
			repo := b.open(t, b.pathFn(t))
			t.Cleanup(func() { _ = repo.Close() })
			h := newAPIHarness(t, repo)
			room := h.createRoom(t)
			root := postText(t, h, room, fixAgentA, "root")

			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/participants",
				map[string]any{"member_type": "agent", "member_id": "nimbus-2", "actor": fixAgentA},
				&struct{}{}, nil))
			require.Equal(t, http.StatusOK, h.do(t, http.MethodPut,
				"/sessions/"+room+"/participants/agent/nimbus-2/context",
				map[string]any{"mode": string(ShareSince), "boundary_message_id": root, "actor": fixAgentA},
				&struct{}{}, nil))

			st, err := repo.Load(context.Background(), room)
			require.NoError(t, err)
			mc := contextShareFor(st, MemberAgent, "nimbus-2")
			require.NotNil(t, mc)
			require.Equal(t, SenderIdentityClaimed, mc.SenderIdentityBasis,
				"the projected share carries the assurance the change was recorded with")

			var add, change *Record
			for _, rec := range st.ContextEvents {
				switch {
				case rec.Type == RecordMemberAdd && rec.MemberID == "nimbus-2":
					add = rec
				case rec.Type == RecordMemberContext && rec.MemberID == "nimbus-2":
					change = rec
				}
			}
			require.NotNil(t, add, "the join is retained")
			require.Equal(t, SenderIdentityClaimed, add.SenderIdentityBasis)
			require.NotNil(t, add.Actor)
			require.Equal(t, fixAgentA, add.Actor.Agent)
			require.NotNil(t, change, "the share change is retained")
			require.Equal(t, SenderIdentityClaimed, change.SenderIdentityBasis)
			require.NotNil(t, change.Actor)
			require.Equal(t, fixAgentA, change.Actor.Agent)

			// The transcript states it too: a system event never presents a
			// claimed identity as a proven one.
			var tr transcriptResponse
			require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+room+"/messages", nil, &tr, nil))
			var bases []string
			for _, ev := range tr.SystemEvents {
				if ev.MemberID == "nimbus-2" {
					bases = append(bases, ev.SenderIdentityBasis)
				}
			}
			require.Equal(t, []string{SenderIdentityClaimed, SenderIdentityClaimed}, bases,
				"join and share change both report the assurance they were recorded with")
		})
	}
}

// TestSessionMembershipRefusesMissingActorDurably is the "operator token with no
// identity" arm at the durability level: the refusal must leave no membership and
// no `actor: {}` LINE behind — the defect was a stored record with an empty
// actor, not merely a lenient status code.
func TestSessionMembershipRefusesMissingActorDurably(t *testing.T) {
	t.Parallel()
	for _, b := range localBackends() {
		t.Run(b.name, func(t *testing.T) {
			repo := b.open(t, b.pathFn(t))
			t.Cleanup(func() { _ = repo.Close() })
			h := newAPIHarness(t, repo)
			room := h.createRoom(t)

			var refusal map[string]string
			require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost, "/sessions/"+room+"/participants",
				map[string]any{"member_type": "agent", "member_id": "nimbus-2"}, &refusal, nil))
			require.Equal(t, "AUTHOR_REQUIRED", refusal["error"])

			recs, err := repo.Records(context.Background(), room)
			require.NoError(t, err)
			for _, rec := range recs {
				if rec.Type == RecordMemberAdd && rec.MemberID == "nimbus-2" {
					t.Fatalf("the refused join must not be recorded: %+v", rec)
				}
			}

			// The X-Agent-ID header remains the fallback a claimed caller uses —
			// and the basis says exactly that: claimed, not verified.
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/participants",
				map[string]any{"member_type": "agent", "member_id": "nimbus-2"}, &struct{}{},
				map[string]string{registry.HeaderAgentID: fixAgentA}))
			st, err := repo.Load(context.Background(), room)
			require.NoError(t, err)
			require.NotNil(t, st.findMember(MemberAgent, "nimbus-2"))
			require.Equal(t, SenderIdentityClaimed, contextShareFor(st, MemberAgent, "nimbus-2").SenderIdentityBasis)
		})
	}
}
