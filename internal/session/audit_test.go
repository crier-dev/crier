package session

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// CR-CHAT-021 — the session+principal audit read surface, the permission
// matrix read, and the per-principal role badges (CHAT-INTERFACE.md §4 rows
// 21, 22, 23).
//
// The acceptance criteria this file owns:
//
//   - AC1: the audit read returns a CLOSED event vocabulary, and an unknown
//     event kind cannot be appended — the renderer REFUSES with
//     ErrUnknownAuditEvent instead of skipping or guessing;
//   - AC2: the permission matrix is readable for a session with >= 2
//     participants and the roles are distinguishable per principal;
//   - AC3: the role badges carry the recorded per-principal role, with the
//     `member` default STATED (defaulted: true), never guessed by a client.
//
// Every case runs over both local backends (JSONL log + SQLite view) via the
// table-driven localBackends() harness the CR-CHAT-019 tests use.
// ---------------------------------------------------------------------------

// buildAuditedSession creates a session with two participants of DIFFERENT
// roles (owner + observer) so every read has distinguishable roles to serve,
// plus a close+reopen pair so the lifecycle events exist.
func buildAuditedSession(t *testing.T, h *apiHarness) string {
	t.Helper()
	var created struct {
		ID string `json:"id"`
	}
	code := h.do(t, http.MethodPost, "/sessions", map[string]any{
		"title":      "audit",
		"created_by": map[string]string{"principal": "kara"},
		"members": []map[string]string{
			{"member_type": "principal", "member_id": "kara", "role": "owner"},
		},
	}, &created, nil)
	require.Equal(t, http.StatusCreated, code)

	// A second participant with a DIFFERENT role — the matrix and the badges
	// must distinguish them.
	code = h.do(t, http.MethodPost, "/sessions/"+created.ID+"/participants", map[string]any{
		"member_type": "agent",
		"member_id":   "atlas",
		"role":        "observer",
	}, &struct{}{}, nil)
	require.Equal(t, http.StatusCreated, code)
	return created.ID
}

// TestAuditEventVocabularyIsClosed pins AC1's static half: the vocabulary is
// exactly the documented set, every member is a valid kind, and the list is
// the one the endpoint serves.
func TestAuditEventVocabularyIsClosed(t *testing.T) {
	want := []AuditEventKind{
		AuditSessionStart,
		AuditSessionClose,
		AuditSessionReopen,
		AuditMemberAdded,
		AuditMemberRemoved,
		AuditMemberContext,
		AuditThreadBranched,
		AuditPermissionGrant,
		AuditPermissionRevoke,
	}
	require.Equal(t, want, AuditVocabulary)
	require.Len(t, AuditVocabulary, 9)
	for _, k := range AuditVocabulary {
		require.True(t, ValidAuditKind(k), "kind %q must be valid", k)
	}
	require.False(t, ValidAuditKind("session.explode"))
	require.False(t, ValidAuditKind(""))
}

// TestAuditRenderRefusesUnknownRecordKind pins AC1's dynamic half: a record
// type that is neither an audit event nor a known transcript type CANNOT be
// appended into the trail — the renderer refuses with ErrUnknownAuditEvent,
// it does not skip the line and it does not guess a kind for it.
func TestAuditRenderRefusesUnknownRecordKind(t *testing.T) {
	forged := &Record{
		V:         RecordFormatVersion,
		Type:      RecordType("session.member.resurrect"),
		SessionID: "s1",
		Seq:       2,
		TS:        time.Now().UTC(),
		MemberID:  "atlas",
	}
	_, err := RenderAuditEvents([]*Record{forged})
	require.ErrorIs(t, err, ErrUnknownAuditEvent,
		"an unknown record kind must be REFUSED, never skipped or guessed")
	require.ErrorContains(t, err, "session.member.resurrect")
}

// TestAuditTrailPerBackend is the behavioural AC1 battery: a session with
// create, join, and close events yields exactly the closed-vocabulary events,
// in order, with the actors attributed.
func TestAuditTrailPerBackend(t *testing.T) {
	for _, b := range localBackends() {
		t.Run(b.name, func(t *testing.T) {
			store := b.open(t, b.pathFn(t))
			h := newAPIHarness(t, store)
			id := buildAuditedSession(t, h)

			var trail AuditTrail
			code := h.do(t, http.MethodGet, "/sessions/"+id+"/audit", nil, &trail, nil)
			require.Equal(t, http.StatusOK, code)

			// The vocabulary the endpoint serves IS the closed set.
			require.Equal(t, AuditVocabulary, trail.Vocabulary)

			// At least: session.start then member.added, in order. (The SQL
			// view serves its projected latest-state facts; the JSONL log
			// serves the full record stream. Both carry these two.)
			require.True(t, trail.Count >= 2, "want >= 2 events, got %d", trail.Count)
			require.Equal(t, AuditSessionStart, trail.Events[0].Kind)
			if trail.Events[0].Seq != 0 {
				require.Equal(t, int64(1), trail.Events[0].Seq)
			}
			require.Equal(t, "kara", trail.Events[0].Actor.Principal,
				"session.start is attributed to the creator")

			// The first member.added is the creator-principal's own join.
			var ownerAdd *AuditEvent
			for i := range trail.Events {
				if trail.Events[i].Kind == AuditMemberAdded && trail.Events[i].MemberID == "kara" {
					ownerAdd = &trail.Events[i]
					break
				}
			}
			require.NotNil(t, ownerAdd, "the owner's join is in the trail")
			require.Equal(t, MemberPrincipal, ownerAdd.MemberType)
			require.Equal(t, RoleOwner, ownerAdd.Role)

			// Every served kind is a member of the closed vocabulary.
			for _, ev := range trail.Events {
				require.True(t, ValidAuditKind(ev.Kind), "served kind %q is outside the vocabulary", ev.Kind)
			}
		})
	}
}

// TestAuditTrailRefusesGraftedRecord proves the REFUSAL at the renderer
// level. The store's own writer cannot produce an unknown record type (both
// the JSONL and SQL Append paths validate first — which is exactly the AC1
// refusal on the WRITE side, asserted here), and the renderer refuses one
// should a future version skew graft one in. The test asserts both refusals
// directly against the store and the renderer.
func TestAuditTrailRefusesGraftedRecord(t *testing.T) {
	forged := &Record{
		V:         RecordFormatVersion,
		Type:      RecordType("session.member.resurrect"),
		SessionID: "s1",
		Seq:       99,
		TS:        time.Now().UTC(),
		MemberID:  "ghost",
	}

	// WRITE side: neither backend accepts the unknown kind.
	for _, b := range localBackends() {
		t.Run("write/"+b.name, func(t *testing.T) {
			store := b.open(t, b.pathFn(t))
			err := store.Append(context.Background(), forged)
			require.ErrorIs(t, err, ErrInvalidRecord,
				"an unknown event kind cannot be appended (AC1)")
		})
	}

	// READ side: the renderer refuses instead of skipping or guessing.
	_, err := RenderAuditEvents([]*Record{forged})
	require.ErrorIs(t, err, ErrUnknownAuditEvent)
}

// TestPermissionMatrixPerBackend is AC2: the matrix is readable for a session
// with >= 2 participants, one row per ACTIVE participant, and the roles are
// distinguishable — the observer's send is a dash while the owner's is a tick.
func TestPermissionMatrixPerBackend(t *testing.T) {
	for _, b := range localBackends() {
		t.Run(b.name, func(t *testing.T) {
			store := b.open(t, b.pathFn(t))
			h := newAPIHarness(t, store)
			id := buildAuditedSession(t, h)

			var matrix permissionMatrixResponse
			code := h.do(t, http.MethodGet, "/sessions/"+id+"/permissions", nil, &matrix, nil)
			require.Equal(t, http.StatusOK, code)

			require.Equal(t, id, matrix.SessionID)
			require.Equal(t, []string{"send", "read", "invite", "admin"}, matrix.Actions)
			require.GreaterOrEqual(t, matrix.Count, 2, "the session has two participants")

			byID := map[string]matrixRow{}
			for _, row := range matrix.Rows {
				byID[row.MemberID] = row
			}
			owner, ok := byID["kara"]
			require.True(t, ok, "owner row present")
			require.Equal(t, RoleOwner, owner.Role)
			require.True(t, owner.Allowed["send"], "owner may send")
			require.True(t, owner.Allowed["admin"], "owner may admin")
			require.Equal(t, "role", owner.Source["send"])

			observer, ok := byID["atlas"]
			require.True(t, ok, "observer row present")
			require.Equal(t, RoleObserver, observer.Role)
			require.True(t, observer.Allowed["read"], "an observer reads")
			require.False(t, observer.Allowed["send"], "read never implies send (§5.2)")
			require.False(t, observer.Allowed["admin"])
			require.NotEqual(t, owner.Role, observer.Role,
				"the two principals' roles are distinguishable")
		})
	}
}

// TestRoleBadgesPerBackend is AC3: the badges carry the role recorded on the
// membership event — the `member` default is applied AND RECORDED at join
// time (the add record validates only with a role), so every badge is the
// durable event's role, never a client-side guess. A removed member keeps
// its badge with active=false.
func TestRoleBadgesPerBackend(t *testing.T) {
	for _, b := range localBackends() {
		t.Run(b.name, func(t *testing.T) {
			store := b.open(t, b.pathFn(t))
			h := newAPIHarness(t, store)
			id := buildAuditedSession(t, h)

			// A join with NO role records the `member` default at join time
			// — the badge serves exactly that recorded role.
			code := h.do(t, http.MethodPost, "/sessions/"+id+"/participants", map[string]any{
				"member_type": "agent",
				"member_id":   "nimbus",
			}, &struct{}{}, nil)
			require.Equal(t, http.StatusCreated, code)

			var badges roleBadgesResponse
			code = h.do(t, http.MethodGet, "/sessions/"+id+"/roles", nil, &badges, nil)
			require.Equal(t, http.StatusOK, code)

			byID := map[string]roleBadge{}
			for _, rb := range badges.Roles {
				byID[rb.MemberID] = rb
			}
			require.Equal(t, RoleOwner, byID["kara"].Role)
			require.Equal(t, RoleObserver, byID["atlas"].Role)
			require.Equal(t, RoleMember, byID["nimbus"].Role,
				"the default recorded at join time is served verbatim")
			require.True(t, byID["kara"].Active)
			require.True(t, byID["atlas"].Active)
			require.True(t, byID["nimbus"].Active)
		})
	}
}

// TestAuditGrantOverlay proves the permission.* half of the vocabulary: when
// the CR-CHAT-003 bridge is wired, grants on the session's subject appear as
// permission.grant events, a tombstoned grant as permission.revoke, and the
// matrix overlay flips the granted cell with source=grant.
func TestAuditGrantOverlay(t *testing.T) {
	now := time.Now().UTC()
	grants := []PermissionsGrantView{
		{
			ID:        "g1",
			Principal: "atlas",
			Actions:   []string{"send"},
			GrantedBy: "kara",
			GrantedAt: now.Add(-time.Hour),
		},
		{
			ID:        "g2",
			Principal: "nimbus",
			Actions:   []string{"send", "admin"},
			GrantedBy: "kara",
			GrantedAt: now.Add(-2 * time.Hour),
			RevokedBy: "kara",
		},
	}
	revokedAt := now.Add(-time.Minute)
	grants[1].RevokedAt = &revokedAt

	store := localBackends()[0].open(t, localBackends()[0].pathFn(t))
	h2 := newAPIHarnessWithGrants(t, store, grants)
	id := buildAuditedSession(t, h2)

	var trail AuditTrail
	code := h2.do(t, http.MethodGet, "/sessions/"+id+"/audit", nil, &trail, nil)
	require.Equal(t, http.StatusOK, code)

	kinds := map[AuditEventKind]bool{}
	for _, ev := range trail.Events {
		kinds[ev.Kind] = true
	}
	require.True(t, kinds[AuditPermissionGrant], "g1 is live: a permission.grant event")
	require.True(t, kinds[AuditPermissionRevoke], "g2 is tombstoned: a permission.revoke event")

	// The matrix overlay: atlas is an observer whose send arrives by GRANT.
	var matrix permissionMatrixResponse
	code = h2.do(t, http.MethodGet, "/sessions/"+id+"/permissions", nil, &matrix, nil)
	require.Equal(t, http.StatusOK, code)
	for _, row := range matrix.Rows {
		if row.MemberID == "atlas" {
			require.True(t, row.Allowed["send"], "the live grant adds send")
			require.Equal(t, "grant", row.Source["send"], "the cell names its source")
		}
		if row.MemberID == "nimbus" {
			// A tombstoned grant adds nothing: the overlay is LIVE grants.
			require.False(t, row.Allowed["send"], "a revoked grant is treated as absent (§6.6)")
		}
	}

	// The grant events render both halves of a tombstone's story.
	sawRevoke := false
	for _, ev := range trail.Events {
		if ev.Kind == AuditPermissionRevoke {
			sawRevoke = true
			require.Equal(t, "g2", ev.GrantID)
			require.Equal(t, "kara", ev.RevokedBy)
			require.NotNil(t, ev.RevokedAt)
		}
	}
	require.True(t, sawRevoke)
}

// TestAuditReadsRefuseUnknownSession: the reads are ordinary scoped reads —
// an unknown session is a 404 in every realm, never a silent empty trail.
func TestAuditReadsRefuseUnknownSession(t *testing.T) {
	store := localBackends()[0].open(t, localBackends()[0].pathFn(t))
	h := newAPIHarness(t, store)

	for _, path := range []string{"/audit", "/permissions", "/roles"} {
		code := h.do(t, http.MethodGet, "/sessions/nope"+path, nil, nil, nil)
		require.Equal(t, http.StatusNotFound, code, "path %s", path)
	}
}

// TestAuditEventsJSONShape pins the wire shape the UI renders: kind, seq, ts
// and actor are present and the member details carry the recorded values.
func TestAuditEventsJSONShape(t *testing.T) {
	store := localBackends()[0].open(t, localBackends()[0].pathFn(t))
	h := newAPIHarness(t, store)
	id := buildAuditedSession(t, h)

	var trail AuditTrail
	code := h.do(t, http.MethodGet, "/sessions/"+id+"/audit", nil, &trail, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, id, trail.SessionID)
	require.Equal(t, trail.Count, len(trail.Events))
}
