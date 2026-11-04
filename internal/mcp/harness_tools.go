// harness_tools.go — the read-only harness tools carried over from
// COURIER-HARNESS §4.4 (folded into CRIER-MCP-NUDGE because the harness row
// declared the deviation).
//
// They wrap harness.Registry.Capabilities and Probe and nothing else: there is
// deliberately NO delivery tool here. Delivery is an ingress action, not an
// agent action — an MCP client can see what a harness can do, never make one
// run a turn.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/crier-dev/crier/internal/harness"
)

// handleHarnessList — harness_list. Probes every registered adapter and reports
// name + Capability. An unavailable adapter is reported (`available:false` with
// its reason), never omitted and never an error: listing is reporting.
func (s *MCPServer) handleHarnessList(args json.RawMessage) (any, error) {
	// Declared with an empty schema: any member at all is a caller mistake.
	if err := decodeArgs(args, &struct{}{}); err != nil {
		return nil, err
	}
	out := HarnessListOutput{Harnesses: []HarnessEntry{}}
	if s.harness == nil {
		return out, nil
	}
	caps := s.harness.Capabilities(context.Background())
	names := make([]string, 0, len(caps))
	for name := range caps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cap := caps[name]
		out.Harnesses = append(out.Harnesses, HarnessEntry{
			Name:      name,
			Available: cap.Available,
			Version:   cap.Version,
			Binary:    cap.Binary,
			Reason:    cap.Reason,
			Features:  cap.Features,
		})
	}
	out.Count = len(out.Harnesses)
	return out, nil
}

// handleHarnessProbe — harness_probe. An unregistered name is the harness
// vocabulary's HARNESS_UNKNOWN, named with the adapters that ARE known so the
// caller can correct itself in one step.
func (s *MCPServer) handleHarnessProbe(args json.RawMessage) (any, error) {
	var in HarnessProbeInput
	if err := decodeArgs(args, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("name is required")
	}
	if s.harness == nil {
		return nil, fmt.Errorf("%s: %q (no adapters are configured in this process)", harness.CodeUnknown, in.Name)
	}
	cap, probeErr := s.harness.Probe(context.Background(), in.Name)
	if probeErr != nil {
		return nil, fmt.Errorf("%s: %q (known: %s)", harness.CodeUnknown, in.Name, strings.Join(s.harness.Names(), ", "))
	}
	return cap, nil
}
