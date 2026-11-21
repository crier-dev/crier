package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	op "github.com/coding-hermes/operator"
	"github.com/crier-dev/crier/internal/harness"
	operatoradapter "github.com/crier-dev/crier/internal/harness/operator"
)

type operatorScript struct {
	starts      atomic.Int32
	spec        op.RunSpec
	result      op.Result
	unavailable bool
}

func (*operatorScript) Descriptor() op.Descriptor {
	return op.Descriptor{Name: "scripted", Readback: op.ReadbackCapability{Sources: 1}}
}
func (d *operatorScript) Probe(context.Context) (op.Capability, error) {
	return op.Capability{Available: !d.unavailable, Version: "1.2.3", ReadbackSources: 1, Reason: "scripted unavailable"}, nil
}
func (d *operatorScript) Start(_ context.Context, s op.RunSpec) (op.Session, error) {
	d.starts.Add(1)
	d.spec = s
	return operatorSession{d.result}, nil
}
func (*operatorScript) Verify(context.Context, op.Result, op.RunSpec) (op.Verification, error) {
	v := op.Verification{Verdict: op.VerdictPass, Attempts: 1}
	for _, n := range []string{"session_id", "workspace", "model_served", "context", "artifact", "exit_clean", "no_fallback", "tokens_positive"} {
		v.Checks = append(v.Checks, op.Check{Name: n, Status: op.VerdictPass})
	}
	return v, nil
}
func (*operatorScript) Reap(context.Context, op.Result) error { return nil }

type operatorSession struct{ result op.Result }

func (s operatorSession) ID() string                              { return "operator-session" }
func (s operatorSession) Events() <-chan op.Event                 { c := make(chan op.Event); close(c); return c }
func (s operatorSession) Wait(context.Context) (op.Result, error) { return s.result, nil }
func (s operatorSession) Interrupt(context.Context) error         { return nil }
func (s operatorSession) Close() error                            { return nil }

// operatorLibraryContract is called by the SAME TestAdapterContract adapter list.
// R2 deliberately changes Harness/Model into agent/project; it does not promise
// SessionRef.ID resume, shell argv or process-group ownership. Those assertions
// remain unchanged for the four binary adapters. These cells grade the actual
// library contract rather than inventing process execution in the fake driver.
func operatorLibraryContract(t *testing.T) {
	newAdapter := func(t *testing.T, d *operatorScript) (*operatoradapter.Adapter, string, string) {
		t.Helper()
		wd := t.TempDir()
		path := filepath.Join(t.TempDir(), "ledger.jsonl")
		sink, err := op.OpenLedger(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sink.Close() })
		drivers := op.NewDriverRegistry()
		if err := drivers.Register(d); err != nil {
			t.Fatal(err)
		}
		operator, err := op.New(op.Config{Registry: op.NewAgentRegistry(op.Agent{ID: "agent", Harness: "scripted", Workspace: wd}), Drivers: drivers, Ledger: sink})
		if err != nil {
			t.Fatal(err)
		}
		return operatoradapter.New(operator), path, wd
	}
	t.Run("ProbeNeverStartsSession", func(t *testing.T) {
		d := &operatorScript{}
		a, _, _ := newAdapter(t, d)
		cap, err := a.Probe(context.Background())
		if err != nil || !cap.Available || cap.Version != "1.2.3" || d.starts.Load() != 0 || cap.Features["continue"] {
			t.Fatalf("probe=%+v err=%v starts=%d", cap, err, d.starts.Load())
		}
	})
	t.Run("ProbeUnavailable", func(t *testing.T) {
		d := &operatorScript{unavailable: true}
		a, _, _ := newAdapter(t, d)
		cap, err := a.Probe(context.Background())
		if err != nil || cap.Available || cap.Reason == "" || d.starts.Load() != 0 {
			t.Fatalf("probe=%+v err=%v", cap, err)
		}
	})
	for _, state := range []string{harness.StateAccepted, harness.StateDispatched} {
		t.Run(state, func(t *testing.T) {
			d := &operatorScript{result: op.Result{State: op.State(state), Created: true, StdoutBytes: 12}}
			a, path, _ := newAdapter(t, d)
			wd := t.TempDir()
			text := "literal ; $(id) `whoami`\nsecond line"
			msg := harness.Message{ID: "message-1", Author: "sender", Text: text}
			ref := harness.SessionRef{Harness: "agent", Model: "project", Workdir: wd}
			rc, err := a.Deliver(context.Background(), ref, msg)
			if err != nil {
				t.Fatal(err)
			}
			if rc.State != state || rc.Harness != "operator" || !rc.Created || rc.SessionID != "operator-session" || rc.StdoutBytes != 12 || rc.At.IsZero() {
				t.Fatalf("receipt=%+v", rc)
			}
			if d.spec.Input != text || d.spec.Agent.ID != "agent" || d.spec.Lane != "project" || d.spec.Workspace != wd {
				t.Fatalf("mapping=%+v", d.spec)
			}
			// The same message is idempotent even after the first turn is terminal.
			again, err := a.Deliver(context.Background(), ref, msg)
			if err != nil || again.SessionID != rc.SessionID || d.starts.Load() != 1 {
				t.Fatalf("duplicate=%+v err=%v starts=%d", again, err, d.starts.Load())
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var records []op.LedgerRecord
			for _, line := range splitOperatorLines(raw) {
				var r op.LedgerRecord
				if err := json.Unmarshal(line, &r); err != nil {
					t.Fatal(err)
				}
				records = append(records, r)
			}
			if len(records) != 2 {
				t.Fatalf("ledger rows=%d", len(records))
			}
			for _, r := range records {
				if r.CorrID != msg.ID || r.Idem != "crier:"+msg.ID || r.Caller != "crier:"+msg.Author || r.Agent != "agent" || r.Project != "project" {
					t.Fatalf("attribution=%+v", r)
				}
			}
			wire, err := json.Marshal(rc)
			if err != nil {
				t.Fatal(err)
			}
			var back harness.Receipt
			if err := json.Unmarshal(wire, &back); err != nil || back.State != state || back.SessionID != rc.SessionID {
				t.Fatal("receipt round trip", err)
			}
		})
	}
	for _, code := range []string{harness.CodeUnknown, harness.CodeUnavailable, harness.CodeSessionUnknown, harness.CodeTimeout, harness.CodeExitNonZero, harness.CodeOutputInvalid, harness.CodeDisabled, harness.CodeConcurrencyFull, harness.CodeWorkdirMissing, harness.CodeInterrupted} {
		t.Run(code, func(t *testing.T) {
			d := &operatorScript{result: op.Result{State: op.StateFailed, Error: op.Code(code), ExitCode: 3, Tail: "scripted failure"}}
			a, _, _ := newAdapter(t, d)
			rc, err := a.Deliver(context.Background(), harness.SessionRef{Harness: "agent"}, harness.Message{ID: code, Text: "x"})
			if err != nil || rc.State != harness.StateFailed || rc.Error != code || rc.ExitCode != 3 || rc.Tail != "scripted failure" || d.starts.Load() != 1 {
				t.Fatalf("receipt=%+v err=%v starts=%d", rc, err, d.starts.Load())
			}
		})
	}
	t.Run("PreAttemptRefusal", func(t *testing.T) {
		d := &operatorScript{}
		a, _, _ := newAdapter(t, d)
		_, err := a.Deliver(context.Background(), harness.SessionRef{Harness: "missing"}, harness.Message{ID: "missing"})
		var refusal *op.Refusal
		if !errors.As(err, &refusal) || refusal.Code != op.RefusalAgentUnknown || d.starts.Load() != 0 {
			t.Fatalf("refusal=%v starts=%d", err, d.starts.Load())
		}
	})
}

func splitOperatorLines(raw []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			if i > start {
				lines = append(lines, raw[start:i])
			}
			start = i + 1
		}
	}
	return lines
}
