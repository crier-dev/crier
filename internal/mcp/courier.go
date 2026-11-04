// courier.go — the unread nudge surface (CRIER-MCP-NUDGE,
// specs/COURIER-MCP-NUDGE.md §2.1, §3, §5).
//
// The nudge INFORMS: it reports how much mail is waiting, never what the mail
// says. UnreadSummary and its resource body therefore carry counts only, and
// the one tool that does return bodies (courier_unread_list) is an explicit,
// bounded call the agent makes itself. Nothing here reads, leases or acks
// anything — the read cursors are consulted, never advanced.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/reads"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/session"
)

// Bounds on courier_unread_list (§2.1): default 20, hard maximum 100. An
// over-max limit is CLAMPED, not refused — the tool is a read, and a client
// asking for "everything" still gets a bounded answer instead of an error.
const (
	NudgeDefaultListLimit = 20
	NudgeMaxListLimit     = 100
)

// Uncategorized is the by_category bucket for messages that carry no tag
// (§3, §6): an untagged message is never omitted from the counts.
const Uncategorized = "uncategorized"

// The §5 error catalog. An UNKNOWN agent is deliberately NOT in this list:
// "a nudge must never fail a turn", so an agent nobody has registered gets an
// empty summary with total:0 (NUDGE_AGENT_UNKNOWN is a documentation name for
// that answer, not a returned error).
var (
	// ErrNudgeAgentRequired — NUDGE_AGENT_REQUIRED: no agent_id argument and
	// no bridge identity (CRIER_AGENT_ID) to fall back to.
	ErrNudgeAgentRequired = errors.New("NUDGE_AGENT_REQUIRED")
	// ErrNudgeScopeDenied — NUDGE_SCOPE_DENIED: another agent's summary was
	// asked for by a bridge that is not an operator.
	ErrNudgeScopeDenied = errors.New("NUDGE_SCOPE_DENIED")
)

// UnreadSummary is the counts-only answer (§2.3). It carries no message text
// and no payload: the field set IS the contract, and TestUnreadSummaryShape
// asserts a body string from the inbox never appears in its JSON.
type UnreadSummary struct {
	Total      int             `json:"total"`
	ByCategory map[string]int  `json:"by_category"`
	OldestAt   *time.Time      `json:"oldest_at,omitempty"`
	Sessions   []SessionUnread `json:"sessions"`
}

// SessionUnread is one courier session's unread count for the queried agent.
type SessionUnread struct {
	SessionID string `json:"session_id"`
	Unread    int    `json:"unread"`
}

// UnreadMessage is one entry of courier_unread_list. `text` is the payload's
// `text` field — present only on this explicit read tool, never on the summary
// or the resource.
type UnreadMessage struct {
	MessageID string   `json:"message_id"`
	SessionID string   `json:"session_id"`
	Author    string   `json:"author"`
	Tags      []string `json:"tags"`
	Text      string   `json:"text"`
	Seq       int64    `json:"seq"`
}

// UnreadList is the bounded body of courier_unread_list.
type UnreadList struct {
	Count    int             `json:"count"`
	Messages []UnreadMessage `json:"messages"`
}

// UnreadQuery scopes one nudge read. AgentID is always resolved (the bridge
// identity fills it in when the caller names none); SessionID is optional and
// narrows the query to one courier session.
type UnreadQuery struct {
	AgentID   string
	SessionID string
}

// CourierSource answers the nudge from the live courier stores. It is an
// interface so the MCP server can be built against a test source and so a
// future HTTP-backed source (a bridge on another host) can be substituted
// without touching the tool handlers.
type CourierSource interface {
	Summary(ctx context.Context, q UnreadQuery) (UnreadSummary, error)
	Unread(ctx context.Context, q UnreadQuery, limit int) (UnreadList, error)
}

// Courier reads unread state from the shipped courier stores: the registry's
// agent inboxes (counts) and the session transcript plus read cursors
// (per-session counts, bodies). Every field is optional; a nil Sessions/Reads
// means this process cannot see session state and the summary reports the
// agent-inbox numbers alone.
type Courier struct {
	// Inbox is the registry store the agent-inbox unread comes from (the
	// existing inboxStats number).
	Inbox registry.Store
	// Sessions is the session repository, read-only here.
	Sessions session.Repository
	// Reads is the read-cursor service; it is what knows a member's cursor.
	Reads *reads.Service
	// Now is the clock for oldest_at; nil means time.Now().UTC().
	Now func() time.Time
}

// Summary implements CourierSource (§2.1). It never returns message bodies:
// the totals are inbox queue depth plus each member session's unread count, and
// by_category aggregates the TAGS of the unread session messages only (an
// agent-inbox entry carries no tags today, so its messages land in
// `uncategorized`).
//
// An agent the registry does not know is answered with an empty summary, not an
// error (§5): silence is the correct answer to "do I have mail" for an agent
// that is not registered yet.
func (c *Courier) Summary(ctx context.Context, q UnreadQuery) (UnreadSummary, error) {
	out := UnreadSummary{ByCategory: map[string]int{}, Sessions: []SessionUnread{}}
	if c == nil {
		return out, nil
	}

	if c.Inbox != nil && q.AgentID != "" {
		depth, _, oldestAge, err := c.Inbox.Stats(q.AgentID)
		switch {
		case errors.Is(err, registry.ErrAgentNotFound):
			// NUDGE_AGENT_UNKNOWN: empty summary, no error.
		case err != nil:
			return out, fmt.Errorf("courier_unread_summary: inbox stats: %w", err)
		case depth > 0:
			out.Total += depth
			out.ByCategory[Uncategorized] += depth
			at := c.now().Add(-oldestAge)
			out.OldestAt = earlier(out.OldestAt, at)
		}
	}

	unreadBySession, err := c.unreadBySession(ctx, q.AgentID, q.SessionID)
	if err != nil {
		return out, err
	}
	for _, row := range unreadBySession {
		if len(row.messages) == 0 {
			continue
		}
		out.Sessions = append(out.Sessions, SessionUnread{SessionID: row.sessionID, Unread: len(row.messages)})
		out.Total += len(row.messages)
		for _, m := range row.messages {
			if len(m.Tags) == 0 {
				out.ByCategory[Uncategorized]++
				continue
			}
			for _, tag := range m.Tags {
				out.ByCategory[tag]++
			}
		}
		if at := row.oldest; !at.IsZero() {
			out.OldestAt = earlier(out.OldestAt, at)
		}
	}
	return out, nil
}

// Unread implements CourierSource: the bounded "read them" the nudge names
// (§2.1). Ordering is session then seq, so the answer is stable.
func (c *Courier) Unread(ctx context.Context, q UnreadQuery, limit int) (UnreadList, error) {
	limit = ClampUnreadLimit(limit)
	out := UnreadList{Messages: []UnreadMessage{}}
	if c == nil {
		return out, nil
	}
	rows, err := c.unreadBySession(ctx, q.AgentID, q.SessionID)
	if err != nil {
		return out, err
	}
	for _, row := range rows {
		for _, m := range row.messages {
			if len(out.Messages) == limit {
				out.Count = len(out.Messages)
				return out, nil
			}
			tags := m.Tags
			if tags == nil {
				tags = []string{}
			}
			out.Messages = append(out.Messages, UnreadMessage{
				MessageID: m.ID,
				SessionID: row.sessionID,
				Author:    m.Author.AgentID(),
				Tags:      tags,
				Text:      messageText(m.Payload),
				Seq:       m.Seq,
			})
		}
	}
	out.Count = len(out.Messages)
	return out, nil
}

// ClampUnreadLimit applies the §2.1 bound: the default when unset, the maximum
// when over it.
func ClampUnreadLimit(limit int) int {
	switch {
	case limit <= 0:
		return NudgeDefaultListLimit
	case limit > NudgeMaxListLimit:
		return NudgeMaxListLimit
	default:
		return limit
	}
}

// sessionUnread is one session's unread messages for the queried agent, with
// its oldest timestamp already resolved.
type sessionUnread struct {
	sessionID string
	messages  []session.Message
	oldest    time.Time
}

// unreadBySession walks the sessions the agent participates in and asks the
// read-cursor service for its unread rows. Sessions without unread are still
// returned (empty) so the caller decides what to report.
func (c *Courier) unreadBySession(ctx context.Context, agentID, sessionID string) ([]sessionUnread, error) {
	if c.Reads == nil || c.Sessions == nil || agentID == "" {
		return nil, nil
	}
	ids, err := c.sessionIDs(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]sessionUnread, 0, len(ids))
	for _, id := range ids {
		st, err := c.Sessions.Load(ctx, id)
		if err != nil {
			if errors.Is(err, session.ErrSessionNotFound) {
				continue
			}
			return nil, fmt.Errorf("courier unread: load session %q: %w", id, err)
		}
		if !isActiveAgentMember(st, agentID, c.now()) {
			// §6: an agent's summary is scoped to its own member id. A
			// session the agent is not in is not its mail.
			continue
		}
		msgs, err := c.Reads.Unread(ctx, id, agentID, nil)
		if err != nil {
			return nil, fmt.Errorf("courier unread: session %q: %w", id, err)
		}
		row := sessionUnread{sessionID: id, messages: msgs}
		for _, m := range msgs {
			if row.oldest.IsZero() || m.CreatedAt.Before(row.oldest) {
				row.oldest = m.CreatedAt
			}
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].sessionID < out[j].sessionID })
	return out, nil
}

// sessionIDs returns the sessions to consult: exactly one when the query names
// it, otherwise every session the repository holds (the membership check
// narrows that to the agent's own rooms).
func (c *Courier) sessionIDs(ctx context.Context, sessionID string) ([]string, error) {
	if sessionID != "" {
		return []string{sessionID}, nil
	}
	ids, err := c.Sessions.Sessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("courier unread: list sessions: %w", err)
	}
	sort.Strings(ids)
	return ids, nil
}

func (c *Courier) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// isActiveAgentMember reports whether agentID is an ACTIVE agent participant of
// st. A removed member has no read-state row and no mail (§7 of COURIER-READS).
func isActiveAgentMember(st *session.State, agentID string, now time.Time) bool {
	if st == nil {
		return false
	}
	for _, m := range st.Members {
		if m.MemberType == session.MemberAgent && m.MemberID == agentID && m.ActiveAt(now) {
			return true
		}
	}
	return false
}

// earlier keeps the oldest of two timestamps, treating nil as "none yet".
func earlier(current *time.Time, candidate time.Time) *time.Time {
	if candidate.IsZero() {
		return current
	}
	candidate = candidate.UTC()
	if current == nil || candidate.Before(*current) {
		return &candidate
	}
	return current
}

// messageText reads the payload's `text` field — the transcript convention the
// session surfaces use. A payload without one is empty text, never a dump of
// the opaque payload.
func messageText(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return ""
	}
	return body.Text
}

// nudgeScope resolves who the nudge is about (§5):
//
//   - an explicit agent_id wins; without one the BRIDGE's own identity is used;
//   - neither present is NUDGE_AGENT_REQUIRED — a count with no member has no
//     meaning;
//   - an explicit agent that is not the bridge's identity is NUDGE_SCOPE_DENIED
//     unless the deployment armed operator scope.
func (s *MCPServer) nudgeScope(agentID, sessionID string) (string, string, error) {
	agentID = strings.TrimSpace(agentID)
	sessionID = strings.TrimSpace(sessionID)
	if agentID == "" {
		agentID = s.agentID
	}
	if agentID == "" {
		return "", "", fmt.Errorf("%w: supply agent_id, or set %s so the bridge knows which agent it is",
			ErrNudgeAgentRequired, EnvAgentID)
	}
	if s.agentID != "" && agentID != s.agentID && !s.operatorScope {
		return "", "", fmt.Errorf("%w: %q is not this bridge's identity (%q); an operator scope is required to read another agent's summary",
			ErrNudgeScopeDenied, agentID, s.agentID)
	}
	return agentID, sessionID, nil
}

// handleCourierUnreadSummary — courier_unread_summary (§2.1). Counts only.
func (s *MCPServer) handleCourierUnreadSummary(args json.RawMessage) (any, error) {
	var in CourierUnreadSummaryInput
	if err := decodeArgs(args, &in); err != nil {
		return nil, err
	}
	agentID, sessionID, err := s.nudgeScope(in.AgentID, in.SessionID)
	if err != nil {
		return nil, err
	}
	if s.courier == nil {
		return s.inboxOnlySummary(agentID), nil
	}
	// Tool handlers have no context parameter on this bridge; the courier
	// reads are local store reads, so a background context is the honest
	// equivalent (there is no caller deadline to honour).
	return s.courier.Summary(context.Background(), UnreadQuery{AgentID: agentID, SessionID: sessionID})
}

// handleCourierUnreadList — courier_unread_list (§2.1). The explicit, bounded
// read the nudge names; it advances nothing.
func (s *MCPServer) handleCourierUnreadList(args json.RawMessage) (any, error) {
	var in CourierUnreadListInput
	if err := decodeArgs(args, &in); err != nil {
		return nil, err
	}
	agentID, sessionID, err := s.nudgeScope(in.AgentID, in.SessionID)
	if err != nil {
		return nil, err
	}
	if s.courier == nil {
		return UnreadList{Count: 0, Messages: []UnreadMessage{}}, nil
	}
	return s.courier.Unread(context.Background(), UnreadQuery{AgentID: agentID, SessionID: sessionID}, in.Limit)
}

// inboxOnlySummary is the answer when this process has no session source wired:
// the agent-inbox number is still real (it comes from the same store the inbox
// tools use), and the session list is honestly empty rather than invented.
func (s *MCPServer) inboxOnlySummary(agentID string) UnreadSummary {
	out := UnreadSummary{ByCategory: map[string]int{}, Sessions: []SessionUnread{}}
	depth, _, oldestAge, err := s.store.Stats(agentID)
	if err != nil || depth == 0 {
		// ErrAgentNotFound is the §5 unknown-agent case: total 0, no error.
		return out
	}
	out.Total = depth
	out.ByCategory[Uncategorized] = depth
	at := time.Now().UTC().Add(-oldestAge)
	out.OldestAt = &at
	return out
}
