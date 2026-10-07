package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/crier-dev/crier/internal/chat"
)

// ---------------------------------------------------------------------------
// CR-CHAT-017 — sub-thread spawning and navigability.
//
// specs/CHAT-THREADING.md §4 and specs/CHAT-SESSIONS.md §4.5 (decision D11):
// a reply STAYS IN THREAD — depth is never a function of who replied. A
// sub-thread is created only when a message deliberately pulls in an agent
// that is not a participant of the session; replies to existing participants
// spawn nothing. This file holds the branch WRITE (the spawn), the depth
// collapse read, the timeline rail, the per-thread summary and the
// search-with-location read (§3.8.2 of CHAT-INTERFACE).
//
// Every record here goes through the SAME log the rest of the session API
// uses: a `session.thread.branch` record plus the message record, appended in
// one handler pass. The parent thread is left byte-identical apart from the
// anchor named on the branch record (§4.5 rule 3) — no field of the parent is
// rewritten, because a record is never rewritten.
// ---------------------------------------------------------------------------

// DefaultCollapseDepth is the collapse threshold the depth read applies when
// the caller names no depth (CHAT-INTERFACE §3.8.2): three is the deepest a
// reader is asked to follow before the interface offers to summarise instead.
// It is a READ default over the thread tree — never a stored shape and never
// a server-side truncation of the record (D11 consequence 5).
const DefaultCollapseDepth = 3

// activeAgentParticipants returns the session's ACTIVE AGENT member ids — the
// participant list a spawn decision is measured against.
func activeAgentParticipants(st *State, now time.Time) []string {
	var out []string
	for _, m := range st.Members {
		if m.MemberType == MemberAgent && m.ActiveAt(now) {
			out = append(out, m.MemberID)
		}
	}
	sort.Strings(out)
	return out
}

// messageAddressees derives the agent ids a message ADDRESSES (in the
// CHAT-ADDRESSING sense: a tag is addressing, never an action — D12): the
// explicit `agent` audience targets plus every @agent token in the payload's
// `text` field. Deduplicated, in stable order.
func messageAddressees(req *postMessageRequest) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ids []string) {
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, t := range req.Targets {
		if t.Kind == TargetAgent {
			add([]string{t.ID})
		}
	}
	add(chat.ParseAddressees(payloadText(req.Payload)))
	return out
}

// messageGroupMentions derives the `@team:<name>` tokens a message body
// carries (CR-CHAT-022): the NAMED-group form of the addressing grammar. The
// capability form is deliberately not returned here — `@cap:y` is a selector
// (one live holder, round-robin), not a fan-out, and never becomes a group
// target (D8).
func messageGroupMentions(req *postMessageRequest) []string {
	var out []string
	for _, m := range chat.ParseMentions(payloadText(req.Payload)) {
		if m.Kind == chat.MentionTeam {
			out = append(out, m.Ref)
		}
	}
	return out
}

// groupMentionTargets renders group names as `group` audience targets. The
// roster itself is NOT resolved here — resolveGroupRoster does that fresh at
// fan-out time, so a roster edit between this call and the delivery still
// routes to the CURRENT members (§1.4 consequence 1).
func (h *Handler) groupMentionTargets(ctx context.Context, names []string) ([]AudienceTarget, error) {
	targets := make([]AudienceTarget, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		if !ValidGroupName(name) {
			return nil, fmt.Errorf("group name %q is not addressable as @team:<name>", name)
		}
		seen[name] = true
		targets = append(targets, AudienceTarget{Kind: TargetGroup, ID: name})
	}
	return targets, nil
}

// payloadText reads the payload's `text` field — the convention the existing
// transcript surfaces read (`payload.text`). A payload without one addresses
// nobody by tag.
func payloadText(p json.RawMessage) string {
	if len(p) == 0 {
		return ""
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(p, &body); err != nil {
		return ""
	}
	return body.Text
}

// spawnedThreadView is what a send reports when it spawned a sub-thread.
type spawnedThreadView struct {
	ThreadID        string   `json:"thread_id"`
	ParentThreadID  string   `json:"parent_thread_id"`
	AnchorMessageID string   `json:"anchor_message_id"`
	RootMessageID   string   `json:"root_message_id"`
	Depth           int      `json:"depth"`
	Reason          string   `json:"reason"`
	NonMembers      []string `json:"non_member_addressees"`
}

// spawnSubThreads decides and records the sub-thread spawn for ONE message
// (§4.5 rule 2, atomic write).
//
// The decision: when the message ADDRESSES an agent NOT in the session's
// active agent participants, the message branches off the thread it was
// headed for into a child thread. Per §4.3 the thread key IS the root
// message's id, so the child thread's id is the message's own id; the branch
// record names parent_thread_id (the thread the message would have landed
// in) and anchor_message_id (the message the sender was replying to — the
// point the branch hangs from). The branching message becomes the CHILD's
// root: it stops being a reply, and the anchor carries its reply attribution.
// Multiple non-member addressees share ONE child: they are pulled into the
// same side conversation rather than N duplicate ones.
//
// When every addressee is already a participant — or the message is itself a
// fresh thread root, which simply opens a new top-level conversation — the
// message stays where it was headed and nothing is written: a reply to
// existing participants never spawns (D11).
//
// Atomicity: the branch record is appended FIRST and the message record rides
// the normal sendMessage pass; both carry seqs allocated back-to-back so the
// pair is contiguous in the log. If the branch record cannot be written the
// send is refused before any message record exists.
func (h *Handler) spawnSubThreads(ctx context.Context, sess *Session, msg *Message, addressees []string) (*spawnedThreadView, error) {
	if len(addressees) == 0 || msg.ParentID == "" {
		// No addressees, or the message opens its own top-level thread —
		// there is nothing to branch FROM (§4.5 rule 2: a sub-thread is a
		// branch off a message in ANOTHER thread).
		return nil, nil
	}
	st, err := h.store.Load(ctx, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("load session state for spawn decision: %w", err)
	}
	nonMembers := chat.NonMemberAddressees(activeAgentParticipants(st, h.now()), addressees)
	if len(nonMembers) == 0 {
		// Every addressee is a participant: the reply stays in thread.
		return nil, nil
	}

	parentThreadID := msg.ThreadID
	anchor := msg.ParentID
	// The branching message becomes the child thread's root (§4.3): its
	// thread_id IS its own message id, and the anchor carries the reply
	// attribution the dropped parent_id held.
	msg.ParentID = ""
	msg.ThreadID = msg.ID
	child := &Thread{
		ID:              msg.ID,
		SessionID:       sess.ID,
		ParentThreadID:  parentThreadID,
		RootMessageID:   msg.ID,
		AnchorMessageID: anchor,
		CreatedBy:       msg.Author,
		CreatedAt:       msg.CreatedAt,
	}
	seq, err := h.store.NextSeq(ctx, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("allocate sub-thread seq: %w", err)
	}
	reason := fmt.Sprintf("addressed non-participant agent(s) %s", strings.Join(nonMembers, ", "))
	// The ONE record that creates a new thread_id (§5.1). The parent thread
	// gains nothing but the anchor named here; no parent record is rewritten.
	if err := h.store.Branch(ctx, child, seq, reason); err != nil {
		return nil, fmt.Errorf("record sub-thread branch: %w", err)
	}
	return &spawnedThreadView{
		ThreadID:        msg.ID,
		ParentThreadID:  parentThreadID,
		AnchorMessageID: anchor,
		RootMessageID:   msg.ID,
		Depth:           st.ThreadDepth(parentThreadID) + 1,
		Reason:          reason,
		NonMembers:      nonMembers,
	}, nil
}

// ---------------------------------------------------------------------------
// GET /sessions/{id}/threads/{thread_id} — the thread read with DEPTH
// COLLAPSE (CHAT-INTERFACE §3.8.2). ?depth=N collapses children beyond N.
// ---------------------------------------------------------------------------

// threadSummaryCard is the COLLAPSED stand-in for a branch subtree: a
// generated index (§4.7 rule 2 — an index, never a replacement for the
// record), with the ids a client needs to expand it.
type threadSummaryCard struct {
	ThreadID         string    `json:"thread_id"`
	ParentThreadID   string    `json:"parent_thread_id,omitempty"`
	AnchorMessageID  string    `json:"anchor_message_id,omitempty"`
	Depth            int       `json:"depth"`
	MessageCount     int       `json:"message_count"`
	ParticipantCount int       `json:"participant_count"`
	SubThreadCount   int       `json:"sub_thread_count"`
	FirstSeq         int64     `json:"first_seq"`
	LastSeq          int64     `json:"last_seq"`
	LastAt           time.Time `json:"last_at"`
	Preview          string    `json:"preview"`
	Generated        bool      `json:"generated"`
}

type collapsedBranch struct {
	ThreadID  string            `json:"thread_id"`
	Depth     int               `json:"depth"`
	Collapsed bool              `json:"collapsed"`
	Summary   threadSummaryCard `json:"summary"`
}

type threadReadResponse struct {
	SessionID  string              `json:"session_id"`
	Thread     *Thread             `json:"thread"`
	Depth      int                 `json:"depth"`
	DepthLimit int                 `json:"depth_limit"`
	Messages   []transcriptMessage `json:"messages"`
	Branches   []*threadNodeView   `json:"branches,omitempty"`
	Collapsed  []*collapsedBranch  `json:"collapsed,omitempty"`
}

// threadNodeView is one ON-SCREEN branch of the tree (below the depth
// limit); its own children at or beyond the limit are reported collapsed.
type threadNodeView struct {
	Thread    *Thread             `json:"thread"`
	Depth     int                 `json:"depth"`
	Messages  []transcriptMessage `json:"messages"`
	Branches  []*threadNodeView   `json:"branches,omitempty"`
	Collapsed []*collapsedBranch  `json:"collapsed,omitempty"`
}

// summaryCardOf builds the generated summary card for one thread.
func summaryCardOf(st *State, t *Thread, depth int) threadSummaryCard {
	msgs := st.ThreadMessages(t.ID)
	card := threadSummaryCard{
		ThreadID:        t.ID,
		ParentThreadID:  t.ParentThreadID,
		AnchorMessageID: t.AnchorMessageID,
		Depth:           depth,
		MessageCount:    len(msgs),
		SubThreadCount:  len(st.SubThreads(t.ID)),
		Generated:       true,
	}
	seen := map[string]bool{}
	for _, m := range msgs {
		if card.FirstSeq == 0 || m.Seq < card.FirstSeq {
			card.FirstSeq = m.Seq
		}
		if m.Seq > card.LastSeq {
			card.LastSeq = m.Seq
			card.LastAt = m.CreatedAt
		}
		if id := m.Author.AgentID(); id != "" && !seen[id] {
			seen[id] = true
		}
	}
	card.ParticipantCount = len(seen)
	if len(msgs) > 0 {
		card.Preview = payloadText(msgs[len(msgs)-1].Payload)
		if len(card.Preview) > 120 {
			card.Preview = card.Preview[:120]
		}
	}
	return card
}

// threadNodeAt renders the ON-SCREEN part of the tree down to the depth
// limit, folding every branch at or below the limit into a collapsed summary
// card. A branch is rendered when its depth is strictly BELOW the limit;
// otherwise its summary card is reported on its parent node — so a branch is
// never both on-screen and collapsed.
func (st *State) threadNodeAt(threadID string, depth, limit int) *threadNodeView {
	t := st.Thread(threadID)
	if t == nil {
		return nil
	}
	node := &threadNodeView{Thread: t, Depth: depth}
	for _, m := range st.ThreadMessages(threadID) {
		node.Messages = append(node.Messages, transcriptMessage{
			ID: m.ID, ThreadID: m.ThreadID, ParentID: m.ParentID, Seq: m.Seq,
			MessageKind: string(m.Kind), Author: m.Author, CreatedAt: m.CreatedAt,
		})
	}
	for _, sub := range st.SubThreads(threadID) {
		if depth+1 < limit {
			if child := st.threadNodeAt(sub.ID, depth+1, limit); child != nil {
				node.Branches = append(node.Branches, child)
				node.Collapsed = append(node.Collapsed, child.Collapsed...)
			}
			continue
		}
		node.Collapsed = append(node.Collapsed, &collapsedBranch{
			ThreadID:  sub.ID,
			Depth:     depth + 1,
			Collapsed: true,
			Summary:   summaryCardOf(st, sub, depth+1),
		})
	}
	return node
}

// HandleThreadRead serves GET /sessions/{id}/threads/{thread_id}?depth=N:
// the thread's on-screen tree with children beyond the depth limit returned
// as collapsed:true summary cards (CHAT-INTERFACE §3.8.2). The collapse is a
// READ shape over the tree — the records themselves are never truncated.
func (h *Handler) HandleThreadRead(w http.ResponseWriter, r *http.Request) {
	st, sess, ok := h.loadScoped(w, r)
	if !ok {
		return
	}
	if err := h.authorizeRead(r.Context(), r, sess, st); err != nil {
		writeAPIError(w, http.StatusForbidden, "VISIBILITY_FORBIDDEN", err.Error())
		return
	}
	tid := muxVar(r, "thread_id")
	t := st.Thread(tid)
	if t == nil {
		writeAPIError(w, http.StatusNotFound, "THREAD_NOT_FOUND",
			fmt.Sprintf("thread %q does not exist in session %q", tid, sess.ID))
		return
	}
	limit := DefaultCollapseDepth
	if raw := strings.TrimSpace(r.URL.Query().Get("depth")); raw != "" {
		n := 0
		if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || n < 0 {
			writeAPIError(w, http.StatusBadRequest, "INVALID_DEPTH", "depth must be a non-negative integer")
			return
		}
		limit = n
	}
	resp := threadReadResponse{
		SessionID:  sess.ID,
		Thread:     t,
		Depth:      st.ThreadDepth(tid),
		DepthLimit: limit,
	}
	if node := st.threadNodeAt(tid, st.ThreadDepth(tid), limit); node != nil {
		resp.Messages = node.Messages
		resp.Branches = node.Branches
		resp.Collapsed = node.Collapsed
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// GET /sessions/{id}/threads/{thread_id}/timeline — the branch rail with the
// reader's current position.
// ---------------------------------------------------------------------------

type timelineNode struct {
	ThreadID        string          `json:"thread_id"`
	ParentThreadID  string          `json:"parent_thread_id,omitempty"`
	AnchorMessageID string          `json:"anchor_message_id,omitempty"`
	Depth           int             `json:"depth"`
	RootMessageID   string          `json:"root_message_id"`
	MessageCount    int             `json:"message_count"`
	LastSeq         int64           `json:"last_seq"`
	Current         bool            `json:"current"`
	Children        []*timelineNode `json:"children,omitempty"`
}

type timelineResponse struct {
	SessionID string          `json:"session_id"`
	ThreadID  string          `json:"thread_id"`
	Current   string          `json:"current"`
	Nodes     []*timelineNode `json:"nodes"`
}

// timelineCurrent names the thread the reader is in: the one holding the
// session's latest message among this subtree (the position the rail marks).
func (st *State) timelineCurrent(rootID string) string {
	bestID, bestSeq := "", int64(-1)
	var walk func(threadID string)
	seen := map[string]bool{}
	walk = func(threadID string) {
		if threadID == "" || seen[threadID] {
			return
		}
		seen[threadID] = true
		for _, m := range st.ThreadMessages(threadID) {
			if m.Seq > bestSeq {
				bestSeq, bestID = m.Seq, threadID
			}
		}
		for _, sub := range st.SubThreads(threadID) {
			walk(sub.ID)
		}
	}
	walk(rootID)
	return bestID
}

func (st *State) timelineNodes(threadID string, depth int, current string) []*timelineNode {
	t := st.Thread(threadID)
	if t == nil {
		return nil
	}
	msgs := st.ThreadMessages(threadID)
	node := &timelineNode{
		ThreadID:        threadID,
		ParentThreadID:  t.ParentThreadID,
		AnchorMessageID: t.AnchorMessageID,
		Depth:           depth,
		RootMessageID:   t.RootMessageID,
		MessageCount:    len(msgs),
		Current:         threadID == current,
	}
	for _, m := range msgs {
		if m.Seq > node.LastSeq {
			node.LastSeq = m.Seq
		}
	}
	for _, sub := range st.SubThreads(threadID) {
		node.Children = append(node.Children, st.timelineNodes(sub.ID, depth+1, current)...)
	}
	return []*timelineNode{node}
}

// HandleThreadTimeline serves GET /sessions/{id}/threads/{thread_id}/timeline:
// the whole branch tree of the thread — every sub-thread with its depth,
// anchor and parent — plus which node is the reader's current position.
func (h *Handler) HandleThreadTimeline(w http.ResponseWriter, r *http.Request) {
	st, sess, ok := h.loadScoped(w, r)
	if !ok {
		return
	}
	if err := h.authorizeRead(r.Context(), r, sess, st); err != nil {
		writeAPIError(w, http.StatusForbidden, "VISIBILITY_FORBIDDEN", err.Error())
		return
	}
	tid := muxVar(r, "thread_id")
	if st.Thread(tid) == nil {
		writeAPIError(w, http.StatusNotFound, "THREAD_NOT_FOUND",
			fmt.Sprintf("thread %q does not exist in session %q", tid, sess.ID))
		return
	}
	current := st.timelineCurrent(tid)
	writeJSON(w, http.StatusOK, timelineResponse{
		SessionID: sess.ID,
		ThreadID:  tid,
		Current:   current,
		Nodes:     st.timelineNodes(tid, st.ThreadDepth(tid), current),
	})
}

// ---------------------------------------------------------------------------
// GET /sessions/{id}/threads/{thread_id}/summary — the generated index plus
// the link to the raw messages it indexes.
// ---------------------------------------------------------------------------

type threadSummaryResponse struct {
	ThreadID string            `json:"thread_id"`
	Summary  threadSummaryCard `json:"summary"`
	// RawMessages is the LINK to the record the summary indexes (§4.7 rule 2:
	// the raw messages stay one read underneath, never replaced).
	RawMessages string `json:"raw_messages"`
	Generated   bool   `json:"generated"`
}

// HandleThreadSummary serves GET /sessions/{id}/threads/{thread_id}/summary:
// a GENERATED digest of the thread — visibly marked generated, with the raw
// messages one read underneath.
func (h *Handler) HandleThreadSummary(w http.ResponseWriter, r *http.Request) {
	st, sess, ok := h.loadScoped(w, r)
	if !ok {
		return
	}
	if err := h.authorizeRead(r.Context(), r, sess, st); err != nil {
		writeAPIError(w, http.StatusForbidden, "VISIBILITY_FORBIDDEN", err.Error())
		return
	}
	tid := muxVar(r, "thread_id")
	t := st.Thread(tid)
	if t == nil {
		writeAPIError(w, http.StatusNotFound, "THREAD_NOT_FOUND",
			fmt.Sprintf("thread %q does not exist in session %q", tid, sess.ID))
		return
	}
	writeJSON(w, http.StatusOK, threadSummaryResponse{
		ThreadID:    tid,
		Summary:     summaryCardOf(st, t, st.ThreadDepth(tid)),
		RawMessages: fmt.Sprintf("/sessions/%s/messages?thread_id=%s", sess.ID, tid),
		Generated:   true,
	})
}

// ---------------------------------------------------------------------------
// GET /sessions/{id}/search?q= — search that returns LOCATION
// (CHAT-INTERFACE §3.8.2): a hit is a path, not a fragment.
// ---------------------------------------------------------------------------

type searchHit struct {
	MessageID string `json:"message_id"`
	ThreadID  string `json:"thread_id"`
	Seq       int64  `json:"seq"`
	Author    string `json:"author,omitempty"`
	Snippet   string `json:"snippet"`
	// FullPath is the LOCATION of the hit: [namespace, channel, thread_id,
	// parent_thread_id?, message_id] — the parent thread id present only for
	// a sub-thread hit.
	FullPath []string `json:"full_path"`
}

type searchResponse struct {
	SessionID string      `json:"session_id"`
	Query     string      `json:"query"`
	Count     int         `json:"count"`
	Hits      []searchHit `json:"hits"`
}

// HandleSessionSearch serves GET /sessions/{id}/search?q= — a substring match
// over the transcript's payload text (and thread/message ids), every hit
// carrying its full_path: namespace > channel > thread > parent thread (for a
// sub-thread) > message. A hit states WHERE it lives, so a reader can walk to
// it.
func (h *Handler) HandleSessionSearch(w http.ResponseWriter, r *http.Request) {
	st, sess, ok := h.loadScoped(w, r)
	if !ok {
		return
	}
	if err := h.authorizeRead(r.Context(), r, sess, st); err != nil {
		writeAPIError(w, http.StatusForbidden, "VISIBILITY_FORBIDDEN", err.Error())
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeAPIError(w, http.StatusBadRequest, "QUERY_REQUIRED", "q is required")
		return
	}
	lower := strings.ToLower
	q = lower(q)
	channel := sess.Title
	if channel == "" {
		channel = sess.ID
	}
	hits := make([]searchHit, 0)
	for _, m := range st.Messages {
		text := lower(payloadText(m.Payload))
		if !strings.Contains(text, q) && !strings.Contains(lower(m.ThreadID), q) && !strings.Contains(lower(m.ID), q) {
			continue
		}
		path := []string{sess.Namespace, channel, m.ThreadID}
		if t := st.Thread(m.ThreadID); t != nil && t.ParentThreadID != "" {
			path = append(path, t.ParentThreadID)
		}
		path = append(path, m.ID)
		snippet := payloadText(m.Payload)
		if len(snippet) > 160 {
			snippet = snippet[:160]
		}
		hits = append(hits, searchHit{
			MessageID: m.ID,
			ThreadID:  m.ThreadID,
			Seq:       m.Seq,
			Author:    m.Author.AgentID(),
			Snippet:   snippet,
			FullPath:  path,
		})
	}
	writeJSON(w, http.StatusOK, searchResponse{SessionID: sess.ID, Query: q, Count: len(hits), Hits: hits})
}
