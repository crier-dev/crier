package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/permissions"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

func lifecycleContract(t *testing.T, open func() Repository) {
	t.Helper()
	ctx := context.Background()
	repo := open()
	h := newAPIHarness(t, repo)
	room := h.createRoom(t)
	url := "/sessions/" + room + "/state"
	transition := func(state string, status int, code, detail string) sessionView {
		t.Helper()
		var raw json.RawMessage
		require.Equal(t, status, h.do(t, http.MethodPost, url, map[string]string{"state": state, "sender": "atlas"}, &raw, nil), string(raw))
		if code != "" {
			var refusal map[string]string
			require.NoError(t, json.Unmarshal(raw, &refusal))
			require.Equal(t, map[string]string{"error": code, "detail": detail}, refusal)
		}
		var view sessionView
		if status == 200 {
			require.NoError(t, json.Unmarshal(raw, &view))
		}
		return view
	}
	transition("open", 409, "SESSION_ALREADY_OPEN", fmt.Sprintf("session %q is already open; cannot reopen it", room))
	transition("other", 400, "INVALID_SESSION_STATE", `state must be "open" or "closed"`)
	var refused map[string]string
	require.Equal(t, 404, h.do(t, "POST", "/sessions/unknown/state", map[string]string{"state": "closed", "sender": "atlas"}, &refused, nil))
	require.Equal(t, map[string]string{"error": "SESSION_NOT_FOUND", "detail": `session "unknown" does not exist`}, refused)
	before, err := repo.NextSeq(ctx, room)
	require.NoError(t, err)
	closed := transition("closed", 200, "", "")
	require.Equal(t, "closed", closed.State)
	require.NotNil(t, closed.ClosedAt)
	transition("closed", 409, "SESSION_ALREADY_CLOSED", fmt.Sprintf("session %q is already closed; cannot close it again", room))
	require.Equal(t, 409, h.do(t, "POST", "/sessions/"+room+"/messages", map[string]any{"sender": "atlas", "payload": map[string]string{"text": "must refuse"}}, &refused, nil))
	require.Equal(t, map[string]string{"error": "SESSION_CLOSED", "detail": fmt.Sprintf("session %q is closed and admits no new message (§1.3)", room)}, refused)
	h.srv.Close()
	require.NoError(t, repo.Close())
	repo = open()
	h = newAPIHarness(t, repo)
	st, err := repo.Load(ctx, room)
	require.NoError(t, err)
	require.Equal(t, SessionClosed, st.Session.State)
	require.True(t, st.Session.ClosedAt.Equal(*closed.ClosedAt))
	require.Len(t, st.Lifecycle, 1)
	event := st.Lifecycle[0]
	require.Equal(t, RecordClose, event.Type)
	require.Equal(t, room, event.SessionID)
	require.Equal(t, before, event.Seq)
	require.Equal(t, AuthorRef{Agent: "atlas"}, *event.Actor)
	require.Equal(t, SenderIdentityClaimed, event.SenderIdentityBasis)
	require.Equal(t, "Session "+room+" marked complete", event.Reason)
	require.True(t, event.TS.Equal(*closed.ClosedAt))
	require.Empty(t, st.Messages)
	var tr transcriptResponse
	require.Equal(t, 200, h.do(t, "GET", "/sessions/"+room+"/messages", nil, &tr, nil))
	var transitions []LifecycleEvent
	for _, ev := range tr.SystemEvents {
		if ev.Type == RecordClose || ev.Type == RecordReopen {
			transitions = append(transitions, ev)
		}
	}
	require.Equal(t, lifecycleEvents(st.Lifecycle), transitions)
	opened := transition("open", 200, "", "")
	require.Equal(t, "open", opened.State)
	require.Nil(t, opened.ClosedAt)
	h.srv.Close()
	require.NoError(t, repo.Close())
	repo = open()
	defer func() { require.NoError(t, repo.Close()) }()
	st, err = repo.Load(ctx, room)
	require.NoError(t, err)
	require.Equal(t, SessionOpen, st.Session.State)
	require.Nil(t, st.Session.ClosedAt)
	require.Len(t, st.Lifecycle, 2)
	require.Equal(t, RecordReopen, st.Lifecycle[1].Type)
	require.Equal(t, SenderIdentityClaimed, st.Lifecycle[1].SenderIdentityBasis)
	require.Equal(t, AuthorRef{Agent: "atlas"}, *st.Lifecycle[1].Actor)
	records, err := repo.Records(ctx, room)
	require.NoError(t, err)
	var events []*Record
	for _, rec := range records {
		if isLifecycleRecord(rec) {
			events = append(events, rec)
		}
	}
	require.Equal(t, st.Lifecycle, events, "event rows must be persisted, not handler response echoes")
	h = newAPIHarness(t, repo)
	require.Equal(t, 201, h.do(t, "POST", "/sessions/"+room+"/messages", map[string]any{"sender": "atlas", "targets": []map[string]string{{"kind": "agent", "id": "atlas"}}, "payload": map[string]string{"text": "after reopen"}}, nil, nil))
	t.Logf("%s: closed+reopened with durable attributed event rows; illegal transitions and exact send refusal verified", t.Name())
}

func TestSessionLifecycleDurable(t *testing.T) {
	for _, b := range localBackends() {
		t.Run(b.name, func(t *testing.T) {
			path := b.pathFn(t)
			lifecycleContract(t, func() Repository { return b.open(t, path) })
		})
	}
}

func TestSessionLifecyclePostgres(t *testing.T) {
	dsn := os.Getenv("CRIER_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PostgreSQL lifecycle integration skipped: CRIER_TEST_POSTGRES_DSN is not configured (must name an isolated test database)")
	}
	for _, adapter := range []string{"pgx", "database-sql"} {
		t.Run(adapter, func(t *testing.T) {
			// Dedicated schema prevents collisions and is dropped on completion.
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			admin, err := NewPostgresSQLStore(ctx, dsn)
			require.NoError(t, err)
			schema := fmt.Sprintf("df446_%d", time.Now().UnixNano())
			_, err = admin.db.ExecContext(ctx, `CREATE SCHEMA `+schema)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, e := admin.db.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
				require.NoError(t, e)
				require.NoError(t, admin.Close())
			})
			// pgx parses both URL and keyword/value DSNs; add search_path through config.
			cfg, err := parseLifecyclePGConfig(dsn, schema)
			require.NoError(t, err)
			lifecycleContract(t, func() Repository {
				var repo Repository
				var e error
				if adapter == "pgx" {
					repo, e = NewPostgresStore(ctx, cfg)
				} else {
					repo, e = NewPostgresSQLStore(ctx, cfg)
				}
				require.NoError(t, e)
				return repo
			})
		})
	}
}

func TestSessionLifecycleMemorySQLite(t *testing.T) {
	repo, err := NewSQLiteStore(":memory:")
	require.NoError(t, err)
	defer func() { require.NoError(t, repo.Close()) }()
	h := newAPIHarness(t, repo)
	room := h.createRoom(t)
	for _, state := range []string{"closed", "open"} {
		require.Equal(t, 200, h.do(t, "POST", "/sessions/"+room+"/state", map[string]string{"state": state, "sender": "atlas"}, nil, nil))
	}
	st, err := repo.Load(context.Background(), room)
	require.NoError(t, err)
	require.Equal(t, SessionOpen, st.Session.State)
	require.Len(t, st.Lifecycle, 2)
}

func TestSessionLifecycleConcurrentClose(t *testing.T) {
	for _, b := range localBackends() {
		t.Run(b.name, func(t *testing.T) {
			path := b.pathFn(t)
			a := b.open(t, path)
			defer func() { _ = a.Close() }()
			other := b.open(t, path)
			defer func() { _ = other.Close() }()
			one := newAPIHarness(t, a)
			two := newAPIHarness(t, other)
			room := one.createRoom(t)
			var wg sync.WaitGroup
			codes := make(chan int, 2)
			for _, h := range []*apiHarness{one, two} {
				wg.Add(1)
				go func(h *apiHarness) {
					defer wg.Done()
					res, err := http.Post(h.srv.URL+"/sessions/"+room+"/state", "application/json", strings.NewReader(`{"state":"closed","sender":"atlas"}`))
					if err != nil {
						codes <- 0
						return
					}
					defer func() { _ = res.Body.Close() }()
					codes <- res.StatusCode
				}(h)
			}
			wg.Wait()
			close(codes)
			var got []int
			for code := range codes {
				got = append(got, code)
			}
			sort.Ints(got)
			require.Equal(t, []int{200, 409}, got)
			st, err := a.Load(context.Background(), room)
			require.NoError(t, err)
			require.Len(t, st.Lifecycle, 1)
		})
	}
}

func TestSessionLifecycleSQLAtomicRollback(t *testing.T) {
	repo := newTestSQLiteStore(t)
	h := newAPIHarness(t, repo)
	room := h.createRoom(t)
	_, err := repo.db.Exec(`CREATE TRIGGER df446_reject_event BEFORE INSERT ON chat_session_lifecycle BEGIN SELECT RAISE(ABORT, 'injected event failure'); END`)
	require.NoError(t, err)
	before, err := repo.NextSeq(context.Background(), room)
	require.NoError(t, err)
	require.Equal(t, 500, h.do(t, "POST", "/sessions/"+room+"/state", map[string]string{"state": "closed", "sender": "atlas"}, nil, nil))
	st, err := repo.Load(context.Background(), room)
	require.NoError(t, err)
	require.Equal(t, SessionOpen, st.Session.State)
	require.Nil(t, st.Session.ClosedAt)
	require.Empty(t, st.Lifecycle)
	after, err := repo.NextSeq(context.Background(), room)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestSessionLifecycleIngressProof(t *testing.T) {
	repo := newTestSQLiteStore(t)
	h := newAPIHarness(t, repo)
	room := h.createRoom(t)
	handler := NewHTTPHandler(repo, HTTPOptions{RequireAgentSignature: true, AuthorizeSender: func(w http.ResponseWriter, r *http.Request, required bool) (bool, bool) {
		require.True(t, required)
		return true, true
	}})
	for _, tc := range []struct {
		name, body, id string
		proof          bool
		status         int
	}{
		{"required", `{"state":"closed","sender":"atlas"}`, "atlas", false, 401},
		{"mismatched", `{"state":"closed","sender":"atlas"}`, "other", true, 403},
		{"principal not authenticated by agent key", `{"state":"closed","principal_id":"atlas"}`, "atlas", true, 403},
		{"no actor", `{"state":"closed"}`, "atlas", true, 400},
		{"forged basis", `{"state":"closed","sender":"atlas","sender_identity_basis":"sender-identity-verified"}`, "atlas", true, 400},
		{"verified", `{"state":"closed","sender":"atlas"}`, "atlas", true, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/sessions/"+room+"/state", bytes.NewBufferString(tc.body))
			r = mux.SetURLVars(r, map[string]string{"id": room})
			r.Header.Set("X-Agent-ID", tc.id)
			if tc.proof {
				r.Header.Set("X-Agent-Sig", "test-verifier-fixture")
			}
			w := httptest.NewRecorder()
			handler.HandleSessionState(w, r)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			st, err := repo.Load(context.Background(), room)
			require.NoError(t, err)
			if tc.status != 200 {
				require.Empty(t, st.Lifecycle)
				require.Equal(t, SessionOpen, st.Session.State)
			} else {
				require.Len(t, st.Lifecycle, 1)
				require.Equal(t, SenderIdentityVerified, st.Lifecycle[0].SenderIdentityBasis)
			}
		})
	}
}

func TestSessionLifecycleAdminRequired(t *testing.T) {
	for _, b := range localBackends() {
		t.Run(b.name, func(t *testing.T) {
			repo := b.open(t, b.pathFn(t))
			t.Cleanup(func() { _ = repo.Close() })
			h := newTaskHarness(t, repo)
			room := h.createRoomAs(t)
			var refusal map[string]string
			require.Equal(t, 403, h.do(t, "POST", "/sessions/"+room+"/state", map[string]string{"state": "closed", "principal_id": "prin_owner", "as_agent": fixAgentA}, &refusal, nil))
			require.Equal(t, map[string]string{"error": "SESSION_STATE_FORBIDDEN", "detail": "admin on the session is required to close or reopen it"}, refusal)
			st, err := repo.Load(context.Background(), room)
			require.NoError(t, err)
			require.Empty(t, st.Lifecycle)
			require.Equal(t, SessionOpen, st.Session.State)
			h.seedGrant(t, &permissions.Grant{ID: "state-admin", Kind: "grant", Principal: "prin_owner", Subject: permissions.Subject{Type: permissions.SubjectSession, Ref: room}, Actions: []permissions.Action{permissions.ActionAdmin}, GrantedBy: "prin_owner"})
			require.Equal(t, 200, h.do(t, "POST", "/sessions/"+room+"/state", map[string]string{"state": "closed", "principal_id": "prin_owner", "as_agent": fixAgentA}, nil, nil))
			st, err = repo.Load(context.Background(), room)
			require.NoError(t, err)
			require.Len(t, st.Lifecycle, 1)
			require.Equal(t, AuthorRef{Principal: "prin_owner", AsAgent: fixAgentA}, *st.Lifecycle[0].Actor)
			require.Equal(t, SenderIdentityClaimed, st.Lifecycle[0].SenderIdentityBasis)
		})
	}
}

// Keep schema paths isolated even when a test DSN has other query parameters.
func parseLifecyclePGConfig(dsn, schema string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	return dsn + " search_path=" + schema, nil
}
