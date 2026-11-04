// nudge_surface.go — the RESOURCE and PROMPT halves of the nudge
// (CRIER-MCP-NUDGE §2.1):
//
//	resource  courier://inbox/{agent_id}/unread  — the same counts-only JSON
//	prompt    courier_nudge                      — the one-line template
//
// Both are read-only views over CrierSource.Summary. Neither carries a
// message body, and neither advances a cursor: a client that attaches the
// resource to a system prompt learns the count, not the mail.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/crier-dev/crier/internal/harness"
)

// NudgeResourceURITemplate is the addressable form clients attach (§2.1).
const NudgeResourceURITemplate = "courier://inbox/{agent_id}/unread"

// NudgePromptName is the prompt a client renders (§2.1).
const NudgePromptName = "courier_nudge"

// nudgePromptZero is the prompt body when there is nothing to read.
const nudgePromptZero = "You have no unread messages in Crier."

// resourceDescription / promptDescription are the metadata a client sees before
// it reads anything; they say "counts only" in words so nobody expects bodies.
const (
	resourceDescription = "Unread Crier counts for one agent: total, by_category, oldest_at and per-session counts. Counts only — no message text."
	promptDescription   = "One-line Crier nudge: how many unread messages are waiting and the tool to read them."
)

// resourceTemplate is one resources/templates/list entry.
type resourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// resourceEntry is one resources/list entry.
type resourceEntry struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// resourceContent is one resources/read body element.
type resourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
}

// promptArgument is one prompts/list argument declaration.
type promptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// promptDefinition is one prompts/list entry.
type promptDefinition struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []promptArgument `json:"arguments,omitempty"`
}

// handleResourcesList — resources/list. With a bridge identity the concrete
// resource for that agent is listed; the template covers every other agent.
func (s *MCPServer) handleResourcesList() any {
	out := []resourceEntry{}
	if s.nudgeEnabled() && s.agentID != "" {
		out = append(out, resourceEntry{
			URI:         NudgeResourceURI(s.agentID),
			Name:        "Crier unread",
			Description: resourceDescription,
			MimeType:    "application/json",
		})
	}
	return map[string]any{"resources": out}
}

// handleResourceTemplatesList — resources/templates/list.
func (s *MCPServer) handleResourceTemplatesList() any {
	out := []resourceTemplate{}
	if s.nudgeEnabled() {
		out = append(out, resourceTemplate{
			URITemplate: NudgeResourceURITemplate,
			Name:        "Crier unread",
			Description: resourceDescription,
			MimeType:    "application/json",
		})
	}
	return map[string]any{"resourceTemplates": out}
}

// handleResourcesRead — resources/read. The body is the summary JSON, byte for
// byte the same shape the tool answers.
func (s *MCPServer) handleResourcesRead(params json.RawMessage) (any, error) {
	var in struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &in); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	agentID, ok := parseNudgeResourceURI(in.URI)
	if !ok {
		return nil, fmt.Errorf("unknown resource: %s", in.URI)
	}
	if !s.nudgeEnabled() {
		return nil, fmt.Errorf("the Crier nudge surface is disabled (mcp.nudge.enabled=false)")
	}
	scopeAgent, _, err := s.nudgeScope(agentID, "")
	if err != nil {
		return nil, err
	}
	summary, err := s.unreadSummary(context.Background(), scopeAgent, "")
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(summary)
	if err != nil {
		return nil, err
	}
	return map[string]any{"contents": []resourceContent{{
		URI:      in.URI,
		MimeType: "application/json",
		Text:     string(body),
	}}}, nil
}

// handlePromptsList — prompts/list.
func (s *MCPServer) handlePromptsList() any {
	out := []promptDefinition{}
	if s.nudgeEnabled() {
		out = append(out, promptDefinition{
			Name:        NudgePromptName,
			Description: promptDescription,
			Arguments: []promptArgument{
				{Name: "agent_id", Description: "Agent whose unread mail to report (default: the bridge's own identity)"},
				{Name: "session_id", Description: "Narrow the count to one Crier session"},
			},
		})
	}
	return map[string]any{"prompts": out}
}

// handlePromptsGet — prompts/get. The server renders the line from the live
// counts, so a client that supports prompts does not have to substitute
// anything; {n} and {categories} in the spec's template are filled here.
func (s *MCPServer) handlePromptsGet(params json.RawMessage) (any, error) {
	var in struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal(params, &in); err != nil {
		return nil, fmt.Errorf("invalid params: %w", err)
	}
	if in.Name != NudgePromptName {
		return nil, fmt.Errorf("unknown prompt: %s", in.Name)
	}
	if !s.nudgeEnabled() {
		return nil, fmt.Errorf("the Crier nudge surface is disabled (mcp.nudge.enabled=false)")
	}
	agentID, sessionID, err := s.nudgeScope(in.Arguments["agent_id"], in.Arguments["session_id"])
	if err != nil {
		return nil, err
	}
	summary, err := s.unreadSummary(context.Background(), agentID, sessionID)
	if err != nil {
		return nil, err
	}
	text := NudgePromptTemplate(summary.Total, summary.ByCategory)
	return map[string]any{
		"description": promptDescription,
		"messages": []map[string]any{{
			"role":    "user",
			"content": map[string]any{"type": "text", "text": text},
		}},
	}, nil
}

// NudgePromptTemplate renders the §2.1 one-line template from a summary. Zero
// unread is a different sentence, not "0 unread ()".
func NudgePromptTemplate(total int, byCategory map[string]int) string {
	if total <= 0 {
		return nudgePromptZero
	}
	categories := CategoryList(byCategory)
	if len(categories) == 0 {
		return fmt.Sprintf("You have %d unread messages in Crier. Call courier_unread_list to read them.", total)
	}
	return fmt.Sprintf("You have %d unread messages in Crier (%s). Call courier_unread_list to read them.",
		total, strings.Join(categories, ", "))
}

// CategoryList returns the summary's category names in a stable order: busiest
// first, then alphabetical, so the same counts always render the same line.
func CategoryList(byCategory map[string]int) []string {
	out := make([]string, 0, len(byCategory))
	for name := range byCategory {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool {
		if byCategory[out[i]] != byCategory[out[j]] {
			return byCategory[out[i]] > byCategory[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// SessionStartCounts adapts a CrierSource to the harness delivery hook: the
// one-line session-start hint needs (total, categories) for the message's
// Crier session, and that is exactly what the summary already computes.
//
// It is the ONE place the two halves are joined, so the number a harness is
// told and the number courier_unread_summary answers cannot drift. The
// categories are ordered busiest-first (CategoryList) so the prefix is stable
// across calls.
func SessionStartCounts(c CrierSource, agentID string) func(ctx context.Context, msg harness.Message) (int, []string) {
	if c == nil {
		return nil
	}
	return func(ctx context.Context, msg harness.Message) (int, []string) {
		summary, err := c.Summary(ctx, UnreadQuery{AgentID: agentID, SessionID: msg.SessionID})
		if err != nil {
			// A hint is optional metadata on a delivery that already
			// succeeded at the store: a read failure must not fail the
			// delivery, and it must not invent a count either.
			return 0, nil
		}
		return summary.Total, CategoryList(summary.ByCategory)
	}
}

// NudgeResourceURI builds the concrete resource URI for one agent.
func NudgeResourceURI(agentID string) string {
	return "courier://inbox/" + agentID + "/unread"
}

// parseNudgeResourceURI extracts the agent id from a concrete resource URI.
func parseNudgeResourceURI(uri string) (string, bool) {
	const prefix = "courier://inbox/"
	const suffix = "/unread"
	if !strings.HasPrefix(uri, prefix) || !strings.HasSuffix(uri, suffix) {
		return "", false
	}
	agentID := strings.TrimSuffix(strings.TrimPrefix(uri, prefix), suffix)
	if agentID == "" || strings.Contains(agentID, "/") {
		return "", false
	}
	return agentID, true
}

// nudgeEnabled reports whether mcp.nudge.enabled left the surface armed.
func (s *MCPServer) nudgeEnabled() bool { return !s.nudgeDisabled }

// unreadSummary is the ONE summary path shared by the tool, the resource and
// the prompt, so all three can never disagree.
func (s *MCPServer) unreadSummary(ctx context.Context, agentID, sessionID string) (UnreadSummary, error) {
	if s.crier == nil {
		return s.inboxOnlySummary(agentID), nil
	}
	return s.crier.Summary(ctx, UnreadQuery{AgentID: agentID, SessionID: sessionID})
}
