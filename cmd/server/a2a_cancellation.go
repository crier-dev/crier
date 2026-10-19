package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/registry"
)

func observation(ev a2a.TaskEvidence, outcome string, now time.Time) (registry.TaskObservation, error) {
	raw, err := json.Marshal(ev)
	return registry.TaskObservation{TaskID: ev.TaskID(), ObservedAt: now, Outcome: outcome, Evidence: raw}, err
}

func (h *a2aHandler) observations(agentID string) ([]a2a.TaskEvidence, error) {
	backend, ok := h.store.(registry.TaskObservationStore)
	if !ok {
		return nil, nil
	}
	records, err := backend.TaskObservations(agentID)
	if err != nil {
		return nil, err
	}
	out := make([]a2a.TaskEvidence, 0, len(records))
	for _, record := range records {
		var ev a2a.TaskEvidence
		if err := json.Unmarshal(record.Evidence, &ev); err != nil {
			return nil, fmt.Errorf("decode task observation: %w", err)
		}
		out = append(out, ev)
	}
	return out, nil
}

func (h *a2aHandler) observedEvidence(agentID, taskID string) (a2a.TaskEvidence, bool, error) {
	backend, ok := h.store.(registry.TaskObservationStore)
	if !ok {
		return a2a.TaskEvidence{}, false, nil
	}
	record, found, err := backend.TaskObservation(agentID, taskID)
	if err != nil || !found {
		return a2a.TaskEvidence{}, false, err
	}
	var ev a2a.TaskEvidence
	if err := json.Unmarshal(record.Evidence, &ev); err != nil {
		return a2a.TaskEvidence{}, false, err
	}
	return ev, true, nil
}

func (h *a2aHandler) closeCanceled(backend registry.TaskObservationStore, agentID string, ev a2a.TaskEvidence, now time.Time) error {
	ev.CanceledAt = now
	record, err := observation(ev, "canceled", now)
	if err != nil {
		return err
	}
	return backend.CloseObservedTask(agentID, record)
}

// Persist before emitting: a successful terminal frame must never depend on
// the observing stream remaining alive. Failure is an error, not success.
func (s *a2aStream) persistObservation(outcome string, now time.Time) bool {
	backend, ok := s.h.store.(registry.TaskObservationStore)
	if !ok {
		s.h.writeEvent(s.w, a2a.ErrorResponse(s.id, a2a.TaskCapabilityUnsupportedError("stream observation", "registry.TaskObservationStore")))
		s.events++
		return false
	}
	ev := a2a.TaskEvidence{Entry: s.snapshot}
	switch outcome {
	case "acknowledged":
		ev.AcknowledgedAt = now
	case "unknown":
		ev.RemovedAt = now
	}
	record, err := observation(ev, outcome, now)
	if err == nil {
		err = backend.RecordTaskObservation(s.target, record)
	}
	if err != nil {
		s.h.writeEvent(s.w, a2a.ErrorResponse(s.id, a2a.NewRPCError(a2a.CodeInternalError, "the task observation could not be retained: "+err.Error())))
		s.events++
		return false
	}
	return true
}
