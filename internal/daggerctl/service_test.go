// Package daggerctl_test is the EXTERNAL test package for internal/daggerctl.
//
// It drives the control surface through its public API only, with the bridge
// faked at the DaggerBridge interface, and proves the four properties
// CR-CHAT-033's acceptance names: a create returns a run id, a status poll
// reports what the BRIDGE said, cancel transitions state, resume/rewind reach
// the bridge, and a terminal run is delivered to the requesting agent through
// the SHIPPED inbox path — while nothing in crier executes a DAG.
package daggerctl_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/daggerctl"
	"github.com/crier-dev/crier/internal/registry"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeBridge is a DaggerBridge that records every call and answers from
// per-verb canned views. It is the ONLY thing the Service can talk to, which is
// what makes "crier embeds no executor" observable: when the fake refuses,
// nothing else in crier can produce a run.
type fakeBridge struct {
	mu     sync.Mutex
	calls  []string
	create *daggerctl.RunView
	status func(runID string) (*daggerctl.RunView, error)
	cancel *daggerctl.RunView
	resume *daggerctl.RunView
	rewind *daggerctl.RunView
	skill  *daggerctl.RunView

	createErr error
	cancelErr error
	resumeErr error
	rewindErr error
	skillErr  error
	statusErr error
}

func (b *fakeBridge) record(call string) {
	b.mu.Lock()
	b.calls = append(b.calls, call)
	b.mu.Unlock()
}

func (b *fakeBridge) Calls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

func (b *fakeBridge) CreateRun(_ context.Context, req daggerctl.CreateRunRequest) (*daggerctl.RunView, error) {
	b.record("create")
	if b.createErr != nil {
		return nil, b.createErr
	}
	if b.create != nil {
		return b.create, nil
	}
	return &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateRunning}, nil
}

func (b *fakeBridge) RunStatus(_ context.Context, runID string) (*daggerctl.RunView, error) {
	b.record("status:" + runID)
	if b.statusErr != nil {
		return nil, b.statusErr
	}
	if b.status != nil {
		return b.status(runID)
	}
	return &daggerctl.RunView{RunID: runID, State: daggerctl.StateRunning}, nil
}

func (b *fakeBridge) Cancel(_ context.Context, runID string) (*daggerctl.RunView, error) {
	b.record("cancel:" + runID)
	if b.cancelErr != nil {
		return nil, b.cancelErr
	}
	if b.cancel != nil {
		return b.cancel, nil
	}
	return &daggerctl.RunView{RunID: runID, State: daggerctl.StateCancelled}, nil
}

func (b *fakeBridge) Resume(_ context.Context, runID string) (*daggerctl.RunView, error) {
	b.record("resume:" + runID)
	if b.resumeErr != nil {
		return nil, b.resumeErr
	}
	if b.resume != nil {
		return b.resume, nil
	}
	return &daggerctl.RunView{RunID: runID, State: daggerctl.StateRunning}, nil
}

func (b *fakeBridge) Rewind(_ context.Context, runID, nodeID string) (*daggerctl.RunView, error) {
	b.record("rewind:" + runID + ":" + nodeID)
	if b.rewindErr != nil {
		return nil, b.rewindErr
	}
	if b.rewind != nil {
		return b.rewind, nil
	}
	return &daggerctl.RunView{RunID: runID, State: daggerctl.StateRunning}, nil
}

func (b *fakeBridge) RunSkill(_ context.Context, req daggerctl.RunSkillRequest) (*daggerctl.RunView, error) {
	b.record("skill:" + req.Skill)
	if b.skillErr != nil {
		return nil, b.skillErr
	}
	if b.skill != nil {
		return b.skill, nil
	}
	return &daggerctl.RunView{RunID: "skill-run-1", State: daggerctl.StateRunning}, nil
}

// recordingDeliverer captures every inbox delivery.
type recordingDeliverer struct {
	mu      sync.Mutex
	payload [][]byte
	agents  []string
	err     error
}

func (d *recordingDeliverer) DeliverToInbox(agentID string, payload []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	d.agents = append(d.agents, agentID)
	d.payload = append(d.payload, append([]byte(nil), payload...))
	return nil
}

func (d *recordingDeliverer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.payload)
}

func (d *recordingDeliverer) last(t *testing.T) map[string]any {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.payload) == 0 {
		t.Fatal("no inbox delivery was made")
	}
	var out map[string]any
	if err := json.Unmarshal(d.payload[len(d.payload)-1], &out); err != nil {
		t.Fatalf("decode delivered payload: %v", err)
	}
	return out
}

func newService(t *testing.T, b daggerctl.DaggerBridge, d daggerctl.Deliverer) *daggerctl.Service {
	t.Helper()
	svc := daggerctl.NewService(b, daggerctl.NewMemoryStore(), d)
	if svc == nil {
		t.Fatal("NewService returned nil")
	}
	return svc
}

// ---------------------------------------------------------------------------
// acceptance: create -> id, status -> bridge status, cancel -> transition
// ---------------------------------------------------------------------------

func TestCreateRunReturnsTheRunIDAndStoresTheRequestingAgent(t *testing.T) {
	bridge := &fakeBridge{create: &daggerctl.RunView{RunID: "run-abc", State: daggerctl.StateRunning}}
	deliver := &recordingDeliverer{}
	svc := newService(t, bridge, deliver)

	rec, err := svc.CreateRun(context.Background(), daggerctl.CreateRunRequest{AgentID: "hermes-1", Prompt: "build a thing"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if rec.RunID != "run-abc" {
		t.Fatalf("run id = %q, want run-abc", rec.RunID)
	}
	if rec.RequestingAgent != "hermes-1" {
		t.Errorf("requesting agent = %q, want hermes-1", rec.RequestingAgent)
	}
	if rec.State != daggerctl.StateRunning {
		t.Errorf("state = %q, want running", rec.State)
	}
	if rec.Kind != daggerctl.KindPrompt || rec.Prompt != "build a thing" {
		t.Errorf("kind/prompt = %q/%q", rec.Kind, rec.Prompt)
	}
	if deliver.count() != 0 {
		t.Errorf("a non-terminal create delivered %d message(s), want 0", deliver.count())
	}

	// The record is readable back.
	got, err := svc.RunStatus(context.Background(), "run-abc")
	if err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if got.RunID != "run-abc" || got.RequestingAgent != "hermes-1" {
		t.Errorf("stored record = %+v", got)
	}
}

func TestRunStatusReportsTheBridgeStatus(t *testing.T) {
	statuses := []daggerctl.RunState{daggerctl.StateRunning, daggerctl.StateRunning}
	i := 0
	bridge := &fakeBridge{
		create: &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateRunning},
		status: func(runID string) (*daggerctl.RunView, error) {
			st := statuses[min(i, len(statuses)-1)]
			i++
			return &daggerctl.RunView{RunID: runID, State: st, Evidence: []string{"ckpt/1"}}, nil
		},
	}
	svc := newService(t, bridge, &recordingDeliverer{})

	if _, err := svc.CreateRun(context.Background(), daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	rec, err := svc.RunStatus(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if rec.State != daggerctl.StateRunning {
		t.Fatalf("state = %q, want running (the bridge's own word)", rec.State)
	}
	if len(rec.Evidence) != 1 || rec.Evidence[0] != "ckpt/1" {
		t.Errorf("evidence = %v, want [ckpt/1]", rec.Evidence)
	}
}

func TestCancelTransitionsStateAndReachesTheBridge(t *testing.T) {
	bridge := &fakeBridge{create: &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateRunning}}
	svc := newService(t, bridge, &recordingDeliverer{})

	if _, err := svc.CreateRun(context.Background(), daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	rec, err := svc.Cancel(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if rec.State != daggerctl.StateCancelled {
		t.Fatalf("state after cancel = %q, want cancelled", rec.State)
	}
	if got := bridge.Calls(); !contains(got, "cancel:run-1") {
		t.Errorf("bridge calls = %v, want a cancel call", got)
	}
}

// TestCancelNamesTheStateWhenTheBridgeReportsNone pins the fallback: cancelling
// a run IS the vocabulary's cancelled state, so a bridge that answers without a
// status word must not leave the record running forever.
func TestCancelNamesTheStateWhenTheBridgeReportsNone(t *testing.T) {
	bridge := &fakeBridge{
		create: &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateRunning},
		cancel: &daggerctl.RunView{RunID: "run-1"}, // no status word
	}
	svc := newService(t, bridge, &recordingDeliverer{})

	if _, err := svc.CreateRun(context.Background(), daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	rec, err := svc.Cancel(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if rec.State != daggerctl.StateCancelled {
		t.Fatalf("state = %q, want cancelled", rec.State)
	}
}

func TestResumeAndRewindReachTheBridge(t *testing.T) {
	bridge := &fakeBridge{create: &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateRunning}}
	svc := newService(t, bridge, &recordingDeliverer{})

	if _, err := svc.CreateRun(context.Background(), daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := svc.Resume(context.Background(), "run-1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if _, err := svc.Rewind(context.Background(), "run-1", "node-7"); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	calls := bridge.Calls()
	if !contains(calls, "resume:run-1") {
		t.Errorf("bridge calls = %v, want a resume call", calls)
	}
	if !contains(calls, "rewind:run-1:node-7") {
		t.Errorf("bridge calls = %v, want a rewind call carrying the node id verbatim", calls)
	}
}

func TestRewindRequiresANode(t *testing.T) {
	svc := newService(t, &fakeBridge{}, &recordingDeliverer{})
	_, err := svc.Rewind(context.Background(), "run-1", "  ")
	if !errors.Is(err, daggerctl.ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
}

func TestRunSkillReachesTheBridge(t *testing.T) {
	bridge := &fakeBridge{skill: &daggerctl.RunView{RunID: "skill-run-9", State: daggerctl.StateRunning}}
	svc := newService(t, bridge, &recordingDeliverer{})

	rec, err := svc.RunSkill(context.Background(), daggerctl.RunSkillRequest{
		AgentID: "hermes-1", Skill: "summarize", Args: map[string]any{"lang": "es"},
	})
	if err != nil {
		t.Fatalf("RunSkill: %v", err)
	}
	if rec.RunID != "skill-run-9" || rec.Kind != daggerctl.KindSkill || rec.Skill != "summarize" {
		t.Fatalf("record = %+v", rec)
	}
	if got := bridge.Calls(); !contains(got, "skill:summarize") {
		t.Errorf("bridge calls = %v, want a skill call", got)
	}
}

func TestCreateRunValidatesInput(t *testing.T) {
	svc := newService(t, &fakeBridge{}, &recordingDeliverer{})
	cases := []struct {
		name string
		req  daggerctl.CreateRunRequest
	}{
		{"no agent", daggerctl.CreateRunRequest{Prompt: "p"}},
		{"no prompt", daggerctl.CreateRunRequest{AgentID: "a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.CreateRun(context.Background(), tc.req); !errors.Is(err, daggerctl.ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// acceptance: terminal delivery through the SHIPPED inbox path
// ---------------------------------------------------------------------------

// TestTerminalRunIsDeliveredToTheRequestingAgentsInbox is the acceptance test
// for "completion or failure is delivered to the requesting agent through the
// ordinary inbox path": the deliverer is the PRODUCTION adapter over a real
// registry store, and the assertion reads the message back out of that agent's
// durable inbox with the store's own Retrieve.
func TestTerminalRunIsDeliveredToTheRequestingAgentsInbox(t *testing.T) {
	store := registry.NewMemoryStore()
	if err := store.Register(&registry.Agent{ID: "hermes-1"}); err != nil {
		t.Fatalf("register agent: %v", err)
	}

	bridge := &fakeBridge{create: &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateRunning}}
	svc := daggerctl.NewService(bridge, daggerctl.NewMemoryStore(), daggerctl.InboxDeliverer(store))

	if _, err := svc.CreateRun(context.Background(), daggerctl.CreateRunRequest{AgentID: "hermes-1", Prompt: "deploy"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// The DAG finishes on its own; the next status observation is where crier
	// learns it and delivers.
	bridge.status = func(runID string) (*daggerctl.RunView, error) {
		return &daggerctl.RunView{RunID: runID, State: daggerctl.StateSucceeded, Evidence: []string{"evidence/run-1"}}, nil
	}
	rec, err := svc.RunStatus(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if rec.State != daggerctl.StateSucceeded || !rec.Notified {
		t.Fatalf("record = %+v, want succeeded + notified", rec)
	}

	entries, _, err := store.Retrieve("hermes-1", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("retrieve inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("inbox holds %d message(s), want exactly 1", len(entries))
	}
	var note daggerctl.RunNotification
	if err := json.Unmarshal(entries[0].Payload, &note); err != nil {
		t.Fatalf("decode notification: %v", err)
	}
	if note.Kind != "dagger_run" || note.Code != daggerctl.CodeRunSucceeded {
		t.Errorf("notification kind/code = %q/%q", note.Kind, note.Code)
	}
	if note.RunID != "run-1" || note.State != daggerctl.StateSucceeded {
		t.Errorf("notification run/state = %q/%q", note.RunID, note.State)
	}
	if note.RequestingAgent != "hermes-1" {
		t.Errorf("notification requesting_agent = %q", note.RequestingAgent)
	}
	if len(note.Evidence) != 1 || note.Evidence[0] != "evidence/run-1" {
		t.Errorf("notification evidence = %v", note.Evidence)
	}
}

func TestTerminalDeliveryIsExactlyOnceAcrossRepeatedPolls(t *testing.T) {
	bridge := &fakeBridge{create: &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateRunning}}
	deliver := &recordingDeliverer{}
	svc := newService(t, bridge, deliver)

	if _, err := svc.CreateRun(context.Background(), daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	bridge.status = func(runID string) (*daggerctl.RunView, error) {
		return &daggerctl.RunView{RunID: runID, State: daggerctl.StateFailed}, nil
	}
	for i := 0; i < 3; i++ {
		if _, err := svc.RunStatus(context.Background(), "run-1"); err != nil {
			t.Fatalf("RunStatus #%d: %v", i, err)
		}
	}
	if got := deliver.count(); got != 1 {
		t.Fatalf("deliveries = %d, want exactly 1 across repeated polls", got)
	}
	if code, _ := deliver.last(t)["code"].(string); code != daggerctl.CodeRunFailed {
		t.Errorf("code = %q, want %q", code, daggerctl.CodeRunFailed)
	}
}

func TestCancelOutcomeIsDeliveredWithItsOwnCode(t *testing.T) {
	bridge := &fakeBridge{create: &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateRunning}}
	deliver := &recordingDeliverer{}
	svc := newService(t, bridge, deliver)

	if _, err := svc.CreateRun(context.Background(), daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := svc.Cancel(context.Background(), "run-1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := deliver.count(); got != 1 {
		t.Fatalf("deliveries = %d, want 1", got)
	}
	if code, _ := deliver.last(t)["code"].(string); code != daggerctl.CodeRunCancelled {
		t.Errorf("code = %q, want %q", code, daggerctl.CodeRunCancelled)
	}
}

// TestNonTerminalRunIsNeverDelivered proves crier does not invent an outcome:
// while the bridge says the run is live, nothing is delivered.
func TestNonTerminalRunIsNeverDelivered(t *testing.T) {
	bridge := &fakeBridge{create: &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateRunning}}
	deliver := &recordingDeliverer{}
	svc := newService(t, bridge, deliver)

	if _, err := svc.CreateRun(context.Background(), daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := svc.RunStatus(context.Background(), "run-1"); err != nil {
			t.Fatalf("RunStatus: %v", err)
		}
	}
	if got := deliver.count(); got != 0 {
		t.Fatalf("a live run delivered %d message(s), want 0", got)
	}
}

// TestUnrecognisedStatusIsRecordedNotGuessed pins the unknown-word rule: crier
// records what it cannot map instead of guessing, and unknown is NOT terminal,
// so it never fires a completion.
func TestUnrecognisedStatusIsRecordedNotGuessed(t *testing.T) {
	bridge := &fakeBridge{create: &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateRunning}}
	deliver := &recordingDeliverer{}
	svc := newService(t, bridge, deliver)

	if _, err := svc.CreateRun(context.Background(), daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	bridge.status = func(runID string) (*daggerctl.RunView, error) {
		return &daggerctl.RunView{RunID: runID, State: daggerctl.StateUnknown}, nil
	}
	rec, err := svc.RunStatus(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if rec.State != daggerctl.StateUnknown {
		t.Errorf("state = %q, want unknown", rec.State)
	}
	if deliver.count() != 0 {
		t.Errorf("an unknown state delivered %d message(s), want 0", deliver.count())
	}
}

// TestDeliveryFailureIsRecordedNotSwallowed keeps the "a missing notification
// is a reported fact" rule: the run still becomes terminal, and the reason is
// on the record.
func TestDeliveryFailureIsRecordedNotSwallowed(t *testing.T) {
	bridge := &fakeBridge{create: &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateSucceeded}}
	deliver := &recordingDeliverer{err: errors.New("inbox unavailable")}
	svc := newService(t, bridge, deliver)

	rec, err := svc.CreateRun(context.Background(), daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if rec.Notified {
		t.Error("notified = true despite a delivery failure")
	}
	if !strings.Contains(rec.NotifyError, "inbox unavailable") {
		t.Errorf("notify_error = %q, want the delivery failure named", rec.NotifyError)
	}
}

// ---------------------------------------------------------------------------
// no executor: the bridge is the ONLY way anything happens
// ---------------------------------------------------------------------------

// TestUnconfiguredServiceRefusesEveryVerbAndExecutesNothing is the boundary
// proof at the API level: with no bridge wired, every verb is a named refusal
// and the run store stays EMPTY — crier did not fall back to running anything.
func TestUnconfiguredServiceRefusesEveryVerbAndExecutesNothing(t *testing.T) {
	store := daggerctl.NewMemoryStore()
	svc := daggerctl.NewService(nil, store, &recordingDeliverer{})
	ctx := context.Background()

	verbs := map[string]func() (*daggerctl.RunRecord, error){
		"create": func() (*daggerctl.RunRecord, error) {
			return svc.CreateRun(ctx, daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"})
		},
		"status": func() (*daggerctl.RunRecord, error) { return svc.RunStatus(ctx, "run-1") },
		"cancel": func() (*daggerctl.RunRecord, error) { return svc.Cancel(ctx, "run-1") },
		"resume": func() (*daggerctl.RunRecord, error) { return svc.Resume(ctx, "run-1") },
		"rewind": func() (*daggerctl.RunRecord, error) { return svc.Rewind(ctx, "run-1", "n1") },
		"run_skill": func() (*daggerctl.RunRecord, error) {
			return svc.RunSkill(ctx, daggerctl.RunSkillRequest{AgentID: "a", Skill: "s"})
		},
	}
	for name, call := range verbs {
		t.Run(name, func(t *testing.T) {
			_, err := call()
			if !errors.Is(err, daggerctl.ErrUnconfigured) {
				t.Fatalf("err = %v, want ErrUnconfigured — the surface must name what is missing, never pretend", err)
			}
		})
	}

	runs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("an unconfigured surface left %d run record(s) — it invented local state", len(runs))
	}
}

// TestBridgeFailureLeavesNoLocalRunRecord proves the mirror image: when the
// bridge refuses to create a run, crier records NOTHING. There is no fallback
// executor that could have produced one.
func TestBridgeFailureLeavesNoLocalRunRecord(t *testing.T) {
	bridge := &fakeBridge{createErr: fmt.Errorf("%w: dagger is down", daggerctl.ErrBridge)}
	store := daggerctl.NewMemoryStore()
	svc := daggerctl.NewService(bridge, store, &recordingDeliverer{})

	if _, err := svc.CreateRun(context.Background(), daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"}); err == nil {
		t.Fatal("CreateRun succeeded against a failing bridge")
	}
	runs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("a failed create left %d record(s); crier must not mint a run the executor never accepted", len(runs))
	}
}

// TestPendingRunCreatedFromTheBridgeWithoutAStatusWordIsNotTerminal covers the
// documented default: a create with no status word is a live run, never a
// completion.
func TestPendingRunCreatedFromTheBridgeWithoutAStatusWordIsNotTerminal(t *testing.T) {
	bridge := &fakeBridge{create: &daggerctl.RunView{RunID: "run-1"}}
	deliver := &recordingDeliverer{}
	svc := newService(t, bridge, deliver)

	rec, err := svc.CreateRun(context.Background(), daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if rec.State != daggerctl.StateRunning {
		t.Errorf("state = %q, want running", rec.State)
	}
	if deliver.count() != 0 {
		t.Errorf("delivered %d message(s) for a run with no status word, want 0", deliver.count())
	}
}

// ---------------------------------------------------------------------------
// watcher: a run that finishes on its own still reaches its requester
// ---------------------------------------------------------------------------

func TestWatcherDeliversACompletionWithoutTheRequesterPolling(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	bridge := &fakeBridge{create: &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateRunning}}
	bridge.status = func(runID string) (*daggerctl.RunView, error) {
		mu.Lock()
		polls++
		n := polls
		mu.Unlock()
		if n < 2 {
			return &daggerctl.RunView{RunID: runID, State: daggerctl.StateRunning}, nil
		}
		return &daggerctl.RunView{RunID: runID, State: daggerctl.StateSucceeded}, nil
	}
	deliver := &recordingDeliverer{}
	svc := newService(t, bridge, deliver)
	svc.SetWatchInterval(2 * time.Millisecond)

	if _, err := svc.CreateRun(context.Background(), daggerctl.CreateRunRequest{AgentID: "a", Prompt: "p"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Watch(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for deliver.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if deliver.count() != 1 {
		t.Fatalf("watcher delivered %d message(s), want 1", deliver.count())
	}
	if code, _ := deliver.last(t)["code"].(string); code != daggerctl.CodeRunSucceeded {
		t.Errorf("code = %q, want %q", code, daggerctl.CodeRunSucceeded)
	}
}

func TestWatcherWithPausedIntervalDoesNothing(t *testing.T) {
	bridge := &fakeBridge{create: &daggerctl.RunView{RunID: "run-1", State: daggerctl.StateRunning}}
	svc := newService(t, bridge, &recordingDeliverer{})
	svc.SetWatchInterval(0)

	done := make(chan struct{})
	go func() {
		svc.Watch(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Watch with a zero interval must return immediately")
	}
}

// ---------------------------------------------------------------------------

// newRegistryWithAgent is a real registry store holding one agent — the
// production inbox backend the terminal notification must land in.
func newRegistryWithAgent(t *testing.T, agentID string) *registry.MemoryStore {
	t.Helper()
	store := registry.NewMemoryStore()
	if err := store.Register(&registry.Agent{ID: agentID}); err != nil {
		t.Fatalf("register agent %q: %v", agentID, err)
	}
	return store
}

// assertInboxHoldsNotification reads agentID's durable inbox and requires
// exactly one dagger notification carrying the expected code.
func assertInboxHoldsNotification(t *testing.T, store registry.Store, agentID, code string) {
	t.Helper()
	entries, _, err := store.Retrieve(agentID, 30*time.Second, 10)
	if err != nil {
		t.Fatalf("retrieve %q inbox: %v", agentID, err)
	}
	if len(entries) != 1 {
		t.Fatalf("%q inbox holds %d message(s), want exactly 1", agentID, len(entries))
	}
	var note daggerctl.RunNotification
	if err := json.Unmarshal(entries[0].Payload, &note); err != nil {
		t.Fatalf("decode notification: %v", err)
	}
	if note.Code != code {
		t.Errorf("notification code = %q, want %q", note.Code, code)
	}
	if note.RequestingAgent != agentID {
		t.Errorf("notification requesting_agent = %q, want %q", note.RequestingAgent, agentID)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
