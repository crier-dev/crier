package main

// hermes_live_delivery_test.go — CRIER-HARNESS-6 criterion 1 against the SERVED
// route: a unique token pushed through POST /ingress/messages into a Crier
// session bound to harness=hermes must come back delivery.state=dispatched, and
// then be found in that Hermes session's own transcript on disk.
//
// This file is the HTTP half of the joint. cmd/server/harness_metrics_test.go
// already proves the route dispatches to a fake harness binary; here the binary
// is the operator's real `hermes` and the far end is a real Hermes session, so
// the whole path — route, harnessDispatch, Registry.Deliver, the adapter's
// `hermes chat -Q --resume <id> -q <text>`, and Hermes's own session store — is
// exercised in one push.
//
// Gated on CRIER_TEST_LIVE_HARNESS=1: it spends real model turns and writes into
// the live Hermes session store. It creates ONE fresh session per run, bound to
// a throwaway Crier session in a temp log root, and deletes nothing.

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/session"
)

// liveHermesSessionID matches the line the CLI prints on exit.
var liveHermesSessionID = regexp.MustCompile(`(?m)^session_id:\s*(\S+)\s*$`)

// liveHermesGate enforces the gate and resolves the real binary; it skips (never
// fails) when the leg is unarmed or hermes is not installed.
func liveHermesGate(t *testing.T) string {
	t.Helper()
	if os.Getenv("CRIER_TEST_LIVE_HARNESS") != "1" {
		t.Skip("set CRIER_TEST_LIVE_HARNESS=1 to run the live hermes ingress leg (needs the hermes CLI and spends real model turns)")
	}
	bin, err := exec.LookPath("hermes")
	if err != nil {
		t.Skip("hermes is not installed on this host; the route's dispatch wiring is covered by the fake-binary tests")
	}
	return bin
}

// liveHermesScrubAmbient removes the caller's own Hermes session binding so the
// dispatch child resumes the session under test rather than inheriting the
// caller's. Values are restored on cleanup.
func liveHermesScrubAmbient(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"HERMES_SESSION_ID",
		"HERMES_SESSION_KEY",
		"HERMES_SESSION_SCOPE_ID",
		"HERMES_SESSION_CHAT_ID",
		"HERMES_SESSION_THREAD_ID",
		"HERMES_SESSION_PLATFORM",
		"HERMES_SESSION_MESSAGE_ID",
		"HERMES_SESSION_SOURCE",
		"HERMES_UI_SESSION_ID",
		"HERMES_CRON_SESSION",
	} {
		value, had := os.LookupEnv(key)
		if !had {
			continue
		}
		require.NoError(t, os.Unsetenv(key))
		t.Cleanup(func() { _ = os.Setenv(key, value) })
	}
}

// liveHermesHome is the profile home the CLI writes its session store under.
func liveHermesHome(t *testing.T) string {
	t.Helper()
	if home := strings.TrimSpace(os.Getenv("HERMES_HOME")); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	return filepath.Join(home, ".hermes")
}

// liveHermesCreateSession makes ONE fresh session for this run and returns its
// id. This is setup — the state an operator creates when they hand Crier an
// existing agent thread — not the delivery under test.
func liveHermesCreateSession(t *testing.T, bin, workdir string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "chat", "-Q", "--in", workdir,
		"-q", "This is a Crier delivery probe. Reply with exactly: SESSION-READY")
	cmd.Env = os.Environ()
	// "session_id: <id>" is printed on STDERR after the answer; stdout carries
	// only the response, so both streams are captured and quoted.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("create a live hermes session: %v\nstderr:\n%s\nstdout:\n%s",
			err, strings.TrimSpace(stderr.String()), strings.TrimSpace(stdout.String()))
	}
	m := liveHermesSessionID.FindSubmatch([]byte(stdout.String() + "\n" + stderr.String()))
	require.NotNil(t, m, "the CLI printed no session_id line:\nstdout:\n%s\nstderr:\n%s",
		strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()))
	id := strings.TrimSpace(string(m[1]))
	require.NotEmpty(t, id)
	return id
}

// liveHermesTranscriptRows returns the stored messages of one session, oldest
// first, from the operator's own session store (SQLite) — read-only.
func liveHermesTranscriptRows(t *testing.T, storePath, sessionID string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+storePath+"?mode=ro&_pragma=busy_timeout(10000)")
	require.NoError(t, err, "open the hermes session store read-only")
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`select role || ': ' || coalesce(content,'') || ' ' || coalesce(api_content,'')
	                        from messages where session_id = ? order by id`, sessionID)
	require.NoError(t, err, "query transcript for %q", sessionID)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		out = append(out, line)
	}
	require.NoError(t, rows.Err())
	return out
}

// TestIngressDispatchesIntoARealHermesSession is CRIER-HARNESS-6 criteria 1-3 on
// the served surface: boot the real server with the REAL hermes adapter
// registered, push a token through POST /ingress/messages, assert
// delivery.state=dispatched, find the token in Hermes's transcript, then push a
// second token and prove the same Hermes session continued.
func TestIngressDispatchesIntoARealHermesSession(t *testing.T) {
	bin := liveHermesGate(t)
	liveHermesScrubAmbient(t)

	storePath := filepath.Join(liveHermesHome(t), "state.db")
	info, err := os.Stat(storePath)
	require.NoError(t, err, "hermes session store %s", storePath)
	require.False(t, info.IsDir(), "%s is not the SQLite transcript", storePath)

	ctx := context.Background()
	root := t.TempDir()
	workdir := t.TempDir()
	seed, err := session.OpenStore(ctx, config.SessionBackendJSONL, session.StoreOptions{LogRoot: root})
	require.NoError(t, err)

	const crierSession = "s_live_hermes"
	harnessSessionID := liveHermesCreateSession(t, bin, workdir)

	now := time.Now().UTC()
	require.NoError(t, seed.CreateSession(ctx, &session.Session{
		ID:               crierSession,
		Namespace:        "default",
		Kind:             session.KindChannel,
		Title:            crierSession,
		CreatedAt:        now,
		CreatedBy:        session.AuthorRef{Agent: "author"},
		State:            session.SessionOpen,
		Harness:          "hermes",
		HarnessSessionID: harnessSessionID,
	}, 1))
	// The author joins so the mode=session participant gate admits the push.
	require.NoError(t, seed.AddMember(ctx, &session.Member{
		SessionID:  crierSession,
		MemberType: session.MemberAgent,
		MemberID:   "author",
		Role:       session.RoleMember,
		AddedAt:    now,
	}, 2, nil))
	require.NoError(t, seed.Close())

	base := startTestServerWithEnv(t, map[string]string{
		"CR_REQUIRE_AGENT_SIG":      "false",
		"CR_SESSION_BACKEND":        "jsonl",
		"CR_SESSION_LOG_ROOT":       root,
		"CR_GROUP_ROOT":             t.TempDir(),
		"CR_HARNESS_ENABLED":        "true",
		"CR_HARNESS_DEFAULT":        "hermes",
		"CR_HARNESS_TIMEOUT":        "300",
		"CR_HARNESS_HERMES_BINARY":  bin,
		"CR_HARNESS_HERMES_WORKDIR": workdir,
	})
	client := &http.Client{Timeout: 320 * time.Second}

	marker := "crier-live-hermes-ingress-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	tokenOne, tokenTwo := marker+"-one", marker+"-two"

	type pushResult struct {
		Delivery struct {
			Mode    string `json:"mode"`
			Harness string `json:"harness"`
			State   string `json:"state"`
			Error   string `json:"error"`
		} `json:"delivery"`
	}
	push := func(messageID, token string) pushResult {
		t.Helper()
		var out pushResult
		start := time.Now()
		code, raw := sessionDo(t, client, http.MethodPost, base+"/ingress/messages", map[string]any{
			"mode":   "session",
			"target": map[string]any{"session_id": crierSession},
			"author": map[string]string{"agent": "author"},
			"text":   "Reply with exactly this token and nothing else: " + token,
		}, &out)
		t.Logf("push %s: http=%d elapsed=%s delivery=%+v", messageID, code, time.Since(start).Round(time.Millisecond), out.Delivery)
		require.Equal(t, http.StatusOK, code, "body: %s", raw)
		require.Equal(t, "harness", out.Delivery.Mode, "the push must have been dispatched, not stored: %s", raw)
		require.Equal(t, "hermes", out.Delivery.Harness, "body: %s", raw)
		require.Equal(t, "dispatched", out.Delivery.State,
			"delivery error %q; the push did not reach the hermes session: %s", out.Delivery.Error, raw)
		require.Empty(t, out.Delivery.Error, "body: %s", raw)
		return out
	}

	// ── Criterion 1: the token travels the served route and is dispatched ───
	push("m_ingress_live_1", tokenOne)

	// ── Criterion 2: the token is in Hermes's own transcript on disk ────────
	rows := liveHermesTranscriptRows(t, storePath, harnessSessionID)
	found := false
	for _, line := range rows {
		if strings.Contains(line, tokenOne) {
			found = true
			break
		}
	}
	require.True(t, found,
		"the delivered token %q is not in the hermes transcript for session %q\nfile: %s (table messages)\nrows:\n%s",
		tokenOne, harnessSessionID, storePath, strings.Join(rows, "\n"))
	t.Logf("transcript evidence: file=%s table=messages session_id=%s (%d rows) carries %s",
		storePath, harnessSessionID, len(rows), tokenOne)

	// ── Criterion 3: a second push continues the SAME session ───────────────
	push("m_ingress_live_2", tokenTwo)
	after := liveHermesTranscriptRows(t, storePath, harnessSessionID)
	require.Greater(t, len(after), len(rows),
		"the transcript did not grow after the second push (file %s, session %s)", storePath, harnessSessionID)
	joined := strings.Join(after, "\n")
	require.Contains(t, joined, tokenOne, "continuation lost the first token; file %s session %s", storePath, harnessSessionID)
	require.Contains(t, joined, tokenTwo, "continuation token missing; file %s session %s", storePath, harnessSessionID)

	t.Logf("continuation proven through the served route: one hermes session %q, %d -> %d rows in %s",
		harnessSessionID, len(rows), len(after), storePath)
	t.Logf("transport: CLI resume — the adapter ran `hermes chat -Q --resume %s -q <text>`; "+
		"the gateway POST /v1/responses create path was NOT used (the binding was pre-recorded, as a deployment records it)",
		harnessSessionID)
	t.Logf("board rows: crier=%s harness=hermes harness_session_id=%s", crierSession, harnessSessionID)
}
