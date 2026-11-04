package reads

// http_test.go — the read-state HTTP surface: the §8.7 permission-scoping
// criterion, the §8.10 mark-on-behalf criterion, and the §6 error catalog
// exercised over a REAL mux against a REAL session handler (the same two
// handlers the server registers), so a code path that only works in-process is
// not mistaken for a working route.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/session"
)

type httpHarness struct {
	srv   *httptest.Server
	f     *fixture
	auth  *Authorizer
	reads *HTTPHandler
}

// newHTTPHarness wires the session handler and the read-state handler on ONE
// router, exactly as cmd/server does.
func newHTTPHarness(t *testing.T, f *fixture, auth *Authorizer) *httpHarness {
	t.Helper()
	sessHandler := session.NewHTTPHandler(f.repo, session.HTTPOptions{})
	h := NewHTTPHandler(f.svc, sessHandler, auth)
	r := mux.NewRouter()
	r.HandleFunc("/sessions/{id}/reads", h.HandleMarkRead).Methods(http.MethodPost)
	r.HandleFunc("/sessions/{id}/reads", h.HandleReadState).Methods(http.MethodGet)
	r.HandleFunc("/sessions/{id}/reads/unread", h.HandleUnread).Methods(http.MethodGet)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &httpHarness{srv: srv, f: f, auth: auth, reads: h}
}

// do issues one request with an optional caller identity header and returns the
// status and decoded body.
func (h *httpHarness) do(t *testing.T, method, path, agentID, body string) (int, []byte) {
	t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if agentID != "" {
		req.Header.Set("X-Agent-ID", agentID)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, buf.Bytes()
}

func decodeInto[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(raw, &v), "body: %s", string(raw))
	return v
}

// ---------------------------------------------------------------------------
// §8.7 TestReadsStatePermissionScoped
// ---------------------------------------------------------------------------

func TestReadsStatePermissionScoped(t *testing.T) {
	f := newJSONLFixture(t)
	h := newHTTPHarness(t, f, &Authorizer{})

	// Two members have read something; bravo (who has not marked) asks.
	_, _, _, err := f.mark(string(session.MemberAgent), agentAlpha, 5, nil)
	require.NoError(t, err)
	_, _, _, err = f.mark(string(session.MemberPrincipal), principalID, 6, nil)
	require.NoError(t, err)

	// A caller that names itself and holds nothing sees ONLY its own row: the
	// other participants are omitted, never zeroed.
	status, raw := h.do(t, http.MethodGet, "/sessions/"+f.sid+"/reads", agentBravo, "")
	require.Equal(t, http.StatusOK, status, string(raw))
	body := decodeInto[readsResponse](t, raw)
	require.Len(t, body.Members, 1, "a non-privileged caller sees one row")
	require.Equal(t, agentBravo, body.Members[0].MemberID)
	require.Equal(t, int64(0), body.Members[0].ReadThroughSeq)
	require.Nil(t, body.Members[0].LastReadAt, "a member that never marked has last_read_at null")
	require.Equal(t, f.lastSeq, body.LastSeq, "last_seq is visible to everyone who may read the session")

	// An operator scope confers session.read_state.all, and the whole roster
	// appears with its real numbers.
	opAuth := &Authorizer{Operators: map[string]bool{agentBravo: true}}
	op := newHTTPHarness(t, f, opAuth)
	status, raw = op.do(t, http.MethodGet, "/sessions/"+f.sid+"/reads", agentBravo, "")
	require.Equal(t, http.StatusOK, status, string(raw))
	all := decodeInto[readsResponse](t, raw)
	require.Len(t, all.Members, 3)
	byMember := map[string]memberReadRow{}
	for _, row := range all.Members {
		byMember[row.MemberID] = row
	}
	require.Equal(t, int64(5), byMember[agentAlpha].ReadThroughSeq)
	require.NotNil(t, byMember[agentAlpha].LastReadAt)
	require.Equal(t, int64(6), byMember[principalID].ReadThroughSeq)
	require.Nil(t, byMember[agentBravo].LastReadAt)

	// The session's private-visibility rule is unchanged: a caller that is not
	// a member of a PRIVATE session cannot reach the read-state surface at all.
	privateID := f.sid + "_private"
	ctx := context.Background()
	require.NoError(t, f.repo.CreateSession(ctx, &session.Session{
		ID: privateID, Kind: session.KindChannel, Visibility: session.VisibilityPrivate,
		CreatedAt: timeNow(), CreatedBy: session.AuthorRef{Agent: agentAlpha},
	}, 1))
	require.NoError(t, f.repo.AddMember(ctx, &session.Member{
		SessionID: privateID, MemberType: session.MemberAgent, MemberID: agentAlpha,
		Role: session.RoleMember, AddedAt: timeNow(),
	}, 2, nil))

	status, raw = h.do(t, http.MethodGet, "/sessions/"+privateID+"/reads", agentBravo, "")
	require.Equal(t, http.StatusForbidden, status, string(raw))
	require.Contains(t, string(raw), "VISIBILITY_FORBIDDEN")

	status, raw = h.do(t, http.MethodGet, "/sessions/"+privateID+"/reads", agentAlpha, "")
	require.Equal(t, http.StatusOK, status, string(raw))
}

// ---------------------------------------------------------------------------
// §8.10 TestMarkOnBehalfDenied
// ---------------------------------------------------------------------------

func TestMarkOnBehalfDenied(t *testing.T) {
	f := newJSONLFixture(t)
	h := newHTTPHarness(t, f, &Authorizer{})

	markBody := func(memberType, memberID string, seq int) string {
		return fmt.Sprintf(`{"member_type":%q,"member_id":%q,"read_through_seq":%d}`, memberType, memberID, seq)
	}

	// bravo marking ITSELF needs no privilege.
	status, raw := h.do(t, http.MethodPost, "/sessions/"+f.sid+"/reads", agentBravo,
		markBody(string(session.MemberAgent), agentBravo, 3))
	require.Equal(t, http.StatusOK, status, string(raw))

	// bravo marking ALPHA is refused: a read receipt is a claim about one's own
	// progress, and bravo holds no session.read_state.write_all.
	status, raw = h.do(t, http.MethodPost, "/sessions/"+f.sid+"/reads", agentBravo,
		markBody(string(session.MemberAgent), agentAlpha, 4))
	require.Equal(t, http.StatusForbidden, status, string(raw))
	require.Contains(t, string(raw), "READS_FOR_OTHER_MEMBER_DENIED")
	require.Equal(t, int64(3), mustCursor(t, f, agentBravo).ReadThroughSeq)
	_, err := f.cursors.Get(context.Background(), f.sid, string(session.MemberAgent), agentAlpha)
	require.ErrorIs(t, err, ErrCursorNotFound, "the refused mark wrote nothing")

	// The same request WITH write_all succeeds.
	op := newHTTPHarness(t, f, &Authorizer{Operators: map[string]bool{agentBravo: true}})
	status, raw = op.do(t, http.MethodPost, "/sessions/"+f.sid+"/reads", agentBravo,
		markBody(string(session.MemberAgent), agentAlpha, 4))
	require.Equal(t, http.StatusOK, status, string(raw))
	require.Equal(t, int64(4), mustCursor(t, f, agentAlpha).ReadThroughSeq)

	// An anonymous caller is the shipped trust-by-reach posture while nothing
	// is armed, and is refused the moment an operator scope exists.
	status, raw = h.do(t, http.MethodPost, "/sessions/"+f.sid+"/reads", "",
		markBody(string(session.MemberPrincipal), principalID, 2))
	require.Equal(t, http.StatusOK, status, string(raw))

	status, raw = op.do(t, http.MethodPost, "/sessions/"+f.sid+"/reads", "",
		markBody(string(session.MemberPrincipal), principalID, 3))
	require.Equal(t, http.StatusForbidden, status, string(raw))
	require.Contains(t, string(raw), "READS_FOR_OTHER_MEMBER_DENIED")
}

// ---------------------------------------------------------------------------
// The §3.1 response shapes and the §6 error catalog.
// ---------------------------------------------------------------------------

func TestMarkReadResponseAndErrorCatalog(t *testing.T) {
	f := newJSONLFixture(t)
	h := newHTTPHarness(t, f, &Authorizer{})

	// 200 with the full §3.1 mark shape.
	status, raw := h.do(t, http.MethodPost, "/sessions/"+f.sid+"/reads", agentAlpha,
		`{"member_type":"agent","member_id":"alpha","read_through_seq":10}`)
	require.Equal(t, http.StatusOK, status, string(raw))
	marked := decodeInto[markReadResponse](t, raw)
	require.Equal(t, "agent", marked.MemberType)
	require.Equal(t, agentAlpha, marked.MemberID)
	require.Equal(t, int64(10), marked.ReadThroughSeq)
	require.False(t, marked.Clamped)
	require.NotNil(t, marked.UpdatedAt)
	wantUnread := int64(0)
	for i := 1; i <= msgCount; i++ {
		if f.base+int64(i) > 10 && authorFor(i) != (session.AuthorRef{Agent: agentAlpha}) {
			wantUnread++
		}
	}
	require.Equal(t, wantUnread, marked.Unread)

	// Absent read_through_seq means "read through the latest".
	status, raw = h.do(t, http.MethodPost, "/sessions/"+f.sid+"/reads", agentBravo,
		`{"member_type":"agent","member_id":"bravo"}`)
	require.Equal(t, http.StatusOK, status, string(raw))
	latest := decodeInto[markReadResponse](t, raw)
	require.Equal(t, f.lastSeq, latest.ReadThroughSeq)
	require.False(t, latest.Clamped, "the last seq is in range, not a clamp")
	require.Equal(t, int64(0), latest.Unread)

	// Clamping: 999999 stores the last seq and says so.
	status, raw = h.do(t, http.MethodPost, "/sessions/"+f.sid+"/reads", "",
		`{"member_type":"principal","member_id":"`+principalID+`","read_through_seq":999999}`)
	require.Equal(t, http.StatusOK, status, string(raw))
	clamped := decodeInto[markReadResponse](t, raw)
	require.True(t, clamped.Clamped)
	require.Equal(t, f.lastSeq, clamped.ReadThroughSeq)

	cases := []struct {
		name   string
		header string
		body   string
		status int
		code   string
	}{
		{"member required", agentAlpha, `{"member_type":"agent","read_through_seq":3}`, http.StatusBadRequest, "READS_MEMBER_REQUIRED"},
		{"member type required", agentAlpha, `{"member_id":"alpha","read_through_seq":3}`, http.StatusBadRequest, "READS_MEMBER_REQUIRED"},
		{"negative seq", agentAlpha, `{"member_type":"agent","member_id":"alpha","read_through_seq":-1}`, http.StatusBadRequest, "READS_SEQ_INVALID"},
		{"both forms", agentAlpha, `{"member_type":"agent","member_id":"alpha","read_through_seq":3,"message_ids":["m_01"]}`, http.StatusBadRequest, "READS_SEQ_INVALID"},
		{"unknown message", agentAlpha, `{"member_type":"agent","member_id":"alpha","message_ids":["m_02","m_nope"]}`, http.StatusNotFound, "READS_MESSAGE_NOT_IN_SESSION"},
		{"rewind", agentAlpha, `{"member_type":"agent","member_id":"alpha","read_through_seq":5}`, http.StatusConflict, "READS_REWIND_REFUSED"},
		// Anonymous (unarmed) so the MEMBER check is what refuses, not the
		// on-behalf check: an identified caller marking a stranger is refused
		// earlier, by write_all, which is the more fundamental refusal.
		{"not a participant", "", `{"member_type":"agent","member_id":"nobody","read_through_seq":5}`, http.StatusForbidden, "READS_MEMBER_NOT_PARTICIPANT"},
		{"unknown field", agentAlpha, `{"member_type":"agent","member_id":"alpha","read_upto":5}`, http.StatusBadRequest, "INVALID_REQUEST"},
		{"empty body", agentAlpha, ``, http.StatusBadRequest, "INVALID_REQUEST"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := h.do(t, http.MethodPost, "/sessions/"+f.sid+"/reads", tc.header, tc.body)
			require.Equal(t, tc.status, status, string(raw))
			require.Contains(t, string(raw), tc.code, "body: %s", string(raw))
		})
	}

	// The stored cursor survived every refusal: 10, unchanged.
	require.Equal(t, int64(10), mustCursor(t, f, agentAlpha).ReadThroughSeq)

	// An unknown session in this realm is SESSION_NOT_FOUND (404), not a 500.
	status, raw = h.do(t, http.MethodPost, "/sessions/no_such_session/reads", agentAlpha,
		`{"member_type":"agent","member_id":"alpha","read_through_seq":1}`)
	require.Equal(t, http.StatusNotFound, status, string(raw))
	require.Contains(t, string(raw), "SESSION_NOT_FOUND")
}

func TestUnreadRoute(t *testing.T) {
	f := newJSONLFixture(t)
	h := newHTTPHarness(t, f, &Authorizer{})

	// A member that has marked half the room sees the other half.
	half := f.base + int64(msgCount/2)
	_, _, _, err := f.mark(string(session.MemberAgent), agentAlpha, half, nil)
	require.NoError(t, err)

	status, raw := h.do(t, http.MethodGet, "/sessions/"+f.sid+"/reads/unread?member_id="+agentAlpha, "", "")
	require.Equal(t, http.StatusOK, status, string(raw))
	body := decodeInto[unreadResponse](t, raw)
	require.NotEmpty(t, body.Unread)
	require.Equal(t, len(body.Unread), body.Count)
	for _, m := range body.Unread {
		require.Greater(t, m.Seq, half, "unread starts after the cursor")
		require.NotEqual(t, agentAlpha, m.Author.Agent, "own messages are never unread")
		require.NotNil(t, m.Tags, "tags is present (possibly empty) per the contract")
	}

	// ?tag= is accepted and, in this tree, passes everything through because
	// CRIER-TAGS-1 has not landed (the reported deviation).
	status, raw = h.do(t, http.MethodGet, "/sessions/"+f.sid+"/reads/unread?member_id="+agentAlpha+"&tag=build&tag=ci", "", "")
	require.Equal(t, http.StatusOK, status, string(raw))
	tagged := decodeInto[unreadResponse](t, raw)
	require.Equal(t, body.Count, tagged.Count)

	// An identified caller need not repeat member_id: it defaults to itself.
	status, raw = h.do(t, http.MethodGet, "/sessions/"+f.sid+"/reads/unread", agentAlpha, "")
	require.Equal(t, http.StatusOK, status, string(raw))
	require.Equal(t, body.Count, decodeInto[unreadResponse](t, raw).Count)

	// With no identity and no member_id there is no member to ask about.
	status, raw = h.do(t, http.MethodGet, "/sessions/"+f.sid+"/reads/unread", "", "")
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	require.Contains(t, string(raw), "READS_MEMBER_REQUIRED")
}

func TestUnconfiguredSurfaceAnswers503(t *testing.T) {
	h := NewHTTPHandler(nil, nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sessions/s1/reads", nil)
	h.HandleReadState(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "READS_UNCONFIGURED")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustCursor(t *testing.T, f *fixture, memberID string) *Cursor {
	t.Helper()
	c, err := f.cursors.Get(context.Background(), f.sid, string(session.MemberAgent), memberID)
	require.NoError(t, err)
	return c
}

func timeNow() time.Time { return time.Now().UTC() }
