// Package operator adapts the Operator library to Crier's internal delivery API.
package operator

import (
	"context"
	"errors"

	op "github.com/coding-hermes/operator"
	"github.com/crier-dev/crier/internal/harness"
)

// Adapter delegates invocation and verification; it never starts a session in Probe.
type Adapter struct{ op *op.Operator }

// New wraps an already-configured Operator. The caller owns its dependencies.
func New(operator *op.Operator) *Adapter { return &Adapter{op: operator} }
func (*Adapter) Name() string            { return "operator" }
func (a *Adapter) Probe(ctx context.Context) (harness.Capability, error) {
	statuses, err := a.op.Drivers(ctx)
	if err != nil {
		return harness.Capability{}, err
	}
	cap := harness.Capability{Features: map[string]bool{"create": true, "continue": false}}
	for _, s := range statuses {
		if s.Available {
			cap.Available = true
			cap.Version = s.Version
			cap.Binary = s.Binary
			return cap, nil
		}
		cap.Reason = s.Reason
	}
	if cap.Reason == "" {
		cap.Reason = "no available Operator driver"
	}
	return cap, nil
}
func (a *Adapter) Deliver(ctx context.Context, s harness.SessionRef, m harness.Message) (harness.Receipt, error) {
	// Per spec 04 §6.3, Harness is the agent id and Model is the project slot.
	// SessionRef.ID is intentionally not a resume instruction in this mapping.
	h, err := a.op.Spawn(ctx, op.SpawnRequest{AgentID: s.Harness, Project: s.Model, Input: m.Text,
		Overrides: op.Overrides{Workdir: s.Workdir}, IdempotencyKey: "crier:" + m.ID,
		CorrID: m.ID, Caller: "crier:" + m.Author, ReplyTo: m.Author})
	if err != nil {
		return harness.Receipt{}, err
	}
	r, err := h.Wait(ctx)
	if err != nil {
		// Cancellation while waiting did not turn the already-attempted call into a
		// pre-attempt Go error; stop it and read its terminal receipt.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			_ = h.Cancel(context.Background())
			r, err = h.Wait(context.WithoutCancel(ctx))
		}
		if err != nil {
			return harness.Receipt{}, err
		}
	}
	state := r.DeliveryState
	if r.RunState == "failed" || r.RunState == "timed_out" || r.RunState == op.RunCancelled || r.RunState == "unknown" {
		state = harness.StateFailed
	}
	return harness.Receipt{Harness: a.Name(), SessionID: r.Result.SessionID, Created: r.Result.Created,
		State: state, ExitCode: r.ExitCode, DurationMS: r.DurationMS, StdoutBytes: r.Result.StdoutBytes,
		Tail: r.Tail, Error: r.ErrorCode, At: r.EndedAt}, nil
}
