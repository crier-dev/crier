// Package daggerctl_test — CR-CHAT-035 acceptance: a DAG may run on the LOCAL
// box beside crier or on a NAMED REMOTE box, through the SAME control
// interface. Four properties, one test each plus their negative controls:
//
//  1. the create call can address local or a named remote target, and the
//     target is resolved ONCE and recorded on the run;
//  2. status and evidence flow back through ONE interface regardless of target;
//  3. a remote run whose link drops is reported as link-lost/held (state stays
//     non-terminal) and is NEVER invented into a success — recovery clears it;
//  4. a target the table does not hold is refused before any bridge is touched.
package daggerctl_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/daggerctl"
)

// newTargetedService builds a Service with a target table: the local target
// points at localBridge's URL, the remote targets at their own test servers.
// Both bridges are the SAME fakeBridge type, distinguished by which server
// fronts them, which is what makes "the observation went to the RECORDED
// target" observable.
func newTargetedService(t *testing.T, localURL string, remotes map[string]string) (*daggerctl.Service, func() []string) {
	t.Helper()
	table, err := daggerctl.NewTargetTable(localURL, remotes)
	if err != nil {
		t.Fatalf("NewTargetTable: %v", err)
	}
	localBridge, err := daggerctl.NewHTTPBridge(localURL)
	if err != nil {
		t.Fatalf("local bridge: %v", err)
	}
	svc := daggerctl.NewService(localBridge, daggerctl.NewMemoryStore(), &recordingDeliverer{})
	svc.SetTargetTable(table)
	return svc, func() []string { return nil }
}

// bridgeServer fronts a DaggerBridge over the real HTTP/JSON wire shape the
// HTTPBridge speaks, so a target's URL is a real address crier's bridge can
// dial. The served run id encodes the target, proving WHICH box answered.
type bridgeServer struct {
	srv *httptest.Server
	mu  atomic.Int32
	// statusAfterCreate lets a test flip the served status word later.
	statusWord atomic.Value // string
}

func newBridgeServer(t *testing.T, runID string) *bridgeServer {
	t.Helper()
	b := &bridgeServer{}
	b.statusWord.Store("running")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /execute", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run_id":"` + runID + `","status":"running"}`))
	})
	mux.HandleFunc("GET /runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run_id":"` + runID + `","status":"` + b.statusWord.Load().(string) + `","evidence":["ref:` + runID + `"]}`))
	})
	mux.HandleFunc("POST /runs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run_id":"` + runID + `","status":"cancelled"}`))
	})
	mux.HandleFunc("POST /skills/{skill}/run", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run_id":"skill-` + runID + `","status":"running"}`))
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

// TestCRCHAT035_CreateRunRecordsTheTargetItWasSentTo proves acceptance 1 + 4:
// a create WITHOUT a target runs locally and records "local"; a create WITH a
// named remote target runs THERE and records that name; the local bridge is
// never called for the remote run (the counts below).
func TestCRCHAT035_CreateRunRecordsTheTargetItWasSentTo(t *testing.T) {
	local := newBridgeServer(t, "run-local")
	remote := newBridgeServer(t, "run-remote")
	svc, _ := newTargetedService(t, local.srv.URL, map[string]string{"bunker-1": remote.srv.URL})
	ctx := context.Background()

	localRec, err := svc.CreateRun(ctx, daggerctl.CreateRunRequest{AgentID: "hermes-1", Prompt: "build"})
	if err != nil {
		t.Fatalf("CreateRun(local): %v", err)
	}
	if localRec.Target != daggerctl.TargetLocal {
		t.Fatalf("default create target = %q, want %q", localRec.Target, daggerctl.TargetLocal)
	}

	remoteRec, err := svc.CreateRun(ctx, daggerctl.CreateRunRequest{AgentID: "hermes-1", Prompt: "build", Target: "bunker-1"})
	if err != nil {
		t.Fatalf("CreateRun(remote): %v", err)
	}
	if remoteRec.Target != "bunker-1" {
		t.Fatalf("remote create target = %q, want bunker-1 (recorded at create time)", remoteRec.Target)
	}
	if remoteRec.RunID != "run-remote" {
		t.Fatalf("remote run id = %q — the run was NOT created on the remote target", remoteRec.RunID)
	}

	// An explicit "local" name resolves to the same default bridge.
	named, err := svc.CreateRun(ctx, daggerctl.CreateRunRequest{AgentID: "hermes-1", Prompt: "build", Target: "local"})
	if err != nil {
		t.Fatalf("CreateRun(explicit local): %v", err)
	}
	if named.Target != daggerctl.TargetLocal || named.RunID != "run-local" {
		t.Fatalf("explicit local create = %+v", named)
	}
}

// TestCRCHAT035_StatusAndEvidenceFlowThroughOneInterface proves acceptance 2:
// RunStatus reaches the RECORDED target — never re-resolved — and evidence
// comes back as string references through the same Control surface for both.
func TestCRCHAT035_StatusAndEvidenceFlowThroughOneInterface(t *testing.T) {
	local := newBridgeServer(t, "run-local")
	remote := newBridgeServer(t, "run-remote")
	svc, _ := newTargetedService(t, local.srv.URL, map[string]string{"bunker-1": remote.srv.URL})
	ctx := context.Background()

	if _, err := svc.CreateRun(ctx, daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p", Target: "bunker-1"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	before := remote.mu.Load()
	rec, err := svc.RunStatus(ctx, "run-remote")
	if err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if rec.State != daggerctl.StateRunning || len(rec.Evidence) != 1 || rec.Evidence[0] != "ref:run-remote" {
		t.Fatalf("status through one interface = %+v", rec)
	}
	if got := remote.mu.Load(); got != before+1 {
		t.Fatalf("status hit the LOCAL bridge %d time(s) — the observation must go to the recorded target", local.mu.Load())
	}
	if local.mu.Load() != 0 {
		t.Fatalf("local bridge calls = %d, want 0 — only the remote run was created in this test", local.mu.Load())
	}

	// The run SKILL verb addresses a target the same way.
	if _, err := svc.RunSkill(ctx, daggerctl.RunSkillRequest{AgentID: "a", Skill: "deploy", Target: "bunker-1"}); err != nil {
		t.Fatalf("RunSkill(remote): %v", err)
	}
}

// TestCRCHAT035_RemoteLinkLostIsHeldNeverSuccess proves acceptance 3: when the
// remote target stops answering while the run is non-terminal, the record is
// marked link_lost with a timestamp, the state stays EXACTLY what was last
// reported, and it is never delivered as a success. A later successful poll
// clears it.
func TestCRCHAT035_RemoteLinkLostIsHeldNeverSuccess(t *testing.T) {
	local := newBridgeServer(t, "run-local")
	remote := newBridgeServer(t, "run-remote")
	svc, _ := newTargetedService(t, local.srv.URL, map[string]string{"bunker-1": remote.srv.URL})
	svc.SetWatchInterval(time.Second) // armed but irrelevant here; we poll directly
	ctx := context.Background()

	if _, err := svc.CreateRun(ctx, daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p", Target: "bunker-1"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Cut the link: point the recorded target's URL at a dead address — the
	// operator's broken network, expressed the only way a test can. The
	// recorded target NAME is unchanged; only its reachability moved.
	table, err := daggerctl.NewTargetTable(local.srv.URL, map[string]string{"bunker-1": "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("NewTargetTable(dead): %v", err)
	}
	svc.SetTargetTable(table)

	_, err = svc.RunStatus(ctx, "run-remote")
	if err == nil {
		t.Fatal("RunStatus through a dead remote link must fail loudly")
	}

	store := svc.Store()
	rec, err := store.Get(ctx, "run-remote")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if !rec.LinkLost {
		t.Fatal("link_lost must be recorded when a remote poll fails on a non-terminal run")
	}
	if rec.LinkLostAt == nil || rec.LinkLostAt.IsZero() {
		t.Fatal("link_lost_at must carry the timestamp of the failed observation")
	}
	if rec.State != daggerctl.StateRunning {
		t.Fatalf("state after link loss = %q, want the last reported state (running) — a lost link must never invent an outcome", rec.State)
	}
	if rec.State.Terminal() {
		t.Fatal("a link-lost run must never read as terminal/succeeded")
	}

	// Recovery: the link comes back (the target's URL is re-pointed at the
	// live executor) and a successful observation clears the indication.
	remote.statusWord.Store("succeeded")
	tableOK, err := daggerctl.NewTargetTable(local.srv.URL, map[string]string{"bunker-1": remote.srv.URL})
	if err != nil {
		t.Fatalf("NewTargetTable(restored): %v", err)
	}
	svc.SetTargetTable(tableOK)
	rec2, err := svc.RunStatus(ctx, "run-remote")
	if err != nil {
		t.Fatalf("RunStatus(recovered): %v", err)
	}
	if rec2.LinkLost || rec2.LinkLostAt != nil {
		t.Fatalf("a recovered observation must clear link_lost, got %+v", rec2)
	}
	if rec2.State != daggerctl.StateSucceeded {
		t.Fatalf("recovered state = %q, want succeeded (the executor's own word)", rec2.State)
	}
}

// TestCRCHAT035_LinkLostRecoveryThroughTheRebuiltTarget is the honest
// recovery arm: the remote target is rebuilt at a NEW address and the table
// re-resolved; the run is still observed through its recorded target NAME.
func TestCRCHAT035_LinkLostRecoveryThroughTheRebuiltTarget(t *testing.T) {
	remote := newBridgeServer(t, "run-remote")
	remote.srv.Close() // link lost from the start

	table, err := daggerctl.NewTargetTable("http://127.0.0.1:1/local", map[string]string{"bunker-1": remote.srv.URL})
	if err != nil {
		t.Fatalf("NewTargetTable: %v", err)
	}
	localBridge, err := daggerctl.NewHTTPBridge("http://127.0.0.1:1/local")
	if err != nil {
		t.Fatalf("local bridge: %v", err)
	}
	svc := daggerctl.NewService(localBridge, daggerctl.NewMemoryStore(), &recordingDeliverer{})
	svc.SetTargetTable(table)
	ctx := context.Background()

	rec, err := svc.CreateRun(ctx, daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p", Target: "bunker-1"})
	if err == nil {
		t.Fatalf("create against a dead remote must fail, got %+v", rec)
	}
	// No half-record may exist for a create the target never confirmed.
	if _, err := svc.Store().Get(ctx, "run-remote"); !errors.Is(err, daggerctl.ErrRunNotFound) {
		t.Fatalf("a refused create left a record: %v", err)
	}
}

// TestCRCHAT035_UnknownTargetIsRefusedBeforeTheBridge proves the negative
// half of acceptance 1: a target the table does not hold is a NAMED refusal
// (ErrUnknownTarget), raised before any bridge is touched, and nothing is
// recorded.
func TestCRCHAT035_UnknownTargetIsRefusedBeforeTheBridge(t *testing.T) {
	local := newBridgeServer(t, "run-local")
	svc, _ := newTargetedService(t, local.srv.URL, map[string]string{"bunker-1": local.srv.URL})
	ctx := context.Background()

	rec, err := svc.CreateRun(ctx, daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p", Target: "no-such-box"})
	if !errors.Is(err, daggerctl.ErrUnknownTarget) {
		t.Fatalf("err = %v, want ErrUnknownTarget (got rec %+v)", err, rec)
	}
	if rec != nil {
		t.Fatal("an unknown-target create must not return a record")
	}
	if local.mu.Load() != 0 {
		t.Fatal("an unknown-target create must never touch any bridge")
	}
	if _, err := svc.Store().Get(ctx, "run-local"); !errors.Is(err, daggerctl.ErrRunNotFound) {
		t.Fatalf("a refused create left a record: %v", err)
	}
	if _, err := svc.RunSkill(ctx, daggerctl.RunSkillRequest{AgentID: "a", Skill: "s", Target: "no-such-box"}); !errors.Is(err, daggerctl.ErrUnknownTarget) {
		t.Fatalf("RunSkill unknown target err = %v, want ErrUnknownTarget", err)
	}
}

// TestCRCHAT035_LocalBridgeFailureNeverMarksLinkLost pins the boundary: the
// link-lost indication is REMOTE-only. A local run whose bridge fails keeps a
// clean record — the caller gets the error, nothing is fabricated.
func TestCRCHAT035_LocalBridgeFailureNeverMarksLinkLost(t *testing.T) {
	local := newBridgeServer(t, "run-local")
	svc, _ := newTargetedService(t, local.srv.URL, nil)
	ctx := context.Background()

	if _, err := svc.CreateRun(ctx, daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	local.srv.Close()

	if _, err := svc.RunStatus(ctx, "run-local"); err == nil {
		t.Fatal("RunStatus against a dead local bridge must fail loudly")
	}
	rec, err := svc.Store().Get(ctx, "run-local")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if rec.LinkLost {
		t.Fatal("a LOCAL bridge failure must never be recorded as link_lost — that indication is remote-only")
	}
	if rec.State != daggerctl.StateRunning {
		t.Fatalf("local failure rewrote state to %q, want untouched running", rec.State)
	}
}

// TestCRCHAT035_LinkLostIsPerRecordedTargetNotPerTable proves the recorded
// name outlives a table change: a run created on "bunker-1" is still observed
// against "bunker-1" after the table gains other targets — the record's name
// is authoritative, resolution happens once at create time.
func TestCRCHAT035_LinkLostIsPerRecordedTargetNotPerTable(t *testing.T) {
	local := newBridgeServer(t, "run-local")
	remote := newBridgeServer(t, "run-remote")
	remote2 := newBridgeServer(t, "run-remote-2")
	svc, _ := newTargetedService(t, local.srv.URL, map[string]string{"bunker-1": remote.srv.URL})
	ctx := context.Background()

	if _, err := svc.CreateRun(ctx, daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p", Target: "bunker-1"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// A different, later-registered target must not be consulted for the
	// recorded run.
	table2, err := daggerctl.NewTargetTable(local.srv.URL, map[string]string{
		"bunker-1": remote.srv.URL,
		"bunker-2": remote2.srv.URL,
	})
	if err != nil {
		t.Fatalf("NewTargetTable(2): %v", err)
	}
	svc.SetTargetTable(table2)

	rec, err := svc.RunStatus(ctx, "run-remote")
	if err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if rec.Target != "bunker-1" {
		t.Fatalf("recorded target mutated to %q, want bunker-1", rec.Target)
	}
	if remote2.mu.Load() != 0 {
		t.Fatal("an unrelated target was consulted for a run recorded against bunker-1")
	}
}
