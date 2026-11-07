package main

// live_opencode_ingress_test.go — CRIER-HARNESS-7's HTTP leg.
//
// WHAT THIS PROVES. The adapter-level leg
// (internal/harness/opencode/live_delivery_test.go) enters at opencode's
// Deliver and proves the argv, the receipt vocabulary and the transcript. It
// never touches the served surface, so on its own it cannot show that a
// message PUSHED THROUGH POST /ingress/messages reaches a harness=opencode
// session — that gap is the rework finding on judge verdict c0aa2fa8.
//
// This leg closes it and keeps the adapter leg as the storage/transcript
// evidence:
//
//  1. create    -> POST /ingress/messages (mode=session) into a session whose
//     delivery mode is harness=opencode, with no initial binding.
//     Response: delivery.mode=harness, harness=opencode, state=dispatched.
//     The minted opencode session id is then read back over HTTP
//     (GET /sessions -> harness_session_id), which is the §4.3 bind the
//     dispatch path persisted.
//  2. continue  -> a SECOND POST into the SAME Crier session. The response is
//     again dispatched, and GET /sessions reports the SAME
//     harness_session_id: a continuation, not a silent re-create.
//  3. transcript -> BOTH marker tokens are looked up in opencode's own
//     storage under that one session, and exactly one session row carries
//     the title the create path sets ("Crier <crier-session-id>").
//
// The server is the REAL server (booted in-process by startTestServerWithEnv,
// so the mux, middleware and ingress handler are the ones that ship) and the
// opencode adapter is the REAL CLI, selected through CR_HARNESS_OPENCODE_BINARY
// (internal/harness/config.go's adapter env table) with
// CR_HARNESS_OPENCODE_WORKDIR as the delivery cwd. No server_url is set, so the
// receipt is "dispatched" (a server attach would report "accepted" because the
// server owns the turn — CRIER-HARNESS §5).
//
// MEASURED STORAGE LAYOUT (opencode 1.18.29 on this host, re-probed before
// asserting):
//
//	~/.local/share/opencode/opencode.db
//	  table `session` (id, title, directory, version)
//	  table `part`    (id, message_id, session_id, data JSON: {"type":"text",...})
//
// The database is opened READ-ONLY (?mode=ro); this test mutates and deletes
// nothing — the only writes are opencode's own, caused by a real delivery.
//
// ch:trace row=CRIER-HARNESS-7 spec=specs/CRIER-HARNESS.md#opencode-id-policy
// test=cmd/server/live_opencode_ingress_test.go::TestLiveOpencodeIngressDelivery
// doc=specs/CRIER-HARNESS.md evidence=CRIER_TEST_LIVE_HARNESS=1 go test -count=1
// -timeout 600s -run 'LiveOpencodeIngress' ./cmd/server/ witness=none:local-worktree-no-published-endpoint
// verdict=answers-gitreins-verdict .gitreins/history/2026-11-07/c0aa2fa8/verdict.json
// commit=wt/CRIER-HARNESS-7 memory=none:foreman-closeout

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/session"
)

// liveOpencodeIngressTimeout bounds ONE delivery on the server side. opencode
// runs a real model turn, so it is generous; the HTTP client below waits longer
// than this so a slow turn surfaces as a dispatched-or-timed-out receipt rather
// than a client-side timeout that hides the response.
const liveOpencodeIngressTimeout = 240 * time.Second

// TestLiveOpencodeIngressDelivery pushes two unique marker tokens through the
// REAL POST /ingress/messages into a session whose delivery mode is
// harness=opencode, and then looks both tokens up in opencode's own storage.
//
// It is env-gated exactly like the existing live legs: CRIER_TEST_LIVE_HARNESS=1
// and out of -short, because it spends real model turns and writes real
// opencode sessions.
func TestLiveOpencodeIngressDelivery(t *testing.T) {
	if os.Getenv("CRIER_TEST_LIVE_HARNESS") != "1" {
		t.Skip("set CRIER_TEST_LIVE_HARNESS=1 to run the opencode ingress delivery leg (needs the opencode CLI and a configured provider)")
	}
	if testing.Short() {
		t.Skip("live delivery spends a real model turn; -short keeps it out of the ordinary suite")
	}

	bin := liveOpencodeBinary(t)
	version := liveOpencodeVersion(t, bin)
	dbPath := liveOpencodeStorePath(t)
	t.Logf("opencode binary=%s version=%s storage=%s", bin, version, dbPath)

	runID := fmt.Sprintf("%d", time.Now().UnixNano())
	// The brief's marker shape. The continuation token is NOT a prefix of the
	// create token (nor vice versa), so a LIKE lookup cannot match the wrong
	// turn.
	tokenCreate := "crier-live-opencode-http-" + runID
	tokenContinue := "crier-live-opencode-http-cont-" + runID
	crierSession := "s_live_opencode_http_" + runID
	// The title the adapter puts on the create path (opencode.go:190):
	// "Crier " + msg.SessionID. A second row with it means the second push
	// created a fresh opencode session instead of continuing.
	title := "Crier " + crierSession

	// The session is seeded through a repository handle BEFORE the boot: the
	// public create route does not expose a harness binding. The server then
	// opens its own handle over the same JSONL root.
	root := t.TempDir()
	workdir := t.TempDir()
	ctx := context.Background()
	seed, err := session.OpenStore(ctx, config.SessionBackendJSONL, session.StoreOptions{LogRoot: root})
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, seed.CreateSession(ctx, &session.Session{
		ID:        crierSession,
		Namespace: "default",
		Kind:      session.KindChannel,
		Title:     crierSession,
		CreatedAt: now,
		CreatedBy: session.AuthorRef{Agent: "author"},
		State:     session.SessionOpen,
		// The delivery mode this leg proves: harness=opencode, no id yet —
		// exactly the state the FIRST push arrives in.
		Harness: "opencode",
	}, 1))
	// The author joins at seq 2 so the mode=session participant gate admits
	// the pushes below.
	require.NoError(t, seed.AddMember(ctx, &session.Member{
		SessionID:  crierSession,
		MemberType: session.MemberAgent,
		MemberID:   "author",
		Role:       session.RoleMember,
		AddedAt:    now,
	}, 2, nil))
	require.NoError(t, seed.Close())

	base := startTestServerWithEnv(t, map[string]string{
		"CR_REQUIRE_AGENT_SIG":           "false",
		"CR_SESSION_BACKEND":             "jsonl",
		"CR_SESSION_LOG_ROOT":            root,
		"CR_GROUP_ROOT":                  t.TempDir(),
		"CR_HARNESS_ENABLED":             "true",
		"CR_HARNESS_DEFAULT":             "opencode",
		"CR_HARNESS_TIMEOUT":             fmt.Sprintf("%d", int(liveOpencodeIngressTimeout.Seconds())),
		"CR_HARNESS_OPENCODE_BINARY":     bin,
		"CR_HARNESS_OPENCODE_WORKDIR":    workdir,
		"CR_HARNESS_OPENCODE_SERVER_URL": "",
		"CR_HARNESS_CONFIG":              "",
	})
	client := &http.Client{Timeout: liveOpencodeIngressTimeout + 60*time.Second}

	// --- 1. create: push #1 through the real HTTP surface -----------------
	code, raw := liveIngressPush(t, client, base, crierSession, tokenCreate)
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	first := liveIngressDelivery(t, raw)
	require.Equal(t, "harness", first.Mode, "the push must have been dispatched, not stored: %s", raw)
	require.Equal(t, "opencode", first.Harness, "body: %s", raw)
	require.Equal(t, "dispatched", first.State,
		"the default CLI path dispatches (only a server attach accepts): %s", raw)
	require.Empty(t, first.Error, "a dispatched receipt carries no error: %s", raw)

	// The §4.3 bind the dispatch path recorded, read back over HTTP.
	harnessSessionID := liveBoundHarnessSession(t, client, base, crierSession, "")
	require.True(t, strings.HasPrefix(harnessSessionID, "ses_"),
		"the persisted harness_session_id must be opencode's minted id, got %q", harnessSessionID)

	// Control first: a token that was NEVER delivered must match nothing, so
	// a lax query cannot make the assertion below vacuous.
	absent := liveTokenSessions(t, dbPath, tokenCreate+"-never-delivered")
	require.Empty(t, absent, "control token matched %v: the storage query does not discriminate", absent)

	hits := liveTokenSessions(t, dbPath, tokenCreate)
	require.Len(t, hits, 1, "token %q found in %v, want exactly %s; storage=%s",
		tokenCreate, hits, harnessSessionID, dbPath)
	require.Contains(t, hits, harnessSessionID,
		"token %q is stored under %v, not the session the bind reported (%s); storage=%s",
		tokenCreate, hits, harnessSessionID, dbPath)
	t.Logf("create proven over HTTP: token %s -> opencode session %s (storage %s)", tokenCreate, harnessSessionID, dbPath)

	// --- 2. continue: push #2 into the SAME Crier session -----------------
	code, raw = liveIngressPush(t, client, base, crierSession, tokenContinue)
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	second := liveIngressDelivery(t, raw)
	require.Equal(t, "harness", second.Mode, "body: %s", raw)
	require.Equal(t, "opencode", second.Harness, "body: %s", raw)
	require.Equal(t, "dispatched", second.State, "body: %s", raw)
	require.Empty(t, second.Error, "a dispatched receipt carries no error: %s", raw)

	// Same binding after the second push: a continuation, not a re-create.
	continued := liveBoundHarnessSession(t, client, base, crierSession, harnessSessionID)
	require.Equal(t, harnessSessionID, continued,
		"the second push moved the binding from %s to %s: a silent re-create splits the transcript",
		harnessSessionID, continued)

	hits2 := liveTokenSessions(t, dbPath, tokenContinue)
	require.Len(t, hits2, 1, "continue token %q found in %v, want exactly %s; storage=%s",
		tokenContinue, hits2, harnessSessionID, dbPath)
	require.Contains(t, hits2, harnessSessionID,
		"continue token %q is stored under %v, not the continued session %s; storage=%s",
		tokenContinue, hits2, harnessSessionID, dbPath)
	require.Equal(t, 1, liveSessionRowsWithTitle(t, dbPath, title),
		"title %q must have exactly one opencode session row after the continuation; a second row means the second push created a fresh session", title)

	// Both turns in the one stored session.
	first2 := liveTokenSessions(t, dbPath, tokenCreate)
	require.Contains(t, first2, harnessSessionID,
		"the first token left session %s after the continuation; transcripts %v", harnessSessionID, first2)

	t.Logf("continuation proven over HTTP: session %s holds both %s and %s; storage=%s",
		crierSession, tokenCreate, tokenContinue, dbPath)
	t.Logf("CRIER-HARNESS-7 HTTP evidence: POST /ingress/messages -> opencode %s, storage %s, harness_session_id %s, crier session %s",
		version, dbPath, harnessSessionID, crierSession)
}

// liveIngressReport is the subset of the /ingress/messages response this leg
// asserts on.
type liveIngressReport struct {
	Mode    string `json:"mode"`
	Harness string `json:"harness"`
	State   string `json:"state"`
	Error   string `json:"error"`
}

// liveIngressPush POSTs one message into an existing session and returns the
// status and the raw body. The body is returned verbatim so the assertion is
// always on what the server actually wrote.
func liveIngressPush(t *testing.T, client *http.Client, base, sessionID, text string) (int, string) {
	t.Helper()
	return sessionDo(t, client, http.MethodPost, base+"/ingress/messages", map[string]any{
		"mode":   "session",
		"target": map[string]any{"session_id": sessionID},
		"author": map[string]string{"agent": "author"},
		"text":   "Remember this token for later: " + text + ". Reply with exactly: ACK",
	}, nil)
}

// liveIngressDelivery decodes the delivery object out of an /ingress/messages
// body.
func liveIngressDelivery(t *testing.T, raw string) liveIngressReport {
	t.Helper()
	var res struct {
		Delivery liveIngressReport `json:"delivery"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &res), "decode ingress response: %s", raw)
	return res.Delivery
}

// liveBoundHarnessSession reads GET /sessions and returns the
// harness_session_id the server reports for crierSession. When want is
// non-empty the binding must already be stable; either way this is the HTTP
// view of the §4.3 record, not a store read.
func liveBoundHarnessSession(t *testing.T, client *http.Client, base, crierSession, want string) string {
	t.Helper()
	status, _, body := get(t, client, base+"/sessions", "")
	require.Equal(t, http.StatusOK, status, "GET /sessions: %s", body)
	var res struct {
		Sessions []struct {
			ID               string `json:"id"`
			Harness          string `json:"harness"`
			HarnessSessionID string `json:"harness_session_id"`
		} `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &res), "decode GET /sessions: %s", body)
	for _, s := range res.Sessions {
		if s.ID != crierSession {
			continue
		}
		require.Equal(t, "opencode", s.Harness,
			"session %s reports harness %q, want opencode", crierSession, s.Harness)
		if want != "" {
			require.Equal(t, want, s.HarnessSessionID,
				"the binding moved: %q -> %q (a continuation must keep it)", want, s.HarnessSessionID)
		}
		return s.HarnessSessionID
	}
	t.Fatalf("GET /sessions did not list %s: %s", crierSession, body)
	return ""
}

// liveOpencodeBinary resolves the opencode CLI: the CR_HARNESS_OPENCODE_BINARY
// knob first (the same env the boot reads), then ~/.opencode/bin/opencode, then
// PATH. Missing entirely is a skip — this host cannot run the leg.
func liveOpencodeBinary(t *testing.T) string {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv("CR_HARNESS_OPENCODE_BINARY")); v != "" {
		if _, err := os.Stat(v); err != nil {
			t.Fatalf("CR_HARNESS_OPENCODE_BINARY=%s: %v", v, err)
		}
		return v
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".opencode", "bin", "opencode")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if p, err := exec.LookPath("opencode"); err == nil {
		return p
	}
	t.Skip("opencode is not installed on this host")
	return ""
}

// liveOpencodeVersion measures `opencode --version` — the version logged as
// evidence for the run.
func liveOpencodeVersion(t *testing.T, bin string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	require.NoError(t, err, "opencode --version")
	fields := strings.Fields(strings.TrimSpace(string(out)))
	require.NotEmpty(t, fields, "opencode --version printed nothing usable")
	return strings.TrimPrefix(fields[0], "v")
}

// liveOpencodeStorePath returns the opencode storage database, failing with
// both the path and the layout assumption so a moved store says so plainly.
func liveOpencodeStorePath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory to resolve opencode storage: %v", err)
	}
	dbPath := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("opencode storage database not found at %s (%v); re-probe the layout before trusting this test", dbPath, err)
	}
	return dbPath
}

// liveOpenStore opens opencode's database READ-ONLY. A read on a WAL database
// is served from the -wal file, so the newest turn is visible; mode=ro
// guarantees this test cannot write to, checkpoint or migrate it.
func liveOpenStore(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(10000)")
	require.NoError(t, err, "open opencode storage %s read-only", dbPath)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// liveTokenSessions returns session id -> number of stored parts containing
// token. Exactly one session is the proof of delivery.
func liveTokenSessions(t *testing.T, dbPath, token string) map[string]int {
	t.Helper()
	db := liveOpenStore(t, dbPath)
	rows, err := db.Query(`SELECT session_id, COUNT(*) FROM part WHERE data LIKE ? GROUP BY session_id`, "%"+token+"%")
	require.NoError(t, err, "query opencode storage %s table `part`", dbPath)
	defer func() { _ = rows.Close() }()
	hits := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		require.NoError(t, rows.Scan(&id, &n), "scan opencode storage row")
		hits[id] = n
	}
	require.NoError(t, rows.Err(), "iterate opencode storage %s", dbPath)
	return hits
}

// liveSessionRowsWithTitle counts opencode session rows carrying title. The
// create path sets it; a continuation with `-s` must not mint a second row.
func liveSessionRowsWithTitle(t *testing.T, dbPath, title string) int {
	t.Helper()
	db := liveOpenStore(t, dbPath)
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM session WHERE title = ?`, title).Scan(&n),
		"count sessions titled %q in opencode storage %s", title, dbPath)
	return n
}
