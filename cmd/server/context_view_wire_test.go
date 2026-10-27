// context_view_wire_test.go — DF-CRIER-480: ONE wire contract for the
// late-join context view, asserted on the served route table.
//
// The dogfood that filed this row saw GET
// /sessions/{id}/participants/{member_type}/{member_id}/context answer 200 from
// the running server and 404 {"error":"not found"} from an independently driven
// SECOND server on what it took to be the same request, and read that as two
// contracts on one surface. Measured at this tree, the two servers do NOT
// disagree about this operation: with the canonical path both answer 200 for a
// live member (this test boots the route table the same way cmd/server does and
// asserts exactly that). The bare body is the ROUTER's answer
// (cmd/server/fallback.go) to a path this operation never matched — reproduced
// by appending a trailing slash, which gorilla/mux does not match and whose
// near-miss hint (cmd/server/routehint.go) does not classify because its
// segment view trims the slash. It is a PATH miss, not a member state, and it
// never comes out of this handler: every 404 written below carries a named
// code AND a detail.
//
// So what this file pins is the half this row can prove at the wire level:
//
//  1. unknown session        -> 404 {error: SESSION_NOT_FOUND, detail: names it}
//  2. known session, unknown member -> 404 {error: MEMBER_NOT_FOUND, detail: names it}
//  3. known (live) member    -> 200 with the documented MemberContextView
//
// The three BODIES are asserted, not just the status codes, because the
// complaint in the row is about what a body can and cannot tell an integrator.
// A body with an empty `detail`, or with the router's bare "not found" as its
// `error`, fails here on both counts.
//
// The behavioural battery (defaults, every mode, boundary handling, the
// undecided-vs-explicit-`none` distinction) lives in
// internal/session/join_context_test.go; the point of a second test here is
// that this one boots the real route registration, so an operation that is
// implemented but never registered — or registered under a different path
// shape — fails here instead of only in a client.
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The run record's own fixture names, so this test reproduces the dogfood
// probe rather than an invented one.
const (
	contextWireRoom   = "df23-room"
	contextWirePeer   = "df23-peer"
	contextWireLate   = "df23-late"
	contextWireAbsent = "no-such-room"
	contextWireGhost  = "ghost"
)

// contextWireError is the envelope every session-route error uses
// (session.writeAPIError): a named code and a human detail. A router-level
// answer has neither.
type contextWireError struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
}

// contextWireView mirrors the documented MemberContextView schema — only the
// fields this test asserts, decoded from the wire rather than from the Go type
// the handler writes, so a rename on either side shows up here.
type contextWireView struct {
	SessionID   string `json:"session_id"`
	MemberType  string `json:"member_type"`
	MemberID    string `json:"member_id"`
	Decided     bool   `json:"decided"`
	GivenCount  int    `json:"given_count"`
	Explanation string `json:"explanation"`
	Share       *struct {
		Mode              string `json:"mode"`
		BoundaryMessageID string `json:"boundary_message_id"`
	} `json:"share"`
	GeneratedSummary string `json:"generated_summary"`
}

// TestContextViewWireBodies is AC 1 + AC 2 of DF-CRIER-480 on the wire.
func TestContextViewWireBodies(t *testing.T) {
	parallelBoot(t)
	base := startTestServerWithEnv(t, map[string]string{
		"CR_SESSION_BACKEND":   "jsonl",
		"CR_SESSION_LOG_ROOT":  t.TempDir(),
		"CR_REQUIRE_AGENT_SIG": "false", // unsigned fixture, exactly as the run record's server
	})
	client := &http.Client{Timeout: 5 * time.Second}

	// A live room holding a registered agent, created through the same HTTP
	// surface a client uses — no store shortcut, no injected state.
	for _, id := range []string{contextWirePeer, contextWireLate} {
		code, raw := sessionDo(t, client, http.MethodPost, base+"/agents", map[string]any{
			"id": id, "public_key": strings.Repeat("ab", 32),
		}, nil)
		require.Equal(t, http.StatusCreated, code, "register %s: %s", id, raw)
	}
	code, raw := sessionDo(t, client, http.MethodPost, base+"/sessions", map[string]any{
		"id":         contextWireRoom,
		"title":      "DF-CRIER-480 wire bodies",
		"created_by": map[string]string{"principal": "kara"},
		"members": []map[string]string{
			{"member_type": "agent", "member_id": contextWirePeer, "role": "member"},
		},
	}, nil)
	require.Equal(t, http.StatusCreated, code, "create room: %s", raw)

	contextPath := func(room, memberType, memberID string) string {
		return base + "/sessions/" + room + "/participants/" + memberType + "/" + memberID + "/context"
	}

	// decodeNamedError refuses anything that is not this surface's error
	// vocabulary: the bare {"error":"not found"} the row captured has no
	// named code in it and no detail, so it fails both assertions below.
	decodeNamedError := func(t *testing.T, name, body string) contextWireError {
		t.Helper()
		var e contextWireError
		require.NoError(t, json.Unmarshal([]byte(body), &e),
			"%s: body %q is not a JSON error object", name, body)
		require.NotEqual(t, notFoundMessage, e.Error,
			"%s: body %q is the ROUTER's answer, not this operation's — a 404 from this route names its code", name, body)
		require.NotEmpty(t, e.Error, "%s: body %q names no error code", name, body)
		require.NotEmpty(t, e.Detail,
			"%s: body %q carries no detail — the row's complaint is that an integrator could not tell WHICH thing was wrong", name, body)
		return e
	}

	// 1. Unknown session: 404, named, and the detail names the session that
	// was asked for — never the bare router body.
	t.Run("unknown session names SESSION_NOT_FOUND with detail", func(t *testing.T) {
		code, body := sessionDo(t, client, http.MethodGet,
			contextPath(contextWireAbsent, "agent", contextWirePeer), nil, nil)
		require.Equal(t, http.StatusNotFound, code, "body: %s", body)

		e := decodeNamedError(t, "unknown session", body)
		require.Equal(t, "SESSION_NOT_FOUND", e.Error)
		require.Contains(t, e.Detail, contextWireAbsent,
			"the detail must name the session that does not exist, so the caller can tell a wrong session from a wrong member")
	})

	// 2. Known session, unknown member: 404 naming the MEMBER. This is the
	// arm that must not collapse into the session answer — the member is what
	// the caller typed wrong.
	t.Run("known session and unknown member names MEMBER_NOT_FOUND", func(t *testing.T) {
		code, body := sessionDo(t, client, http.MethodGet,
			contextPath(contextWireRoom, "agent", contextWireGhost), nil, nil)
		require.Equal(t, http.StatusNotFound, code, "body: %s", body)

		e := decodeNamedError(t, "unknown member", body)
		require.Equal(t, "MEMBER_NOT_FOUND", e.Error)
		require.Contains(t, e.Detail, contextWireGhost, "the detail must name the member")
		require.Contains(t, e.Detail, "agent", "the detail must name the member type")
		require.Contains(t, e.Detail, contextWireRoom,
			"the detail names the session the member was looked for in — so the caller can tell the session WAS found")
	})

	// 3. Known member: 200 with the documented view. Two members are read
	// here so the arm cannot pass on an empty object: the room's creation
	// member (share recorded at creation) and a member that joins later with
	// an explicit `summary` share.
	t.Run("known member answers 200 with the documented view", func(t *testing.T) {
		code, peerBody := sessionDo(t, client, http.MethodGet,
			contextPath(contextWireRoom, "agent", contextWirePeer), nil, nil)
		require.Equal(t, http.StatusOK, code, "body: %s", peerBody)

		var v contextWireView
		require.NoError(t, json.Unmarshal([]byte(peerBody), &v), "body: %s", peerBody)
		require.Equal(t, contextWireRoom, v.SessionID)
		require.Equal(t, "agent", v.MemberType)
		require.Equal(t, contextWirePeer, v.MemberID)
		require.True(t, v.Decided, "a member the room was created with has a recorded share: %s", peerBody)
		require.NotNil(t, v.Share, "a decided member carries its share: %s", peerBody)
		require.NotEmpty(t, v.Share.Mode, "the share names its mode: %s", peerBody)
		require.NotEmpty(t, v.Explanation, "the view explains what the member holds: %s", peerBody)
		require.GreaterOrEqual(t, v.GivenCount, 0)

		// A late joiner with an explicit summary share: the view must report
		// the recorded answer, not a default. One message is posted first,
		// because a join into a session with NOTHING in it is recorded as
		// `none` by resolveJoinShare ("nothing to share: session has no
		// messages") — which is the surface's own explanation of the dogfood's
		// "0 rows" observation, and a state this test wants on the record.
		msgCode, msgRaw := sessionDo(t, client, http.MethodPost,
			base+"/sessions/"+contextWireRoom+"/messages",
			map[string]any{"payload": map[string]string{"text": "root"}, "sender": contextWirePeer}, nil)
		require.Equal(t, http.StatusCreated, msgCode, "post root message: %s", msgRaw)

		lateCode, lateRaw := sessionDo(t, client, http.MethodPost,
			base+"/sessions/"+contextWireRoom+"/participants",
			map[string]any{
				"member_type":   "agent",
				"member_id":     contextWireLate,
				"context_share": map[string]string{"mode": "summary"},
				"actor":         contextWirePeer,
			}, nil)
		require.Equal(t, http.StatusCreated, lateCode, "add late joiner: %s", lateRaw)

		getCode, body := sessionDo(t, client, http.MethodGet,
			contextPath(contextWireRoom, "agent", contextWireLate), nil, nil)
		require.Equal(t, http.StatusOK, getCode, "body: %s", body)

		var late contextWireView
		require.NoError(t, json.Unmarshal([]byte(body), &late), "body: %s", body)
		require.True(t, late.Decided)
		require.NotNil(t, late.Share)
		require.Equal(t, "summary", late.Share.Mode)
		require.Contains(t, late.Explanation, "GENERATED digest",
			"the explanation of a summary share names the digest: %s", body)
		require.Equal(t, 1, late.GivenCount,
			"the one message recorded before the late join is what the digest covers: %s", body)

		// The distinction the row was missing, asserted as a distinction:
		// an EMPTY view is still a 200. The room's creation member joined a
		// session that held nothing, so its recorded share is `none` and its
		// given set is empty — the dogfood's "0 rows" — and that state is a
		// view, never a 404. An absent member is the 404, proven above.
		require.Equal(t, 0, v.GivenCount,
			"the creation member joined an empty session: %s", peerBody)
		require.Equal(t, "none", v.Share.Mode,
			"a join into a session with no messages is recorded `none` (nothing to share)")
		var asMap map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &asMap), "body: %s", body)
		_, isError := asMap["error"]
		require.False(t, isError, "the 200 body is a view, never an error envelope: %s", body)
	})
}
