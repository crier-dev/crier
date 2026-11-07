package opencode_test

// CRIER-HARNESS-7 — delivery proof into a REAL opencode session.
//
// WHAT THIS PROVES. Two pushes carrying unique marker tokens go through the
// adapter's ordinary delivery path (the argv it builds, the JSON it parses, the
// receipt it returns) and are then looked up in opencode's OWN session storage:
//
//  1. create    -> `opencode run --title <t> --format json <text>`
//     receipt state=dispatched, created=true, session id minted and parsed
//     back out of opencode's JSON stream.
//  2. continue  -> `opencode run -s <id> --format json <text>`
//     receipt state=dispatched, created=false, SAME session id: the second
//     token lands in the SAME stored session, and no second session row with
//     this run's title exists.
//
// EQUIVALENCE TO POST /ingress/messages. The ingress route registers
// ingressHandler at cmd/server/main.go:974; its Dispatch is
// (*harnessDispatch).Dispatch at cmd/server/harnesswiring.go:60, which loads
// the Crier session, copies HarnessSessionID into harness.SessionRef.ID and the
// message fields into harness.Message, then calls reg.Deliver ->
// (*opencode.Adapter).Deliver (internal/harness/opencode/opencode.go:75). This
// test enters at exactly that adapter call with the same SessionRef/Message
// shapes, so the HTTP layer is the only thing not exercised here; the delivery
// surface, the state vocabulary and the receipt are the same code. (A server
// attach configuration reports state "accepted" — adapters.opencode.server_url
// set — because the server owns the turn; the default CLI path asserted here is
// "dispatched", per CRIER-HARNESS §5.)
//
// MEASURED STORAGE LAYOUT — the brief's assumed shape does not exist on this
// host. Probed before asserting anything (2026-11-06):
//
//	$ ls ~/.local/share/opencode/project/     -> no such directory
//	$ ls ~/.local/share/opencode/storage/     -> migration, session_diff only
//
// opencode 1.18.29 — the version measured here — keeps a single SQLite database:
//
//	~/.local/share/opencode/opencode.db
//
// The session row lives in table `session` (id, title, directory, version) and
// every transcript part, including the delivered USER text, in table `part`,
// column `data` (JSON: {"type":"text","text":"..."}), keyed by `session_id`.
// The database is read read-only (`?mode=ro`); nothing here mutates, deletes or
// rewrites another session, and the delivery itself is opencode's own write.
//
// VERSION PIN (AC 4). Measured and logged at run time via `opencode --version`
// and cross-checked against the `version` column opencode writes on the session
// row it created: 1.18.29 on this host. A future v2 CLI with a different shape
// is a separate row — this test pins the shape above, not a promise about it.
//
// ch:trace row=CRIER-HARNESS-7 spec=specs/CRIER-HARNESS.md#opencode-id-policy
// test=internal/harness/opencode/live_delivery_test.go::TestLiveOpencodeDelivery
// doc=specs/CRIER-HARNESS.md evidence=CRIER_TEST_LIVE_HARNESS=1 go test -count=1
// -timeout 600s -run 'LiveOpencode' ./internal/harness/opencode/ (local run log in the closeout
// report) witness=none:local-worktree-no-published-endpoint
// verdict=none:foreman-completes-after-merge commit=wt/CRIER-HARNESS-7
// memory=none:foreman-closeout

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/harness/opencode"
)

// liveOpencodeVersion is what `opencode --version` measured on the host this
// proof was written on. It is recorded, not asserted as a floor: the test
// re-measures at run time and cross-checks the version opencode stamps on the
// session row it creates.
const liveOpencodeVersion = "1.18.29"

// liveDeliveryTimeout bounds ONE delivery. opencode runs a real model turn, so
// this is generous by design; the process itself is capped by the same value.
const liveDeliveryTimeout = 300 * time.Second

// TestLiveOpencodeDelivery is the CRIER-HARNESS-7 live leg. It skips unless
// CRIER_TEST_LIVE_HARNESS=1 is set (the shape used by the codex live leg and the
// pi joint) and under -short, because it spends a real model turn.
func TestLiveOpencodeDelivery(t *testing.T) {
	if os.Getenv("CRIER_TEST_LIVE_HARNESS") != "1" {
		t.Skip("set CRIER_TEST_LIVE_HARNESS=1 to run the opencode delivery leg (needs the opencode CLI and a configured provider)")
	}
	if testing.Short() {
		t.Skip("live delivery spends a real model turn; -short keeps it out of the ordinary suite")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode is not installed on this host")
	}
	version := measuredOpencodeVersion(t, bin)
	dbPath := opencodeStorePath(t)
	t.Logf("opencode version=%s (recorded %s) storage=%s", version, liveOpencodeVersion, dbPath)

	// A unique run id keeps every token and the session title unambiguous, so
	// the storage query cannot match a session from an earlier run.
	runID := fmt.Sprintf("%d", time.Now().UnixNano())
	tokenCreate := "crier-live-opencode-" + runID + "-create"
	tokenContinue := "crier-live-opencode-" + runID + "-continue"
	crierSession := "s_live_opencode_" + runID
	// title is what the adapter puts on the create path (opencode.go:190):
	// "Crier " + msg.SessionID. A second session row with this title would mean
	// the continuation created a fresh session instead of continuing.
	title := "Crier " + crierSession

	workdir := t.TempDir()
	adapter := opencode.New(opencode.Config{
		Binary:  bin,
		Workdir: workdir,
		Timeout: liveDeliveryTimeout,
	})

	// --- 1. create -------------------------------------------------------
	ctx, cancel := context.WithTimeout(context.Background(), liveDeliveryTimeout)
	defer cancel()
	start := time.Now()
	rc, err := adapter.Deliver(ctx, harness.SessionRef{},
		harness.Message{
			ID:        "m_live_opencode_1",
			SessionID: crierSession,
			Text:      "Remember this token for later: " + tokenCreate + ". Reply with exactly: ACK",
		})
	elapsed := time.Since(start)
	t.Logf("create receipt=%+v elapsed=%s", rc, elapsed.Round(time.Millisecond))
	if err != nil {
		t.Fatalf("create delivery returned an error: %v", err)
	}
	if rc.State != harness.StateDispatched {
		t.Fatalf("create state = %q (error %q, tail %q), want %q: the default CLI path dispatches, only a server attach accepts",
			rc.State, rc.Error, rc.Tail, harness.StateDispatched)
	}
	if rc.Error != "" {
		t.Fatalf("a dispatched create must carry no error code, got %q (tail %q)", rc.Error, rc.Tail)
	}
	if !rc.Created {
		t.Fatalf("a delivery with no session id is a create: %+v", rc)
	}
	if !strings.HasPrefix(rc.SessionID, "ses_") {
		t.Fatalf("the create receipt must carry the opencode session id it minted, got %q", rc.SessionID)
	}
	sessionID := rc.SessionID

	// The delivered text is in opencode's own storage, under this session.
	// The control first: a token that was NEVER delivered must match nothing,
	// so a broken/lax query cannot make the arm below pass on any row.
	if absent := tokenSessions(t, dbPath, tokenCreate+"-never-delivered"); len(absent) != 0 {
		t.Fatalf("control token matched %d stored sessions (%v): the storage query does not discriminate, so the delivery assertion below would be vacuous", len(absent), keys(absent))
	}
	hits := tokenSessions(t, dbPath, tokenCreate)
	if len(hits) != 1 {
		t.Fatalf("token %q found in %d opencode sessions (%v), want exactly the one just created (%s); storage=%s",
			tokenCreate, len(hits), hits, sessionID, dbPath)
	}
	if _, ok := hits[sessionID]; !ok {
		t.Fatalf("token %q is stored under %v, not the session the receipt reported (%s); storage=%s",
			tokenCreate, keys(hits), sessionID, dbPath)
	}
	if stored := sessionRowVersion(t, dbPath, sessionID); stored != version {
		t.Fatalf("opencode stamped version %q on session %s but `opencode --version` measured %q", stored, sessionID, version)
	}
	t.Logf("create proven: token %s stored in %s table `part` (session %s)", tokenCreate, dbPath, sessionID)

	// --- 2. continue the SAME session ------------------------------------
	ctx2, cancel2 := context.WithTimeout(context.Background(), liveDeliveryTimeout)
	defer cancel2()
	start = time.Now()
	rc2, err := adapter.Deliver(ctx2, harness.SessionRef{ID: sessionID},
		harness.Message{
			ID:        "m_live_opencode_2",
			SessionID: crierSession,
			Text:      "Second turn. Now remember this token too: " + tokenContinue + ". Reply with exactly: ACK",
		})
	elapsed = time.Since(start)
	t.Logf("continue receipt=%+v elapsed=%s", rc2, elapsed.Round(time.Millisecond))
	if err != nil {
		t.Fatalf("continue delivery returned an error: %v", err)
	}
	if rc2.State != harness.StateDispatched || rc2.Error != "" {
		t.Fatalf("continue state = %q (error %q, tail %q), want %q with no error",
			rc2.State, rc2.Error, rc2.Tail, harness.StateDispatched)
	}
	if rc2.Created {
		t.Fatalf("a delivery with a session id must continue that session, not create one: %+v", rc2)
	}
	if rc2.SessionID != sessionID {
		t.Fatalf("continue reported session %q, want the created session %q (a silent re-create splits the transcript)", rc2.SessionID, sessionID)
	}

	// The second token went into the SAME stored session — not a fresh one.
	hits2 := tokenSessions(t, dbPath, tokenContinue)
	if len(hits2) != 1 {
		t.Fatalf("continue token %q found in %d opencode sessions (%v), want exactly %s; storage=%s",
			tokenContinue, len(hits2), hits2, sessionID, dbPath)
	}
	if _, ok := hits2[sessionID]; !ok {
		t.Fatalf("continue token %q is stored under %v, not the continued session %s; storage=%s",
			tokenContinue, keys(hits2), sessionID, dbPath)
	}
	if n := sessionRowsWithTitle(t, dbPath, title); n != 1 {
		t.Fatalf("title %q has %d session rows after the continuation, want 1: a new row means `run -s` created a fresh session", title, n)
	}
	// Both turns are in the one session, so the transcript is a continuation
	// rather than two single-turn sessions.
	first := tokenSessions(t, dbPath, tokenCreate)
	if _, ok := first[sessionID]; !ok {
		t.Fatalf("the first token left session %s after the continuation; transcripts %v", sessionID, keys(first))
	}
	t.Logf("continuation proven: session %s holds both %s and %s; storage=%s", sessionID, tokenCreate, tokenContinue, dbPath)
	t.Logf("CRIER-HARNESS-7 evidence: opencode %s, storage %s, session %s", version, dbPath, sessionID)
}

// opencodeStorePath returns the measured opencode storage database. It fails
// with both the path it looked for and the layout the brief assumed, so a host
// whose opencode moved its storage says so plainly instead of "no rows".
func opencodeStorePath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory to resolve opencode storage: %v", err)
	}
	dbPath := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("opencode storage database not found at %s (%v); the JSON tree this brief assumed (~/.local/share/opencode/project/<hash>/storage/session/) does not exist on opencode %s either — re-probe the layout before trusting this test",
			dbPath, err, liveOpencodeVersion)
	}
	return dbPath
}

// measuredOpencodeVersion runs `opencode --version` once and returns the version
// token, which is what the adapter's Probe would report.
func measuredOpencodeVersion(t *testing.T, bin string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		t.Fatalf("opencode --version: %v", err)
	}
	line := strings.TrimSpace(string(out))
	fields := strings.Fields(line)
	if len(fields) == 0 {
		t.Fatalf("opencode --version printed nothing usable: %q", line)
	}
	return strings.TrimPrefix(fields[0], "v")
}

// openStore opens the opencode database READ-ONLY. A read on a WAL database is
// served from the -wal file by SQLite, so the newest turn is visible; mode=ro
// guarantees this test cannot write to, checkpoint or migrate it.
func openStore(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("open opencode storage %s read-only: %v", dbPath, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// tokenSessions returns session id -> number of stored parts containing token.
// A non-empty result for exactly one session is the proof of delivery.
func tokenSessions(t *testing.T, dbPath, token string) map[string]int {
	t.Helper()
	db := openStore(t, dbPath)
	rows, err := db.Query(`SELECT session_id, COUNT(*) FROM part WHERE data LIKE ? GROUP BY session_id`, "%"+token+"%")
	if err != nil {
		t.Fatalf("query opencode storage %s table `part`: %v", dbPath, err)
	}
	defer func() { _ = rows.Close() }()
	hits := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatalf("scan opencode storage row: %v", err)
		}
		hits[id] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate opencode storage %s: %v", dbPath, err)
	}
	return hits
}

// sessionRowVersion returns the version opencode stamped on the session row —
// the CLI version that wrote it.
func sessionRowVersion(t *testing.T, dbPath, sessionID string) string {
	t.Helper()
	db := openStore(t, dbPath)
	var version string
	if err := db.QueryRow(`SELECT version FROM session WHERE id = ?`, sessionID).Scan(&version); err != nil {
		t.Fatalf("read session %s from opencode storage %s: %v", sessionID, dbPath, err)
	}
	return version
}

// sessionRowsWithTitle counts opencode session rows carrying title. A create
// path sets the title; a continuation with `-s` must not mint a second one.
func sessionRowsWithTitle(t *testing.T, dbPath, title string) int {
	t.Helper()
	db := openStore(t, dbPath)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session WHERE title = ?`, title).Scan(&n); err != nil {
		t.Fatalf("count sessions titled %q in opencode storage %s: %v", title, dbPath, err)
	}
	return n
}

func keys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
