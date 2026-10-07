package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crier-dev/crier/internal/daggerctl"
)

// daggerToolTimeout bounds one control call made from an MCP tool. The tools
// are synchronous by contract (the executor owns any long-running work), so the
// budget is a client timeout and nothing more.
const daggerToolTimeout = 60 * time.Second

// DaggerControl is the dagger control surface the dagger MCP tools delegate to.
// It drives the SAME records the crier server holds — in process through the
// server's Service, out of process through daggerctl.RESTClient — so a run
// created from a thread is visible in that thread. Crier never embeds an
// executor (decision D18): this is a client, not an engine.
type DaggerControl interface {
	CreateRun(ctx context.Context, req daggerctl.CreateRunRequest) (*daggerctl.RunRecord, error)
	RunStatus(ctx context.Context, runID string) (*daggerctl.RunRecord, error)
	Cancel(ctx context.Context, runID string) (*daggerctl.RunRecord, error)
	Resume(ctx context.Context, runID string) (*daggerctl.RunRecord, error)
	Rewind(ctx context.Context, runID, nodeID string) (*daggerctl.RunRecord, error)
	RunSkill(ctx context.Context, req daggerctl.RunSkillRequest) (*daggerctl.RunRecord, error)
}

// daggerControl returns the control surface, or the gate error naming the one
// variable that enables it. The gate is checked BEFORE any argument decode, so
// a client that cannot run the tool at all is told that first — the same order
// mesh_peers uses.
func (s *MCPServer) daggerControl() (daggerctl.Control, error) {
	if s.dagger == nil {
		return nil, fmt.Errorf("dagger control requires CRIER_HTTP_URL (the Crier server base URL exposing the /dagger control routes)")
	}
	return s.dagger, nil
}

// withDagger runs fn under the tool timeout.
func withDagger(fn func(ctx context.Context) (*daggerctl.RunRecord, error)) (*daggerctl.RunRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), daggerToolTimeout)
	defer cancel()
	return fn(ctx)
}

// handleCreateRun creates a DAG run and answers with its record — run id, state
// and requesting agent included.
func (s *MCPServer) handleCreateRun(args json.RawMessage) (any, error) {
	ctrl, err := s.daggerControl()
	if err != nil {
		return nil, err
	}
	var in CreateRunInput
	if err := decodeArgs(args, &in); err != nil {
		return nil, err
	}
	if in.AgentID == "" {
		return nil, fmt.Errorf("agent_id is required")
	}
	if in.Prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	return withDagger(func(ctx context.Context) (*daggerctl.RunRecord, error) {
		return ctrl.CreateRun(ctx, daggerctl.CreateRunRequest{AgentID: in.AgentID, Prompt: in.Prompt, Target: in.Target})
	})
}

// handleRunStatus reads a run's record back.
func (s *MCPServer) handleRunStatus(args json.RawMessage) (any, error) {
	ctrl, err := s.daggerControl()
	if err != nil {
		return nil, err
	}
	var in RunStatusInput
	if err := decodeArgs(args, &in); err != nil {
		return nil, err
	}
	if in.RunID == "" {
		return nil, fmt.Errorf("run_id is required")
	}
	return withDagger(func(ctx context.Context) (*daggerctl.RunRecord, error) {
		return ctrl.RunStatus(ctx, in.RunID)
	})
}

// handleCancelRun cancels a running DAG.
func (s *MCPServer) handleCancelRun(args json.RawMessage) (any, error) {
	ctrl, err := s.daggerControl()
	if err != nil {
		return nil, err
	}
	var in CancelRunInput
	if err := decodeArgs(args, &in); err != nil {
		return nil, err
	}
	if in.RunID == "" {
		return nil, fmt.Errorf("run_id is required")
	}
	return withDagger(func(ctx context.Context) (*daggerctl.RunRecord, error) {
		return ctrl.Cancel(ctx, in.RunID)
	})
}

// handleResumeRun resumes a run from its last checkpoint.
func (s *MCPServer) handleResumeRun(args json.RawMessage) (any, error) {
	ctrl, err := s.daggerControl()
	if err != nil {
		return nil, err
	}
	var in ResumeRunInput
	if err := decodeArgs(args, &in); err != nil {
		return nil, err
	}
	if in.RunID == "" {
		return nil, fmt.Errorf("run_id is required")
	}
	return withDagger(func(ctx context.Context) (*daggerctl.RunRecord, error) {
		return ctrl.Resume(ctx, in.RunID)
	})
}

// handleRewindRun rewinds a run to a node.
func (s *MCPServer) handleRewindRun(args json.RawMessage) (any, error) {
	ctrl, err := s.daggerControl()
	if err != nil {
		return nil, err
	}
	var in RewindRunInput
	if err := decodeArgs(args, &in); err != nil {
		return nil, err
	}
	if in.RunID == "" {
		return nil, fmt.Errorf("run_id is required")
	}
	if in.NodeID == "" {
		return nil, fmt.Errorf("node_id is required")
	}
	return withDagger(func(ctx context.Context) (*daggerctl.RunRecord, error) {
		return ctrl.Rewind(ctx, in.RunID, in.NodeID)
	})
}

// handleRunSkill runs a registered skill on the executor.
func (s *MCPServer) handleRunSkill(args json.RawMessage) (any, error) {
	ctrl, err := s.daggerControl()
	if err != nil {
		return nil, err
	}
	var in RunSkillInput
	if err := decodeArgs(args, &in); err != nil {
		return nil, err
	}
	if in.AgentID == "" {
		return nil, fmt.Errorf("agent_id is required")
	}
	if in.Skill == "" {
		return nil, fmt.Errorf("skill is required")
	}
	return withDagger(func(ctx context.Context) (*daggerctl.RunRecord, error) {
		return ctrl.RunSkill(ctx, daggerctl.RunSkillRequest{AgentID: in.AgentID, Skill: in.Skill, Args: in.Args, Target: in.Target})
	})
}
