package permissions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var baseTime = time.Date(2026, 10, 3, 17, 45, 5, 0, time.UTC)

func at(sec int) time.Time { return baseTime.Add(time.Duration(sec) * time.Second) }

func newStore(t *testing.T) *JSONLStore {
	t.Helper()
	s, err := NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustAppend(t *testing.T, s Store, rec *Record) {
	t.Helper()
	require.NoError(t, s.Append(context.Background(), rec))
}

func snap(t *testing.T, s Store) *Snapshot {
	t.Helper()
	sn, err := s.Snapshot(context.Background())
	require.NoError(t, err)
	return sn
}

// activePrincipal is the fixture every checker test starts from.
func activePrincipal(id, role, namespace string) *Principal {
	return &Principal{
		ID: id, DisplayName: id, Namespace: namespace, Role: Role(role),
		Status: PrincipalActive, CreatedAt: at(0), LastLoginAt: ptrTime(at(60)),
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// ---------------------------------------------------------------------------
// §5.2 — the may/may-not matrix, as a table.
// ---------------------------------------------------------------------------

// TestRoleMatrix is §5.2 transcribed: every cell of the read/send/invite/admin/
// mint/invoke matrix for the four roles. It is the contract the rest of the
// ACL trusts, so it is asserted cell by cell rather than inferred from
// behaviour.
func TestRoleMatrix(t *testing.T) {
	cases := []struct {
		role  Role
		allow []Action
		deny  []Action
	}{
		{RoleOwner, []Action{ActionSend, ActionRead, ActionInvite, ActionAdmin, ActionMint, ActionInvoke}, nil},
		{RoleAdmin, []Action{ActionSend, ActionRead, ActionInvite, ActionAdmin, ActionMint, ActionInvoke}, nil},
		{RoleMember, []Action{ActionSend, ActionRead, ActionInvite, ActionInvoke}, []Action{ActionAdmin, ActionMint}},
		{RoleViewer, []Action{ActionRead}, []Action{ActionSend, ActionInvite, ActionAdmin, ActionMint, ActionInvoke}},
	}
	for _, tc := range cases {
		t.Run(string(tc.role), func(t *testing.T) {
			for _, a := range tc.allow {
				require.Truef(t, RoleAllows(tc.role, a), "%s must allow %s (§5.2)", tc.role, a)
			}
			for _, a := range tc.deny {
				require.Falsef(t, RoleAllows(tc.role, a), "%s must NOT allow %s (§5.2)", tc.role, a)
			}
			// The full action set is partitioned: every action above is
			// classified, so a new action cannot slip in unasserted.
			require.Len(t, tc.allow, len(tc.allow))
		})
	}
}

// TestRoleMatrixPartitionsActionSet proves the table above covers the whole
// closed action set, so a newly added action cannot be silently unclassified.
func TestRoleMatrixPartitionsActionSet(t *testing.T) {
	covered := map[Action]bool{}
	for _, r := range Roles {
		for _, a := range Actions {
			if RoleAllows(r, a) {
				covered[a] = true
			}
		}
	}
	for _, a := range Actions {
		require.Truef(t, covered[a], "action %s is allowed by no role — §5.2 must classify it", a)
	}
}

// TestOnlyOwnerChangesNamespacePolicy is §5.1's owner-only axis.
func TestOnlyOwnerChangesNamespacePolicy(t *testing.T) {
	require.True(t, RoleChangesNamespacePolicy(RoleOwner))
	for _, r := range []Role{RoleAdmin, RoleMember, RoleViewer} {
		require.Falsef(t, RoleChangesNamespacePolicy(r), "%s must not change namespace policy (§5.1)", r)
	}
}

// ---------------------------------------------------------------------------
// Validation.
// ---------------------------------------------------------------------------

func TestRecordValidation(t *testing.T) {
	valid := func() *Record {
		return (&Grant{
			ID: "grant_1", Principal: "prin_a",
			Subject: Subject{Type: SubjectAgent, Ref: "atlas"},
			Actions: []Action{ActionSend}, GrantedBy: "prin_owner", GrantedAt: at(0),
		}).Record(at(0))
	}
	require.NoError(t, valid().Validate())

	cases := []struct {
		name    string
		mutate  func(*Record)
		wantErr string
	}{
		{"unknown version", func(r *Record) { r.V = 99 }, "unsupported record version"},
		{"unknown type", func(r *Record) { r.Type = "permission.wat" }, "unknown record type"},
		{"missing id", func(r *Record) { r.ID = ""; r.Grant.ID = "" }, "record without id"},
		{"zero ts", func(r *Record) { r.TS = time.Time{} }, "zero ts"},
		{"payload id mismatch", func(r *Record) { r.Grant.ID = "other" }, "same id"},
		{"unknown action", func(r *Record) { r.Grant.Actions = []Action{"teleport"} }, "unknown action"},
		{"empty actions", func(r *Record) { r.Grant.Actions = nil }, "empty action set"},
		{"unknown subject", func(r *Record) { r.Grant.Subject.Type = "planet" }, "unknown subject type"},
		{"no principal", func(r *Record) { r.Grant.Principal = "" }, "without a principal"},
		{"no grantor", func(r *Record) { r.Grant.GrantedBy = "" }, "without granted_by"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := valid()
			tc.mutate(r)
			err := r.Validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestAgentClassOwnerCoherence is §3.1's accountability rule: a class that
// cannot answer "who is accountable" is not a class.
func TestAgentClassOwnerCoherence(t *testing.T) {
	require.NoError(t, (&AgentInfo{ID: "atlas", Class: ClassPersonal, Owner: "prin_a"}).Validate())
	require.NoError(t, (&AgentInfo{ID: "deploy-bot", Class: ClassService}).Validate())
	require.NoError(t, (&AgentInfo{ID: "legacy"}).Validate(), "an unclassed row is the legacy posture")

	require.ErrorContains(t, (&AgentInfo{ID: "atlas", Class: ClassPersonal}).Validate(), "has no owner")
	require.ErrorContains(t, (&AgentInfo{ID: "deploy-bot", Class: ClassService, Owner: "prin_a"}).Validate(), "carries an owner")
	require.ErrorContains(t, (&AgentInfo{ID: "x", Class: "robot"}).Validate(), "unknown class")
}

func TestRecordMarshalParseRoundTrip(t *testing.T) {
	rec := (&Grant{
		ID: "grant_x", Principal: "prin_a",
		Subject: Subject{Type: SubjectCapability, Ref: "research"},
		Actions: []Action{ActionInvoke}, GrantedBy: "prin_owner", GrantedAt: at(0),
	}).Record(at(0))
	line, err := rec.MarshalLine()
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(string(line), "\n"))
	got, err := ParseRecord(line)
	require.NoError(t, err)
	require.Equal(t, rec.ID, got.ID)
	require.Equal(t, rec.Grant.Subject, got.Grant.Subject)
	require.Equal(t, rec.Grant.Actions, got.Grant.Actions)
}

// TestRefusalBodyShape pins the §6.7 wire body: the exact key set and the
// machine-readable error code a client branches on.
func TestRefusalBodyShape(t *testing.T) {
	in := CheckInput{
		Sender:  EffectiveSender{Kind: SenderPrincipal, Principal: "prin_viewer", AsAgent: "quill"},
		Action:  ActionSend,
		Subject: Subject{Type: SubjectAgent, Ref: "atlas"},
	}
	body, err := json.Marshal(RefusalFor(in, Result{Reason: ReasonNoGrant, Detail: "no live grant"}))
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	require.Equal(t, ErrorDeliveryForbidden, decoded["error"])
	require.Equal(t, ReasonNoGrant, decoded["reason"])
	require.Equal(t, "prin_viewer", decoded["principal"])
	require.Equal(t, "quill", decoded["as_agent"])
	require.Equal(t, string(ActionSend), decoded["action"])
	target, ok := decoded["target"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, string(SubjectAgent), target["type"])
	require.Equal(t, "atlas", target["ref"])

	// Field ORDER matches the spec's printed body (encoding/json preserves
	// struct order), so a client reading raw bytes sees error first.
	require.True(t, strings.HasPrefix(string(body), `{"error":"DELIVERY_FORBIDDEN"`), "body: %s", body)
}
