package webhook

import (
	"errors"
	"fmt"
)

// ErrPaused is returned when a delivery is refused because the agent's outbound
// webhook lane was paused by the detection layer's kill-switch (CR-FEAT-030).
// It is a terminal, operator-caused refusal — never retried.
var ErrPaused = errors.New("webhook: agent paused by the kill-switch")

// Paused reports whether the agent's outbound webhook lane is paused.
func (d *Driver) Paused(agentID string) bool {
	if d == nil || agentID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.paused[agentID]
}

// PauseAgent stops every outbound webhook delivery for one agent and drops what
// it had queued, returning how many deliveries were dropped (the per-agent
// batch buffer plus the shared redelivery queue). It is the kill-switch's
// outbound step: containment is worthless if the contained agent's already
// accepted deliveries keep POSTing to its endpoint, or to someone else's.
//
// What it does NOT do, stated so an operator does not over-trust it:
//
//   - a POST already in flight when the call lands is not recalled (an HTTP
//     request cannot be un-sent);
//   - dropped deliveries are DEAD-LETTERED the way v1 dead-letters everything
//     else — one structured log line each, counted in
//     webhook_deliveries_total{outcome="dropped"} — not written to a file;
//   - the pause is per-process. A restart forgets it, which is why the
//     kill-switch also quarantines the agent in the detector and unregisters
//     its registry row: those are the states that survive.
func (d *Driver) PauseAgent(agentID string) (int, error) {
	if d == nil {
		return 0, errors.New("webhook: nil driver")
	}
	if agentID == "" {
		return 0, errors.New("webhook: empty agent id")
	}

	// 1. Mark paused and take the agent's pending batch buffer.
	d.mu.Lock()
	if d.paused == nil {
		d.paused = make(map[string]bool)
	}
	already := d.paused[agentID]
	d.paused[agentID] = true
	buffered := 0
	if buf := d.batches[agentID]; buf != nil {
		items, _ := buf.take()
		buffered = len(items)
		delete(d.batches, agentID)
	}
	d.mu.Unlock()

	// 2. Drop what the shared redelivery queue holds for that agent.
	queued := d.dropQueued(agentID)

	total := buffered + queued
	if total > 0 {
		webhookOutcomeTotal.With("dropped").Add(float64(total))
	}
	logf("webhook: agent paused by kill-switch",
		"agent", agentID, "already_paused", already,
		"buffered_dropped", buffered, "queued_dropped", queued)
	return total, nil
}

// dropQueued removes every queued item for agentID and pushes the rest back,
// preserving their relative order. The queue is bounded, so draining and
// re-filling it is O(n) on a call an operator makes once.
func (d *Driver) dropQueued(agentID string) int {
	items := d.queue.PopBatch(0)
	if len(items) == 0 {
		return 0
	}
	dropped := 0
	for _, it := range items {
		if it == nil {
			continue
		}
		if it.AgentID == agentID {
			dropped++
			continue
		}
		_ = d.queue.Push(it)
	}
	return dropped
}

// refuseIfPaused is the guard the delivery entry points share.
func (d *Driver) refuseIfPaused(agentID string) error {
	if d.Paused(agentID) {
		return fmt.Errorf("%w: %s", ErrPaused, agentID)
	}
	return nil
}
