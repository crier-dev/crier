// Package daggerctl is crier's CONTROL surface for a Hermes-dagger pipeline
// (CR-CHAT-033, specs/CHAT-INTERFACE.md). It lets a Hermes instance or an
// agent CREATE and CONTROL a DAG from inside a thread — create a run, observe
// its status, cancel it, resume it from a checkpoint, rewind to a node, and
// run a registered skill — and it delivers a run's terminal outcome back to
// the requesting agent through the SHIPPED inbox path.
//
// THE BOUNDARY RULE (decision D18). This package does NOT embed a second
// executor. DaggerBridge is the WHOLE of crier's execution vocabulary, and the
// only implementation this repository ships speaks HTTP/JSON to a dagger
// endpoint configured by CR_DAGGER_URL (see bridge.go). Crier holds the run's
// IDENTITY — its id, its state, its evidence references and the agent that
// asked for it — and nothing else. A second scheduler would be a second truth;
// there is none here, and internal/daggerctl contains no DAG evaluation, no
// node execution and no scheduling loop.
//
// Because the run's record is held by crier, the run is addressable and
// auditable like any other work: POST /dagger/runs creates it, GET
// /dagger/runs/{id} reads it back, and the terminal transition is delivered to
// the requester's durable inbox through the same Store.Deliver path a
// MESSAGE_EXPIRED receipt uses — never a side channel.
package daggerctl

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RunState is a run's lifecycle state as crier stores and reports it. The
// vocabulary is deliberately small: it is the set crier can act on, not a
// mirror of any particular engine's status words.
type RunState string

const (
	// StateRunning is a live run: created, not yet finished.
	StateRunning RunState = "running"
	// StateSucceeded, StateFailed and StateCancelled are the TERMINAL states.
	// Only a terminal state is delivered to the requesting agent.
	StateSucceeded RunState = "succeeded"
	StateFailed    RunState = "failed"
	StateCancelled RunState = "cancelled"
	// StateUnknown is a status word crier could not map onto the vocabulary.
	// It is recorded rather than guessed at.
	StateUnknown RunState = "unknown"
)

// Terminal reports whether a state is final. Only a terminal run is delivered.
//
// StateUnknown is deliberately NOT terminal: an unrecognised status is a
// status crier could not read, not a finished run, so it never fabricates an
// outcome and never fires a completion notification.
func (s RunState) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateCancelled:
		return true
	default:
		return false
	}
}

// Run kinds — what was asked for when the run was created.
const (
	KindPrompt = "prompt" // a prompt-driven DAG
	KindSkill  = "skill"  // a registered skill run
)

// Notification codes delivered to a requesting agent's inbox when a run goes
// terminal. They are machine-readable, in the same shape the inbox already
// carries for MESSAGE_EXPIRED and WEBHOOK_FAILED.
const (
	CodeRunSucceeded = "DAGGER_RUN_SUCCEEDED"
	CodeRunFailed    = "DAGGER_RUN_FAILED"
	CodeRunCancelled = "DAGGER_RUN_CANCELLED"
)

// normalizeState maps a bridge-supplied status word onto the vocabulary. An
// empty word stays EMPTY (the caller then keeps what it already had rather
// than writing a wrong value); an unrecognised word becomes StateUnknown.
func normalizeState(s string) RunState {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return ""
	case "running", "started", "in_progress", "in-progress":
		return StateRunning
	case "succeeded", "success", "completed", "complete", "done":
		return StateSucceeded
	case "failed", "failure", "error", "errored":
		return StateFailed
	case "cancelled", "canceled", "aborted":
		return StateCancelled
	default:
		return StateUnknown
	}
}

// Node is one DAG node's reported shape, as evidence of what the run did. It
// is a REFERENCE to the executor's state, not a copy of it: crier stores what
// the bridge reported and never derives node outcomes of its own.
type Node struct {
	ID     string `json:"id"`
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// RunRecord is crier's record of one DAG run: the run id the bridge returned,
// the state the bridge last reported, the requesting agent, and references to
// the run's evidence. It is the REST body, the stored JSONL line and the MCP
// tool result — one shape, so a reader cannot tell them apart.
type RunRecord struct {
	RunID string   `json:"run_id"`
	State RunState `json:"state"`
	// RequestingAgent is the agent whose inbox a terminal outcome is
	// delivered to. It is recorded at create time and never re-resolved.
	RequestingAgent string `json:"requesting_agent"`
	// Kind is KindPrompt or KindSkill — what created the run.
	Kind string `json:"kind"`
	// Prompt and Skill are the create inputs, kept so a reader can tell two
	// runs of the same pipeline apart. Exactly one is set, per Kind.
	Prompt string `json:"prompt,omitempty"`
	Skill  string `json:"skill,omitempty"`
	// Evidence is the list of references the executor reported (checkpoint
	// ids, artifact urls, …). crier treats them as opaque strings.
	Evidence []string `json:"evidence,omitempty"`
	// Nodes is the last reported node shape, when the bridge supplies one.
	Nodes []Node `json:"nodes,omitempty"`
	// Notified reports that the terminal outcome was delivered to the
	// requesting agent's inbox. It is what makes delivery exactly-once
	// across repeated status polls.
	Notified bool `json:"notified"`
	// NotifyError names why a terminal delivery did not land, so a missing
	// notification is a reported fact rather than a silence. Empty once a
	// delivery succeeds.
	NotifyError string    `json:"notify_error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RunNotification is the durable inbox payload delivered when a run reaches a
// terminal state. It is the whole message: the requesting agent learns the run
// id, the outcome and where the run's evidence lives, without polling.
type RunNotification struct {
	Kind            string    `json:"kind"` // always "dagger_run"
	Code            string    `json:"code"` // DAGGER_RUN_* — see the codes above
	RunID           string    `json:"run_id"`
	State           RunState  `json:"state"`
	RequestingAgent string    `json:"requesting_agent"`
	Prompt          string    `json:"prompt,omitempty"`
	Skill           string    `json:"skill,omitempty"`
	Evidence        []string  `json:"evidence,omitempty"`
	Nodes           []Node    `json:"nodes,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// notificationCode maps a terminal state to its inbox code.
func notificationCode(s RunState) string {
	switch s {
	case StateSucceeded:
		return CodeRunSucceeded
	case StateFailed:
		return CodeRunFailed
	case StateCancelled:
		return CodeRunCancelled
	default:
		return ""
	}
}

// CreateRunRequest is a request to start a prompt-driven DAG. AgentID names the
// agent the run was started for — the address its outcome is delivered to.
type CreateRunRequest struct {
	AgentID string `json:"agent_id"`
	Prompt  string `json:"prompt"`
}

// RunSkillRequest is a request to run a skill REGISTERED with the executor.
// crier never loads or interprets a skill; it names it.
type RunSkillRequest struct {
	AgentID string         `json:"agent_id"`
	Skill   string         `json:"skill"`
	Args    map[string]any `json:"args,omitempty"`
}

// RunView is one status observation reported by the executor: the state plus
// whatever evidence and node shape the executor offered. A field the executor
// did not report is left nil and the stored record keeps its previous value.
type RunView struct {
	RunID    string
	State    RunState
	Evidence []string
	Nodes    []Node
}

// Deliverer is the SHIPPED inbox path crier already uses for terminal
// notifications. The production implementation is the registry store's own
// Deliver (see cmd/server), so a dagger outcome travels exactly the same path
// as a MESSAGE_EXPIRED receipt: the durable inbox, no side channel.
type Deliverer interface {
	DeliverToInbox(agentID string, payload []byte) error
}

// Control is the whole surface crier offers over a DAG: create, observe,
// cancel, resume, rewind, and run a registered skill. Both the server-side
// Service and the out-of-process RESTClient implement it, which is what lets
// the MCP tools and the REST routes act on ONE set of records.
type Control interface {
	CreateRun(ctx context.Context, req CreateRunRequest) (*RunRecord, error)
	RunStatus(ctx context.Context, runID string) (*RunRecord, error)
	Cancel(ctx context.Context, runID string) (*RunRecord, error)
	Resume(ctx context.Context, runID string) (*RunRecord, error)
	Rewind(ctx context.Context, runID, nodeID string) (*RunRecord, error)
	RunSkill(ctx context.Context, req RunSkillRequest) (*RunRecord, error)
}

var (
	// ErrRunNotFound is a run id the store does not hold.
	ErrRunNotFound = errors.New("dagger run not found")
	// ErrUnconfigured is the control surface answering with no bridge wired
	// (CR_DAGGER_URL unset on the server). It is a NAMED refusal, never a
	// silent drop: the caller is told the surface exists but has no executor.
	ErrUnconfigured = errors.New("dagger control is not configured")
	// ErrInvalidInput is a request crier refused before touching the bridge.
	ErrInvalidInput = errors.New("invalid dagger request")
	// ErrBridge is a failure reported by the executor's HTTP surface (a
	// transport error, a non-2xx answer, an undecodable body).
	ErrBridge = errors.New("dagger bridge request failed")
)

// runIDRequired validates a run id at the API boundary.
func validateRunID(runID string) error {
	if strings.TrimSpace(runID) == "" {
		return fmt.Errorf("%w: run_id is required", ErrInvalidInput)
	}
	return nil
}

// validateAgent validates the requesting agent at the API boundary.
func validateAgent(agentID string) error {
	if strings.TrimSpace(agentID) == "" {
		return fmt.Errorf("%w: agent_id is required", ErrInvalidInput)
	}
	return nil
}
