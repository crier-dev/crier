package session

import "sort"

// ---------------------------------------------------------------------------
// CR-CHAT-015 — nested reads: the thread tree and the message attachment tree.
//
// These are READ surfaces over the State the package already reduces from the
// transcript (§4.3). They add no object and no storage: a thread tree is a walk
// of parent_thread_id and a message tree is the §4.3 "attach each record to its
// parent_id" grouping. Both are reconstructed from the stored records ALONE, so
// a caller needs no client state.
// ---------------------------------------------------------------------------

// ReplyNode is one node of a thread's ATTACHMENT tree: a transcript message
// plus the messages that name it as their parent_id (§4.3).
//
// It is what a reader renders a thread with, and it is NOT a depth measure:
// parent_id is reply ATTRIBUTION, never a level (D11, §4.5). The nesting of
// this tree counts replies-to-replies and says nothing about the thread tree's
// level; a caller asking "how deep is this?" asks State.ThreadDepth about the
// message's thread_id. A reader that treats this nesting as thread levels has
// misread the record.
type ReplyNode struct {
	Message *Message     `json:"message"`
	Replies []*ReplyNode `json:"replies,omitempty"`
}

// ThreadNode is one thread of a session's THREAD tree with its transcript and
// its deliberately branched sub-threads (§4.1, §4.5). The thread tree is the
// only thing that deepens: a reply stays in its thread and adds no node here;
// a `session.thread.branch` record is what adds one (D11).
type ThreadNode struct {
	Thread   *Thread       `json:"thread"`
	Messages []*Message    `json:"messages"`
	Branches []*ThreadNode `json:"branches,omitempty"`
}

// messageSeq returns the seq of a message, or ok=false when the id is not in
// the transcript (a root that was retained away).
func (st *State) messageSeq(id string) (int64, bool) {
	if m := st.Message(id); m != nil {
		return m.Seq, true
	}
	return 0, false
}

// orderThreads sorts threads by the seq of their root message — so the top
// level reads in the order the conversations were opened — and falls back to
// the thread id when a root message is not in the transcript. A thread whose
// root message is present sorts before one whose root was retained away.
func (st *State) orderThreads(threads []*Thread) {
	sort.SliceStable(threads, func(i, j int) bool {
		si, oki := st.messageSeq(threads[i].RootMessageID)
		sj, okj := st.messageSeq(threads[j].RootMessageID)
		if oki != okj {
			return oki
		}
		if oki && si != sj {
			return si < sj
		}
		return threads[i].ID < threads[j].ID
	})
}

// RootThreads returns the session's TOP-LEVEL threads: the ones no branch
// created (parent_thread_id empty), ordered by the seq of their root message.
//
// This is the "top level" of the interface (§1.4 acceptance 2): one entry per
// conversation rather than one per message, which is what lets two features be
// read as two distinct threads instead of one interleaved stream.
func (st *State) RootThreads() []*Thread {
	var out []*Thread
	for _, t := range st.Threads {
		if t.ParentThreadID == "" {
			out = append(out, t)
		}
	}
	st.orderThreads(out)
	return out
}

// SubThreads returns the threads deliberately branched off threadID
// (§4.5 rule 2) — the direct children in the thread tree, ordered by root seq.
// An empty threadID returns nothing: a root thread is not a sub-thread of "".
func (st *State) SubThreads(threadID string) []*Thread {
	if threadID == "" {
		return nil
	}
	var out []*Thread
	for _, t := range st.Threads {
		if t.ParentThreadID == threadID {
			out = append(out, t)
		}
	}
	st.orderThreads(out)
	return out
}

// ThreadTree returns ONE thread with its messages (in seq order) and its nested
// sub-threads, recursively — the walk of parent_thread_id that ThreadDepth also
// uses (§4.5, §4.7 rule 1). A thread id that is not in the session returns nil.
// A cycle (which a valid transcript cannot contain) is cut rather than looped.
func (st *State) ThreadTree(threadID string) *ThreadNode {
	return st.threadTree(threadID, map[string]bool{})
}

func (st *State) threadTree(threadID string, seen map[string]bool) *ThreadNode {
	if threadID == "" || seen[threadID] {
		return nil
	}
	t := st.Thread(threadID)
	if t == nil {
		return nil
	}
	seen[threadID] = true
	node := &ThreadNode{Thread: t, Messages: st.ThreadMessages(threadID)}
	for _, sub := range st.SubThreads(threadID) {
		if child := st.threadTree(sub.ID, seen); child != nil {
			node.Branches = append(node.Branches, child)
		}
	}
	return node
}

// SessionThreads returns the session's top-level threads, each carrying its
// transcript and its nested sub-threads — the ordered TREE the interface draws
// at the top level (§1.4 acceptance 1 and 2). Two threads never share a
// message: a node's Messages are exactly the records whose thread_id is that
// thread, so reading one conversation cannot show another's.
func (st *State) SessionThreads() []*ThreadNode {
	roots := st.RootThreads()
	out := make([]*ThreadNode, 0, len(roots))
	seen := map[string]bool{}
	for _, t := range roots {
		if n := st.threadTree(t.ID, seen); n != nil {
			out = append(out, n)
		}
	}
	return out
}

// MessageTree returns a thread's messages attached to their parent_id, in seq
// order: each node names the message it replies to, recursively. It is the
// §4.3 reconstruction ("attach each record to its parent_id") for a whole
// thread, reconstructed from the stored records alone — no side table, no
// membership lookup, no client state.
//
// A message whose parent_id names a message that is not in this thread is
// returned at the top level in seq order rather than dropped, so the read never
// loses a record. §4.3's broken threads are REPORTED on State.Findings; this
// read returns everything that IS recorded.
//
// The nesting is reply ATTRIBUTION, not thread depth (D11, §4.5): a three-deep
// tree here is three replies in a row, all at the SAME thread level.
func (st *State) MessageTree(threadID string) []*ReplyNode {
	msgs := st.ThreadMessages(threadID)
	inThread := make(map[string]*Message, len(msgs))
	for _, m := range msgs {
		inThread[m.ID] = m
	}
	children := map[string][]*Message{}
	var roots []*Message
	for _, m := range msgs { // ThreadMessages is seq-ordered
		if m.ParentID == "" || inThread[m.ParentID] == nil {
			roots = append(roots, m)
			continue
		}
		children[m.ParentID] = append(children[m.ParentID], m)
	}
	out := make([]*ReplyNode, 0, len(roots))
	for _, r := range roots {
		out = append(out, buildReplyTree(r, children, map[string]bool{}))
	}
	return out
}

// buildReplyTree attaches a message's replies recursively, cutting a cycle
// (which a valid transcript cannot contain) rather than looping.
func buildReplyTree(m *Message, children map[string][]*Message, seen map[string]bool) *ReplyNode {
	seen[m.ID] = true
	n := &ReplyNode{Message: m}
	for _, c := range children[m.ID] {
		if seen[c.ID] {
			continue
		}
		n.Replies = append(n.Replies, buildReplyTree(c, children, seen))
	}
	return n
}
