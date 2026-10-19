package main

import (
	"time"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/registry"
)

type a2aCancellationKey struct{ agentID, taskID string }

// cancellation is process-local evidence, not a reconstruction from absence.
// Include the agent in the key: message ids are scoped to inboxes.
func (h *a2aHandler) cancellation(agentID, taskID string) (a2a.TaskEvidence, bool) {
	h.cancelMu.RLock()
	defer h.cancelMu.RUnlock()
	ev, ok := h.canceled[a2aCancellationKey{agentID, taskID}]
	return ev, ok
}

func (h *a2aHandler) closeCanceled(closer registry.InboxCloser, agentID string, ev a2a.TaskEvidence, now time.Time) error {
	h.cancelMu.Lock()
	defer h.cancelMu.Unlock()
	if err := closer.CloseEntry(agentID, ev.TaskID()); err != nil {
		return err
	}
	if h.canceled == nil {
		h.canceled = make(map[a2aCancellationKey]a2a.TaskEvidence)
	}
	ev.CanceledAt = now
	h.canceled[a2aCancellationKey{agentID, ev.TaskID()}] = ev
	return nil
}
