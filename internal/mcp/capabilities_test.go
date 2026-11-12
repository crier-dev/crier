package mcp

import (
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/registry"
)

func TestCapabilityReportMatchesToolsListAndNamesEveryUnlock(t *testing.T) {
	server := New(registry.NewMemoryStore())
	report := CapabilityReportMarkdown(server.ToolNames())
	if got := strings.Count(report, "| `"); got != server.ToolCount() {
		t.Fatalf("capability rows = %d, want tools/list count %d", got, server.ToolCount())
	}
	for _, name := range server.ToolNames() {
		if !strings.Contains(report, "| `"+name+"` |") {
			t.Errorf("capability report does not name %q", name)
		}
	}
	for _, p := range toolPrerequisites {
		row := "| `" + p.Tool + "` | "
		start := strings.Index(report, row)
		if start < 0 {
			t.Errorf("capability report does not contain gated tool %q", p.Tool)
			continue
		}
		end := strings.IndexByte(report[start:], '\n')
		if end < 0 {
			end = len(report) - start
		}
		line := report[start : start+end]
		if !strings.Contains(line, p.Condition) {
			t.Errorf("%s: report row %q omits condition %q", p.Tool, line, p.Condition)
		}
		for _, env := range p.EnvVars {
			if !strings.Contains(line, env) {
				t.Errorf("%s: report row omits enabling variable %s", p.Tool, env)
			}
		}
	}
	if !strings.Contains(report, "Always available") {
		t.Fatal("capability report does not document always-available tools")
	}
}
