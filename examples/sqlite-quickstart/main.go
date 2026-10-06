// Command sqlite-quickstart is the CR-CHAT-006 "no PostgreSQL service" demo:
// it runs a chat session on the JSONL ordered append log plus a SQLite query
// view, restarts from disk, and reconciles the two.
//
// Run it with:
//
//	go run ./examples/sqlite-quickstart
//
// Nothing is installed beyond the Go toolchain: the SQLite driver is pure Go
// (modernc.org/sqlite), so there is no CGO, no database server and no
// PostgreSQL URL anywhere in this program.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/crier-dev/crier/internal/session"
)

// timeAt returns a stable whole-second UTC instant, so the demo's transcript
// is deterministic run to run.
func timeAt(sec int) time.Time {
	return time.Date(2026, 10, 4, 5, 0, 0, 0, time.UTC).Add(time.Duration(sec) * time.Second)
}

const sessionID = "quickstart-session"

func main() {
	if err := run(); err != nil {
		log.Fatalf("sqlite-quickstart: %v", err)
	}
}

func run() error {
	ctx := context.Background()

	root, err := os.MkdirTemp("", "crier-sqlite-quickstart-")
	if err != nil {
		return err
	}
	fmt.Printf("scratch dir: %s\n", root)

	logRoot := filepath.Join(root, "log")
	dbPath := filepath.Join(root, "sessions.sqlite")

	// The two halves of the dual backend: the JSONL ordered append log (the
	// allocator and the transport form) and the SQLite query view.
	log1, err := session.NewJSONLStore(logRoot)
	if err != nil {
		return err
	}
	view1, err := session.OpenStore(ctx, session.BackendSQLite, session.StoreOptions{SQLitePath: dbPath})
	if err != nil {
		return err
	}
	defer func() { _ = view1.Close() }()

	// A writer that appends to the log and projects the SAME record into the
	// view — the one allowed write direction (record -> log line -> view).
	write := func(make func(seq int64) *session.Record) error {
		seq, err := log1.NextSeq(ctx, sessionID)
		if err != nil {
			return err
		}
		rec := make(seq)
		if err := log1.Append(ctx, rec); err != nil {
			return err
		}
		_, err = session.ProjectLog(ctx, view1, []*session.Record{rec})
		return err
	}

	sess := &session.Session{
		ID:        sessionID,
		Kind:      session.KindChannel,
		Title:     "Quickstart",
		CreatedBy: session.AuthorRef{Agent: "atlas"},
	}
	if err := write(func(seq int64) *session.Record {
		return sess.CreateRecord(seq, timeAt(0))
	}); err != nil {
		return err
	}
	fmt.Println("created session", sessionID)

	member := &session.Member{
		SessionID: sessionID, MemberType: session.MemberAgent,
		MemberID: "atlas", Role: session.RoleOwner, AddedBy: "atlas",
	}
	if err := write(func(seq int64) *session.Record {
		return member.AddRecord(seq, timeAt(1), nil)
	}); err != nil {
		return err
	}

	first := &session.Message{
		ID: "m1", SessionID: sessionID, ThreadID: "m1", Kind: session.MessagePlain,
		Author:   session.AuthorRef{Agent: "atlas"},
		Payload:  json.RawMessage(`{"text":"kick off the data sweep"}`),
		Audience: session.Audience{Rule: session.AudienceSession, Targets: []session.AudienceTarget{{Kind: session.TargetAgent, ID: "atlas"}}},
	}
	if err := write(func(seq int64) *session.Record {
		first.Seq, first.CreatedAt = seq, timeAt(2)
		return first.Record()
	}); err != nil {
		return err
	}
	fmt.Println("posted root message", first.ID)

	reply := &session.Message{
		ID: "m2", SessionID: sessionID, ThreadID: "m1", ParentID: "m1",
		Kind:     session.MessagePlain,
		Author:   session.AuthorRef{Agent: "atlas"},
		Payload:  json.RawMessage(`{"text":"on it"}`),
		Audience: session.Audience{Rule: session.AudienceReplyDefault, Targets: []session.AudienceTarget{{Kind: session.TargetAgent, ID: "atlas"}}},
	}
	if err := write(func(seq int64) *session.Record {
		reply.Seq, reply.CreatedAt = seq, timeAt(3)
		return reply.Record()
	}); err != nil {
		return err
	}
	fmt.Println("posted reply", reply.ID, "in thread", reply.ThreadID)

	// A deliberate branch — the ONLY record that creates a thread.
	branch := &session.Thread{
		ID: "thr1", SessionID: sessionID, ParentThreadID: "m1",
		RootMessageID: "thr1", AnchorMessageID: "m1",
		CreatedBy: session.AuthorRef{Agent: "atlas"},
	}
	if err := write(func(seq int64) *session.Record {
		branch.CreatedAt = timeAt(4)
		return branch.BranchRecord(seq, timeAt(4), "split the schema work out")
	}); err != nil {
		return err
	}
	fmt.Println("branched thread", branch.ID, "from", branch.ParentThreadID)

	if err := show(ctx, view1, "before restart"); err != nil {
		return err
	}

	// Reconcile: the log and the view must reduce to the same State, and a
	// mismatch would be REPORTED rather than healed.
	logRecs, err := log1.Records(ctx, sessionID)
	if err != nil {
		return err
	}
	if _, err := session.Reconcile(ctx, view1, sessionID, logRecs); err != nil {
		return err
	}
	fmt.Println("reconcile: the log and the SQLite view agree")

	// Restart: close both, reopen the same paths, re-project the log.
	if err := view1.Close(); err != nil {
		return err
	}
	if err := log1.Close(); err != nil {
		return err
	}
	log2, err := session.NewJSONLStore(logRoot)
	if err != nil {
		return err
	}
	defer func() { _ = log2.Close() }()
	view2, err := session.OpenStore(ctx, session.BackendSQLite, session.StoreOptions{SQLitePath: dbPath})
	if err != nil {
		return err
	}
	defer func() { _ = view2.Close() }()

	logRecs, err = log2.Records(ctx, sessionID)
	if err != nil {
		return err
	}
	if _, err := session.ProjectLog(ctx, view2, logRecs); err != nil {
		return err
	}
	return show(ctx, view2, "after restart (same paths, fresh process state)")
}

// show prints the reduced session State, which is what every read path serves.
func show(ctx context.Context, view session.Store, label string) error {
	st, err := view.Load(ctx, sessionID)
	if err != nil {
		return err
	}
	fmt.Printf("views %q: %d member(s), %d message(s), %d thread(s), branch depth %d\n",
		label, len(st.Members), len(st.Messages), len(st.Threads), st.ThreadDepth("thr1"))
	for _, m := range st.Messages {
		fmt.Printf("  seq %d  %s  thread=%s parent=%s\n", m.Seq, m.ID, m.ThreadID, m.ParentID)
	}
	return nil
}
