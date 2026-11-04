package main

// ingress_test.go — the HTTP integration rows of COURIER-INGRESS §7 (10-11)
// plus the parity check that matters most for the old surface: an inbox push
// made through /ingress/messages is the SAME kind of durable entry, with the
// same ack semantics, as one made through /agents/{id}/inbox (test 7, proven
// here against the real registered dispatcher rather than a unit double).

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestIngressRouteRegisteredAndAuthenticated is §7 test 10: the route exists on
// the served mux, sits INSIDE the authenticated subrouter (401 without the
// bearer on an auth-enabled relay, 201 with it), and is not a public path.
func TestIngressRouteRegisteredAndAuthenticated(t *testing.T) {
	parallelBoot(t)
	const token = "ingress-test-deployment-access"
	base := startTestServerWithEnv(t, map[string]string{
		"CR_AUTH_TOKEN":        token,
		"CR_REQUIRE_AGENT_SIG": "false",
		"CR_SESSION_BACKEND":   "jsonl",
		"CR_SESSION_LOG_ROOT":  t.TempDir(),
		"CR_GROUP_ROOT":        t.TempDir(),
	})

	body := map[string]any{
		"mode":   "new_session",
		"target": map[string]any{"template": map[string]any{"title": "auth"}},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "hello",
	}

	// Without the bearer: refused before the handler runs.
	anon := &http.Client{Timeout: 5 * time.Second}
	code, raw := sessionDo(t, anon, http.MethodPost, base+"/ingress/messages", body, nil)
	require.Equal(t, http.StatusUnauthorized, code, "unauthenticated ingress must be 401, got %s", raw)

	// With it: the documented success.
	auth := &http.Client{Timeout: 5 * time.Second, Transport: sessionBearerTransport{token}}
	var created struct {
		Mode           string `json:"mode"`
		SessionID      string `json:"session_id"`
		CreatedSession bool   `json:"created_session"`
	}
	code, raw = sessionDo(t, auth, http.MethodPost, base+"/ingress/messages", body, &created)
	require.Equal(t, http.StatusCreated, code, "body: %s", raw)
	require.Equal(t, "new_session", created.Mode)
	require.True(t, created.CreatedSession)
	require.NotEmpty(t, created.SessionID)
}

// TestIngressAppearsInTranscript is §7 test 11: after a new_session ingress the
// session's own transcript read returns the message at seq 1 with the SAME
// message_id — proof the push went through the session store, not a side path.
func TestIngressAppearsInTranscript(t *testing.T) {
	parallelBoot(t)
	base := startTestServerWithEnv(t, map[string]string{
		"CR_SESSION_BACKEND":   "jsonl",
		"CR_SESSION_LOG_ROOT":  t.TempDir(),
		"CR_GROUP_ROOT":        t.TempDir(),
		"CR_REQUIRE_AGENT_SIG": "false",
	})
	client := &http.Client{Timeout: 10 * time.Second}

	var created struct {
		SessionID string `json:"session_id"`
		MessageID string `json:"message_id"`
		Seq       int64  `json:"seq"`
	}
	code, raw := sessionDo(t, client, http.MethodPost, base+"/ingress/messages", map[string]any{
		"mode": "new_session",
		"target": map[string]any{"template": map[string]any{
			"title": "triage",
			"members": []map[string]string{
				{"member_type": "agent", "member_id": "alpha"},
			},
		}},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "build 4412 failed on main",
		"tags":   []string{"build"},
	}, &created)
	require.Equal(t, http.StatusCreated, code, "body: %s", raw)
	require.NotEmpty(t, created.SessionID)
	require.NotEmpty(t, created.MessageID)
	require.EqualValues(t, 4, created.Seq,
		"session.create=1, the author's auto-join=2, alpha's join=3, the message=4")

	var transcript struct {
		Count    int `json:"count"`
		Messages []struct {
			ID       string   `json:"id"`
			Seq      int64    `json:"seq"`
			Tags     []string `json:"tags"`
			ThreadID string   `json:"thread_id"`
			Payload  struct {
				Text string `json:"text"`
			} `json:"payload"`
		} `json:"messages"`
	}
	code, raw = sessionDo(t, client, http.MethodGet, base+"/sessions/"+created.SessionID+"/messages", nil, &transcript)
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.Equal(t, 1, transcript.Count)
	require.Equal(t, created.MessageID, transcript.Messages[0].ID)
	require.Equal(t, created.Seq, transcript.Messages[0].Seq)
	require.Equal(t, []string{"build"}, transcript.Messages[0].Tags)
	require.Equal(t, created.MessageID, transcript.Messages[0].ThreadID)
	require.Equal(t, "build 4412 failed on main", transcript.Messages[0].Payload.Text)
}

// TestIngressSessionModeAgainstTheServedRoute posts into a session created over
// the session API, and then reads it back through the same route. It proves the
// 200 path and that an ingress push and a /sessions/{id}/messages send compose
// into ONE transcript.
func TestIngressSessionModeAgainstTheServedRoute(t *testing.T) {
	parallelBoot(t)
	base := startTestServerWithEnv(t, map[string]string{
		"CR_SESSION_BACKEND":   "jsonl",
		"CR_SESSION_LOG_ROOT":  t.TempDir(),
		"CR_GROUP_ROOT":        t.TempDir(),
		"CR_REQUIRE_AGENT_SIG": "false",
	})
	client := &http.Client{Timeout: 10 * time.Second}

	for _, agent := range []string{"alpha", "beta"} {
		code, raw := sessionDo(t, client, http.MethodPost, base+"/agents", map[string]any{
			"id": agent, "public_key": strings.Repeat("ab", 32),
		}, nil)
		require.Equal(t, http.StatusCreated, code, "register %s: %s", agent, raw)
	}

	var room struct {
		ID string `json:"id"`
	}
	code, raw := sessionDo(t, client, http.MethodPost, base+"/sessions", map[string]any{
		"title":      "compose",
		"created_by": map[string]string{"agent": "alpha"},
		"members": []map[string]string{
			{"member_type": "agent", "member_id": "alpha"},
			{"member_type": "agent", "member_id": "beta"},
		},
	}, &room)
	require.Equal(t, http.StatusCreated, code, "create: %s", raw)

	var pushed struct {
		Mode      string `json:"mode"`
		SessionID string `json:"session_id"`
		MessageID string `json:"message_id"`
		Outcomes  []struct {
			MemberID string `json:"member_id"`
			Result   string `json:"result"`
		} `json:"outcomes"`
	}
	code, raw = sessionDo(t, client, http.MethodPost, base+"/ingress/messages", map[string]any{
		"mode":   "session",
		"target": map[string]any{"session_id": room.ID},
		"author": map[string]string{"agent": "alpha"},
		"text":   "ingress push",
	}, &pushed)
	require.Equal(t, http.StatusOK, code, "push: %s", raw)
	require.Equal(t, "session", pushed.Mode)
	require.Equal(t, room.ID, pushed.SessionID)
	require.Len(t, pushed.Outcomes, 1, "the other participant is the audience: %s", raw)
	require.Equal(t, "beta", pushed.Outcomes[0].MemberID)
	require.Equal(t, "accepted", pushed.Outcomes[0].Result)

	// A normal send still works beside it, and both messages share a transcript.
	code, raw = sessionDo(t, client, http.MethodPost, base+"/sessions/"+room.ID+"/messages", map[string]any{
		"sender":  "beta",
		"payload": map[string]string{"text": "plain send"},
	}, nil)
	require.Equal(t, http.StatusCreated, code, "send: %s", raw)

	var transcript struct {
		Count    int `json:"count"`
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	code, raw = sessionDo(t, client, http.MethodGet, base+"/sessions/"+room.ID+"/messages", nil, &transcript)
	require.Equal(t, http.StatusOK, code, raw)
	require.Equal(t, 2, transcript.Count)
	require.Equal(t, pushed.MessageID, transcript.Messages[0].ID)
}

// TestIngressInboxModeMatchesTheInboxRoute is §7 test 7 against the real
// dispatcher: the same agent receives a message delivered through
// POST /agents/{id}/inbox and one delivered through /ingress/messages mode=inbox,
// both are retrievable and both are acknowledged the same way. The ingress
// response names the SAME entry id the inbox held.
func TestIngressInboxModeMatchesTheInboxRoute(t *testing.T) {
	parallelBoot(t)
	base := startTestServerWithEnv(t, map[string]string{
		"CR_SESSION_BACKEND":   "jsonl",
		"CR_SESSION_LOG_ROOT":  t.TempDir(),
		"CR_GROUP_ROOT":        t.TempDir(),
		"CR_REQUIRE_AGENT_SIG": "false",
	})
	client := &http.Client{Timeout: 10 * time.Second}

	code, raw := sessionDo(t, client, http.MethodPost, base+"/agents", map[string]any{
		"id": "alpha", "public_key": strings.Repeat("ab", 32),
	}, nil)
	require.Equal(t, http.StatusCreated, code, "register: %s", raw)

	var oldAccept struct {
		ID        string `json:"id"`
		Transport string `json:"transport"`
	}
	code, raw = sessionDo(t, client, http.MethodPost, base+"/agents/alpha/inbox", map[string]any{
		"payload": map[string]string{"text": "via the old route"},
	}, &oldAccept)
	require.Equal(t, http.StatusCreated, code, "old route: %s", raw)
	require.Equal(t, "inbox", oldAccept.Transport)
	require.NotEmpty(t, oldAccept.ID)

	var pushed struct {
		Mode      string `json:"mode"`
		SessionID string `json:"session_id"`
		MessageID string `json:"message_id"`
		Outcomes  []struct {
			InboxEntryID string `json:"inbox_entry_id"`
			Result       string `json:"result"`
		} `json:"outcomes"`
	}
	code, raw = sessionDo(t, client, http.MethodPost, base+"/ingress/messages", map[string]any{
		"mode":   "inbox",
		"target": map[string]any{"agent_id": "alpha"},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "via ingress",
		"tags":   []string{"build"},
	}, &pushed)
	require.Equal(t, http.StatusCreated, code, "ingress: %s", raw)
	require.Equal(t, "inbox", pushed.Mode)
	require.Empty(t, pushed.SessionID, "mode=inbox names no session")
	require.Len(t, pushed.Outcomes, 1)
	require.Equal(t, pushed.MessageID, pushed.Outcomes[0].InboxEntryID,
		"the response names the inbox entry the delivery path stored")
	require.Equal(t, "accepted", pushed.Outcomes[0].Result)

	// Both entries are in the SAME durable inbox, with the same shape.
	var batch struct {
		Messages []struct {
			ID      string   `json:"id"`
			Sender  string   `json:"sender"`
			Tags    []string `json:"tags"`
			Payload []byte   `json:"payload"`
		} `json:"messages"`
		LeaseID string `json:"lease_id"`
	}
	code, raw = sessionDo(t, client, http.MethodGet, base+"/agents/alpha/inbox?max_messages=10", nil, &batch)
	require.Equal(t, http.StatusOK, code, "retrieve: %s", raw)
	require.Len(t, batch.Messages, 2, "both deliveries are queued: %s", raw)
	require.NotEmpty(t, batch.LeaseID)

	byID := map[string]bool{}
	for _, m := range batch.Messages {
		byID[m.ID] = true
	}
	require.True(t, byID[oldAccept.ID], "the old route's entry is present")
	require.True(t, byID[pushed.MessageID], "the ingress entry id is the stored id")

	// Acks are the same: one lease covers both entries, and acking that lease
	// drains the inbox exactly as it always did.
	code, raw = sessionDo(t, client, http.MethodPost, base+"/agents/alpha/inbox/ack", map[string]any{
		"lease_id":    batch.LeaseID,
		"message_ids": []string{oldAccept.ID, pushed.MessageID},
	}, nil)
	require.Equal(t, http.StatusNoContent, code, "ack: %s", raw)

	code, raw = sessionDo(t, client, http.MethodGet, base+"/agents/alpha/inbox", nil, &batch)
	require.Equal(t, http.StatusOK, code, raw)
	require.Empty(t, batch.Messages, "both entries were acked through the same lease")
}

// TestIngressInboxTagsRideTheRouteRecord proves the §6 inbox+tags edge case:
// the ingress entry carries its tags on the inbox record, so ?tag= filters it
// without reading the session it came from.
func TestIngressInboxTagsRideTheRouteRecord(t *testing.T) {
	parallelBoot(t)
	base := startTestServerWithEnv(t, map[string]string{
		"CR_SESSION_BACKEND":   "jsonl",
		"CR_SESSION_LOG_ROOT":  t.TempDir(),
		"CR_GROUP_ROOT":        t.TempDir(),
		"CR_REQUIRE_AGENT_SIG": "false",
	})
	client := &http.Client{Timeout: 10 * time.Second}

	code, raw := sessionDo(t, client, http.MethodPost, base+"/agents", map[string]any{
		"id": "beta", "public_key": strings.Repeat("cd", 32),
	}, nil)
	require.Equal(t, http.StatusCreated, code, "register: %s", raw)

	code, raw = sessionDo(t, client, http.MethodPost, base+"/ingress/messages", map[string]any{
		"mode":   "inbox",
		"target": map[string]any{"agent_id": "beta"},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "tagged while queued",
		"tags":   []string{"build", "urgent"},
	}, nil)
	require.Equal(t, http.StatusCreated, code, "ingress: %s", raw)

	var filtered struct {
		Messages []struct {
			Tags []string `json:"tags"`
		} `json:"messages"`
	}
	code, raw = sessionDo(t, client, http.MethodGet, base+"/agents/beta/inbox?tag=build", nil, &filtered)
	require.Equal(t, http.StatusOK, code, "filtered retrieve: %s", raw)
	require.Len(t, filtered.Messages, 1, "the queued message carries its tags: %s", raw)
	require.Equal(t, []string{"build", "urgent"}, filtered.Messages[0].Tags)

	code, raw = sessionDo(t, client, http.MethodGet, base+"/agents/beta/inbox?tag=nothing", nil, &filtered)
	require.Equal(t, http.StatusOK, code, raw)
	require.Empty(t, filtered.Messages, "an unmatched tag is an empty set, never an error")
}

// TestIngressRefusesUnknownAgentOverHTTP pins the 404 on the served route, so a
// capability-less inbox push to a stranger is refused rather than queued.
func TestIngressRefusesUnknownAgentOverHTTP(t *testing.T) {
	parallelBoot(t)
	base := startTestServerWithEnv(t, map[string]string{
		"CR_SESSION_BACKEND":   "jsonl",
		"CR_SESSION_LOG_ROOT":  t.TempDir(),
		"CR_GROUP_ROOT":        t.TempDir(),
		"CR_REQUIRE_AGENT_SIG": "false",
	})
	client := &http.Client{Timeout: 10 * time.Second}
	var body struct {
		Error string `json:"error"`
	}
	code, raw := sessionDo(t, client, http.MethodPost, base+"/ingress/messages", map[string]any{
		"mode":   "inbox",
		"target": map[string]any{"agent_id": "nobody"},
		"author": map[string]string{"agent": "ci-bot"},
		"text":   "hello",
	}, &body)
	require.Equal(t, http.StatusNotFound, code, "body: %s", raw)
	require.Equal(t, "INGRESS_AGENT_UNKNOWN", body.Error)
}
