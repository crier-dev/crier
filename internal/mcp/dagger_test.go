package mcp

// Handler-level tests for the dagger control tools (CR-CHAT-033). They drive
// the real JSON-RPC dispatch path with a fake DaggerControl, so the tools are
// proven REGISTERED, callable and correctly delegating without any network,
// executor or server — the boundary rule (D18) is that these tools only ever
// ask something else to run a DAG.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/daggerctl"
	"github.com/crier-dev/crier/internal/registry"
)

// fakeDaggerControl records what each tool asked for and answers a fixed record.
type fakeDaggerControl struct {
	calls    []string
	record   *daggerctl.RunRecord
	failWith error
}

func (f *fakeDaggerControl) out() *daggerctl.RunRecord {
	if f.record != nil {
		return f.record
	}
	return &daggerctl.RunRecord{RunID: "run-1", State: daggerctl.StateRunning, Kind: daggerctl.KindPrompt}
}

func (f *fakeDaggerControl) CreateRun(_ context.Context, req daggerctl.CreateRunRequest) (*daggerctl.RunRecord, error) {
	f.calls = append(f.calls, "create:"+req.AgentID+":"+req.Prompt)
	if f.failWith != nil {
		return nil, f.failWith
	}
	return f.out(), nil
}

func (f *fakeDaggerControl) RunStatus(_ context.Context, runID string) (*daggerctl.RunRecord, error) {
	f.calls = append(f.calls, "status:"+runID)
	if f.failWith != nil {
		return nil, f.failWith
	}
	return f.out(), nil
}

func (f *fakeDaggerControl) Cancel(_ context.Context, runID string) (*daggerctl.RunRecord, error) {
	f.calls = append(f.calls, "cancel:"+runID)
	if f.failWith != nil {
		return nil, f.failWith
	}
	return f.out(), nil
}

func (f *fakeDaggerControl) Resume(_ context.Context, runID string) (*daggerctl.RunRecord, error) {
	f.calls = append(f.calls, "resume:"+runID)
	if f.failWith != nil {
		return nil, f.failWith
	}
	return f.out(), nil
}

func (f *fakeDaggerControl) Rewind(_ context.Context, runID, nodeID string) (*daggerctl.RunRecord, error) {
	f.calls = append(f.calls, "rewind:"+runID+":"+nodeID)
	if f.failWith != nil {
		return nil, f.failWith
	}
	return f.out(), nil
}

func (f *fakeDaggerControl) RunSkill(_ context.Context, req daggerctl.RunSkillRequest) (*daggerctl.RunRecord, error) {
	f.calls = append(f.calls, "skill:"+req.AgentID+":"+req.Skill)
	if f.failWith != nil {
		return nil, f.failWith
	}
	return f.out(), nil
}

func daggerTestServer(t *testing.T, ctrl DaggerControl) *MCPServer {
	t.Helper()
	return NewWithOptions(registry.NewMemoryStore(), Options{Dagger: ctrl})
}

func TestDaggerToolsDelegateToTheControlSurface(t *testing.T) {
	fake := &fakeDaggerControl{}
	s := daggerTestServer(t, fake)

	cases := []struct {
		tool string
		args any
		want string
	}{
		{"create_run", CreateRunInput{AgentID: "hermes-1", Prompt: "build"}, "create:hermes-1:build"},
		{"run_status", RunStatusInput{RunID: "run-1"}, "status:run-1"},
		{"cancel_run", CancelRunInput{RunID: "run-1"}, "cancel:run-1"},
		{"resume_run", ResumeRunInput{RunID: "run-1"}, "resume:run-1"},
		{"rewind_run", RewindRunInput{RunID: "run-1", NodeID: "node-2"}, "rewind:run-1:node-2"},
		{"run_skill", RunSkillInput{AgentID: "hermes-1", Skill: "summarize"}, "skill:hermes-1:summarize"},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			text, isErr := parseToolResult(t, callTool(t, s, tc.tool, tc.args))
			if isErr {
				t.Fatalf("%s returned an error: %s", tc.tool, text)
			}
			var rec daggerctl.RunRecord
			if err := json.Unmarshal([]byte(text), &rec); err != nil {
				t.Fatalf("%s result is not a run record: %v (%s)", tc.tool, err, text)
			}
			if rec.RunID != "run-1" {
				t.Errorf("%s answered run_id %q, want run-1", tc.tool, rec.RunID)
			}
			if got := fake.calls[len(fake.calls)-1]; got != tc.want {
				t.Errorf("%s reached the surface as %q, want %q", tc.tool, got, tc.want)
			}
		})
	}
}

func TestDaggerToolsRequireTheServerURL(t *testing.T) {
	// No Dagger control and no HTTP URL: every tool refuses with the gate
	// naming CRIER_HTTP_URL, before it looks at anything else.
	s := New(registry.NewMemoryStore())
	for _, tool := range []string{"create_run", "run_status", "cancel_run", "resume_run", "rewind_run", "run_skill"} {
		t.Run(tool, func(t *testing.T) {
			text, isErr := parseToolResult(t, callTool(t, s, tool, struct{}{}))
			if !isErr {
				t.Fatalf("%s succeeded with no server URL: %s", tool, text)
			}
			if !strings.Contains(text, EnvHTTPURL) {
				t.Errorf("%s gate error does not name %s: %s", tool, EnvHTTPURL, text)
			}
		})
	}
}

func TestDaggerToolsValidateTheirArguments(t *testing.T) {
	s := daggerTestServer(t, &fakeDaggerControl{})
	cases := []struct {
		tool string
		args any
		want string
	}{
		{"create_run", CreateRunInput{Prompt: "p"}, "agent_id"},
		{"create_run", CreateRunInput{AgentID: "a"}, "prompt"},
		{"run_status", RunStatusInput{}, "run_id"},
		{"cancel_run", CancelRunInput{}, "run_id"},
		{"resume_run", ResumeRunInput{}, "run_id"},
		{"rewind_run", RewindRunInput{RunID: "run-1"}, "node_id"},
		{"run_skill", RunSkillInput{Skill: "s"}, "agent_id"},
		{"run_skill", RunSkillInput{AgentID: "a"}, "skill"},
	}
	for _, tc := range cases {
		t.Run(tc.tool+"/"+tc.want, func(t *testing.T) {
			text, isErr := parseToolResult(t, callTool(t, s, tc.tool, tc.args))
			if !isErr {
				t.Fatalf("%s accepted invalid arguments: %s", tc.tool, text)
			}
			if !strings.Contains(text, tc.want) {
				t.Errorf("error does not name %q: %s", tc.want, text)
			}
		})
	}
}

func TestDaggerToolSurfaceErrorsAreInBand(t *testing.T) {
	fake := &fakeDaggerControl{failWith: daggerctl.ErrUnconfigured}
	s := daggerTestServer(t, fake)

	text, isErr := parseToolResult(t, callTool(t, s, "create_run", CreateRunInput{AgentID: "a", Prompt: "p"}))
	if !isErr {
		t.Fatalf("a failing control surface did not produce an in-band error: %s", text)
	}
	if !strings.Contains(text, daggerctl.ErrUnconfigured.Error()) {
		t.Errorf("error does not carry the surface's reason: %s", text)
	}
}
