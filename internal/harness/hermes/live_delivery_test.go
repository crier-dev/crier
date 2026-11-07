package hermes_test

// live_delivery_test.go — CRIER-HARNESS-6: prove delivery into a REAL Hermes
// session, end to end.
//
// What is under test is the adapter's own delivery path, which is the path the
// ingress route uses (cmd/server/harnesswiring.go calls Registry.Deliver ->
// Adapter.Deliver -> `hermes chat -Q --resume <id> -q <text>`). The HTTP half of
// that joint is covered against the served route by
// cmd/server/hermes_live_delivery_test.go; this file is the half that cannot be
// faked: a REAL `hermes` binary, a REAL session in the operator's own Hermes
// store, and the delivered token found in that session's transcript on disk.
//
// The whole leg is gated on CRIER_TEST_LIVE_HARNESS=1 because it spends two
// model turns and writes into the live Hermes session store. It creates ONE
// fresh session per run (its own unique marker names it), never touches an
// existing session, and never deletes anything.
//
// Where the transcript lives on this Hermes (v0.22.5): the session store is
// SQLite, not a per-session .jsonl file. `hermes sessions export` reads the
// same rows. The evidence below therefore quotes the file path
//   <hermes home>/state.db :: messages(session_id = <id>)
// and prints the rows it found, so a reader can re-run the query.

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/harness/hermes"
)

// liveSessionIDAboard is the line `hermes chat -Q` prints after the answer.
var liveSessionIDAboard = regexp.MustCompile(`(?m)^session_id:\s*(\S+)\s*$`)

// liveGate enforces the environment gate and resolves the real binary. It skips
// (never fails) when the operator has not armed the leg or the CLI is absent.
func liveGate(t *testing.T) string {
	t.Helper()
	if os.Getenv("CRIER_TEST_LIVE_HARNESS") != "1" {
		t.Skip("set CRIER_TEST_LIVE_HARNESS=1 to run the live hermes delivery leg (needs the hermes CLI and spends real model turns)")
	}
	bin, err := exec.LookPath("hermes")
	if err != nil {
		t.Skip("hermes is not installed on this host; the adapter's argv and receipt logic is covered by the unit tests")
	}
	return bin
}

// scrubAmbientSessionEnv removes the caller's own Hermes session binding from
// the environment the delivery child inherits. A worker shell carries
// HERMES_SESSION_ID/HERMES_SESSION_KEY; leaving them set would let the child
// believe it belongs to the caller's session instead of the one it resumes, and
// the evidence would then point at the wrong transcript. Values are restored on
// cleanup, so the operator's shell is untouched.
func scrubAmbientSessionEnv(t *testing.T) {
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
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
		t.Cleanup(func() { _ = os.Setenv(key, value) })
	}
}

// hermesHome resolves the profile home the CLI writes its session store under.
func hermesHome(t *testing.T) string {
	t.Helper()
	if home := strings.TrimSpace(os.Getenv("HERMES_HOME")); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home: %v", err)
	}
	return filepath.Join(home, ".hermes")
}

// liveTranscript is the session store file this test reads its evidence from.
func liveTranscript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(hermesHome(t), "state.db")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("hermes session store %s: %v", path, err)
	}
	if info.IsDir() {
		t.Fatalf("hermes session store %s is a directory, not the SQLite transcript", path)
	}
	return path
}

// openLiveStore opens the operator's session store READ-ONLY. Nothing in this
// file can mutate a session: the handle is mode=ro.
func openLiveStore(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("open %s read-only: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping %s: %v", path, err)
	}
	return db
}

// liveRow is one transcript row: the message role and its text.
type liveRow struct {
	ID      int64
	Role    string
	Content string
}

// rowsFor returns every message the session stores, oldest first, using the
// (session_id, id) index so a 50 GB store answers in milliseconds.
func rowsFor(t *testing.T, db *sql.DB, sessionID string) []liveRow {
	t.Helper()
	rows, err := db.Query(
		`select id, role, coalesce(content,'') || ' ' || coalesce(api_content,'')
		   from messages where session_id = ? order by id`, sessionID)
	if err != nil {
		t.Fatalf("query transcript for %q: %v", sessionID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []liveRow
	for rows.Next() {
		var r liveRow
		if err := rows.Scan(&r.ID, &r.Role, &r.Content); err != nil {
			t.Fatalf("scan transcript row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate transcript for %q: %v", sessionID, err)
	}
	return out
}

// tokenRows counts the rows of sessionID whose text carries token, and returns
// the first match for the log.
func tokenRows(rows []liveRow, token string) (int, liveRow) {
	n := 0
	var first liveRow
	for _, r := range rows {
		if strings.Contains(r.Content, token) {
			if n == 0 {
				first = r
			}
			n++
		}
	}
	return n, first
}

// sessionCount is how many rows the store holds for this id. A continuation that
// silently minted a NEW session would leave this at 1 with only one of the two
// tokens, which is exactly what the assertion below refuses.
func sessionCount(t *testing.T, db *sql.DB, sessionID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`select count(*) from sessions where id = ?`, sessionID).Scan(&n); err != nil {
		t.Fatalf("count sessions for %q: %v", sessionID, err)
	}
	return n
}

// createLiveSession makes ONE fresh Hermes session for this run by asking the
// CLI a harmless question and reading back the id it prints. This is setup, not
// the delivery under test: an operator hands Crier an existing session id, and
// that is the state the deliveries below start from. The session's title is the
// marker, so a human reading `hermes sessions list` later can see where it came
// from — nothing is renamed or deleted afterwards.
func createLiveSession(t *testing.T, bin, workdir, marker string) string {
	t.Helper()
	const prompt = "This is a Crier delivery probe. Reply with exactly: SESSION-READY"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "chat", "-Q", "--in", workdir, "-q", prompt)
	cmd.Env = os.Environ()
	// The id rides on STDERR ("session_id: <id>" after the answer), stdout
	// carries only the response — so both streams are read and quoted.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("create a live hermes session: %v\nstderr:\n%s\nstdout:\n%s",
			err, strings.TrimSpace(stderr.String()), strings.TrimSpace(stdout.String()))
	}
	m := liveSessionIDAboard.FindSubmatch([]byte(stdout.String() + "\n" + stderr.String()))
	if m == nil {
		t.Fatalf("the CLI printed no session_id line, so there is no session to deliver into:\nstdout:\n%s\nstderr:\n%s",
			strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()))
	}
	id := strings.TrimSpace(string(m[1]))
	if id == "" {
		t.Fatalf("the CLI printed an empty session_id:\nstderr:\n%s", strings.TrimSpace(stderr.String()))
	}
	t.Logf("setup: created live hermes session %q (marker %s) via `hermes chat -Q --in <workdir> -q <prompt>`", id, marker)
	return id
}

// TestLiveHermesDeliveryLandsInTheRealSession is the CRIER-HARNESS-6 live leg.
//
// Phase 1 pushes a unique token through the adapter's delivered path and asserts
// the receipt is state=dispatched for the session it was given.
// Phase 2 finds that token in Hermes's OWN transcript on disk and quotes the
// path.
// Phase 3 pushes a second, different token and proves it continued the SAME
// session: same id, same transcript file, both tokens in it, and Hermes still
// holds exactly one session row for that id.
func TestLiveHermesDeliveryLandsInTheRealSession(t *testing.T) {
	bin := liveGate(t)
	scrubAmbientSessionEnv(t)

	store := liveTranscript(t)
	db := openLiveStore(t, store)

	workdir := t.TempDir()
	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	marker := "crier-live-hermes-" + stamp
	tokenOne := marker + "-one"
	tokenTwo := marker + "-two"

	sessionID := createLiveSession(t, bin, workdir, marker)
	before := sessionCount(t, db, sessionID)
	if before != 1 {
		t.Fatalf("the freshly created session %q has %d rows in the store, want 1", sessionID, before)
	}

	adapter := hermes.New(hermes.Config{
		Binary:  bin,
		Workdir: workdir,
		Timeout: 300 * time.Second,
	})
	if got := adapter.Name(); got != hermes.Name {
		t.Fatalf("adapter name = %q, want %q", got, hermes.Name)
	}

	deliver := func(t *testing.T, messageID, token string) harness.Receipt {
		t.Helper()
		start := time.Now()
		rc, err := adapter.Deliver(context.Background(),
			harness.SessionRef{Harness: hermes.Name, ID: sessionID, Workdir: workdir},
			harness.Message{
				ID:        messageID,
				SessionID: "s_crier_live_hermes",
				Author:    "crier",
				Text:      "Reply with exactly this token and nothing else: " + token,
			})
		elapsed := time.Since(start)
		t.Logf("delivery %s: receipt=%+v elapsed=%s", messageID, rc, elapsed.Round(time.Millisecond))
		if err != nil {
			t.Fatalf("deliver %s: %v", messageID, err)
		}
		if rc.State != harness.StateDispatched {
			t.Fatalf("deliver %s: state = %q (error %q, tail %q), want %q; "+
				"the CLI answered nothing or failed", messageID, rc.State, rc.Error, rc.Tail, harness.StateDispatched)
		}
		if rc.Error != "" {
			t.Fatalf("deliver %s: dispatched with error code %q", messageID, rc.Error)
		}
		if rc.ExitCode != 0 {
			t.Fatalf("deliver %s: exit code %d, want 0", messageID, rc.ExitCode)
		}
		if rc.SessionID != sessionID {
			t.Fatalf("deliver %s: receipt session id %q, want %q (a re-create would split the transcript)", messageID, rc.SessionID, sessionID)
		}
		if rc.Created {
			t.Fatalf("deliver %s: the continuation path must not claim it created a session: %+v", messageID, rc)
		}
		if rc.StdoutBytes == 0 {
			t.Fatalf("deliver %s: the CLI printed nothing, so nothing was delivered: %+v", messageID, rc)
		}
		return rc
	}

	// ── Phase 1: first push, dispatched ────────────────────────────────────
	rcOne := deliver(t, "m_live_hermes_1", tokenOne)

	// ── Phase 2: the token is in Hermes's own transcript on disk ────────────
	rows := rowsFor(t, db, sessionID)
	n, first := tokenRows(rows, tokenOne)
	if n == 0 {
		dump := make([]string, 0, len(rows))
		for _, r := range rows {
			dump = append(dump, fmt.Sprintf("  #%d %s %q", r.ID, r.Role, truncate(r.Content, 120)))
		}
		t.Fatalf("the delivered token %q is not in the hermes transcript for session %q\n"+
			"file: %s (table messages, column content)\nrows:\n%s",
			tokenOne, sessionID, store, strings.Join(dump, "\n"))
	}
	t.Logf("transcript evidence: file=%s table=messages session_id=%s row_id=%d role=%s token=%s",
		store, sessionID, first.ID, first.Role, tokenOne)

	// The same rows must also prove the session really answered: that is what
	// makes the token a DELIVERED turn rather than a stored string.
	answered := false
	for _, r := range rows {
		if r.Role == "assistant" && strings.Contains(r.Content, tokenOne) {
			answered = true
			break
		}
	}
	if !answered {
		t.Fatalf("the transcript carries the user token %q but no assistant turn quoting it; "+
			"the session did not run the message. file=%s session=%s", tokenOne, store, sessionID)
	}

	// ── Phase 3: second push continues the SAME session ─────────────────────
	rcTwo := deliver(t, "m_live_hermes_2", tokenTwo)
	if rcTwo.SessionID != rcOne.SessionID {
		t.Fatalf("second push went to session %q, first went to %q", rcTwo.SessionID, rcOne.SessionID)
	}

	after := rowsFor(t, db, sessionID)
	nOne, _ := tokenRows(after, tokenOne)
	nTwo, second := tokenRows(after, tokenTwo)
	if nOne == 0 || nTwo == 0 {
		t.Fatalf("continuation not proven in one transcript: session %q has %d rows quoting %q and %d quoting %q\n"+
			"file: %s", sessionID, nOne, tokenOne, nTwo, tokenTwo, store)
	}
	if got := sessionCount(t, db, sessionID); got != before {
		t.Fatalf("session count for %q changed %d -> %d; the second push minted a session instead of continuing this one",
			sessionID, before, got)
	}
	if len(after) <= len(rows) {
		t.Fatalf("the transcript did not grow after the second push: %d rows before, %d after (file %s)",
			len(rows), len(after), store)
	}

	t.Logf("continuation proven: one session %q, %d rows before the second push, %d after; "+
		"both tokens in %s", sessionID, len(rows), len(after), store)
	t.Logf("transport: CLI resume — `hermes chat -Q --resume %s -q <text>` (adapter continue path; "+
		"the gateway POST /v1/responses create path was NOT used)", sessionID)
	t.Logf("evidence: transcript file=%s session_id=%s token_one_row=%d token_two_row=%d",
		store, sessionID, first.ID, second.ID)
}

// truncate keeps a transcript row to a readable length in failure output.
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
