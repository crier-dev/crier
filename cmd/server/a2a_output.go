package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/registry"
)

// recordPublishedOutput is installed only with the A2A route. It runs on the
// validated HTTP publish path, before fanout/202, never in a subscriber or poll.
// X-Agent-ID is the publishing identity (as on the existing relay route), not
// the envelope's self-reported role/metadata. No header means no task evidence;
// ordinary anonymous relay traffic remains accepted when rate limiting is off.
// This checks ownership, not cryptographic authorship: the relay's existing
// bearer/realm authorization still governs who can assert X-Agent-ID.
func (h *a2aHandler) recordPublishedOutput(realm, agentID, topic string, event json.RawMessage) error {
	if agentID == "" || !strings.HasPrefix(topic, "a2a.task.") {
		return nil
	}
	env, ok := a2a.ParseEnvelope(event)
	if !ok || env.Role != a2a.RoleAgent {
		return nil
	}
	// Accept the envelope and A2A message spellings, but never choose between
	// conflicting claims. Parts use the binding's existing lossless mapper.
	var ids struct {
		TaskID      string `json:"task_id"`
		TaskIDCamel string `json:"taskId"`
	}
	if json.Unmarshal(event, &ids) != nil || (ids.TaskID != "" && ids.TaskIDCamel != "" && ids.TaskID != ids.TaskIDCamel) {
		return nil
	}
	taskID := ids.TaskID
	if taskID == "" {
		taskID = ids.TaskIDCamel
	}
	wantTopic, valid := a2a.TaskTopic(taskID)
	if !valid || wantTopic != topic {
		return nil
	}
	parts, err := a2a.PartsFromPayload(event)
	if err != nil || !a2a.HasOutputParts(parts) {
		return nil
	}
	backend, ok := h.store.(registry.TaskOutputStore)
	if !ok {
		return fmt.Errorf("task outputs unsupported by inbox backend")
	}
	agent, err := h.store.Get(agentID)
	if errors.Is(err, registry.ErrAgentNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lookup output publisher: %w", err)
	}
	if !agent.A2A.OptedIn() || namespace.Canonical(agent.Namespace) != namespace.Canonical(realm) {
		return nil
	}
	// This composite-key lookup includes retained pre-Ack snapshots, so an
	// answer after hard-delete works, but foreign and nonexistent task ids do
	// not. Require the original task envelope, not an arbitrary inbox row.
	ev, rpcErr := h.lifecycleEvidence(agentID, taskID)
	if rpcErr != nil {
		return fmt.Errorf("lookup published task: %s", rpcErr.Message)
	}
	if ev.Entry == nil {
		return nil
	}
	if _, ok := a2a.ParseEnvelope(ev.Entry.Payload); !ok {
		return nil
	}
	artifact, err := json.Marshal(a2a.Artifact{
		ArtifactID: taskID + "-answer", Parts: parts,
		Metadata: map[string]any{"crier": map[string]any{"relay_topic": topic}},
	})
	if err != nil {
		return fmt.Errorf("encode published output: %w", err)
	}
	return backend.RecordTaskOutput(agentID, registry.TaskOutput{TaskID: taskID, ObservedAt: h.opts.now(), Artifact: artifact})
}
