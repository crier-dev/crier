package chat

import (
	"errors"
	"time"
)

// Sub-thread spawn decision (CR-CHAT-017, specs/CHAT-THREADING.md §4 and
// specs/CHAT-SESSIONS.md §4.5 rule 2, decision D11).
//
// The rule this file encodes: a REPLY STAYS IN THREAD. Agents talking to each
// other inside a thread are messages at the SAME level — depth is never a
// function of who replied or of a tag being used. A sub-thread is created only
// when a message ADDRESSES an agent that is not a participant of the session:
// pulling a stranger into an existing conversation is the deliberate branch
// the three-level model reserves a sub-thread for. Replies to existing
// participants spawn nothing.
//
// This file holds the DECISION only; the caller (internal/session) owns the
// records — session.thread.branch plus the message — because a branch record
// is a transcript fact, and this package holds no store.

// NonMemberAddressees returns the agent addressees that are NOT in the
// participant list, in first-appearance order without duplicates. These are
// the agents whose address spawns a sub-thread; an empty result means the
// message stays in its thread.
//
// participants is the session's agent membership (agent-typed members only);
// addressees is the message's agent targets. Both are compared as exact
// strings — trimming and deduplication are the caller's job (EnsureUnique).
func NonMemberAddressees(participants, addressees []string) []string {
	members := make(map[string]bool, len(participants))
	for _, p := range participants {
		members[p] = true
	}
	var out []string
	seen := make(map[string]bool, len(addressees))
	for _, a := range addressees {
		if a == "" || members[a] || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// BranchRecord records the creation of a sub-thread (specs/CHAT-SESSIONS.md §5.1).
// A sub-thread is created only by a deliberate branch — never automatically,
// never merely because a tag was used (D11).
type BranchRecord struct {
	ParentThreadID  string    `json:"parent_thread_id"`
	AnchorMessageID string    `json:"anchor_message_id"` // the message that was branched from
	ChildThreadID   string    `json:"child_thread_id"`   // the new sub-thread's id (== its root's message id)
	CreatedAt       time.Time `json:"created_at"`
	CreatedBy       string    `json:"created_by"` // principal id who issued the branch
}

// Validate checks the branch record's required fields. A thread cannot branch
// to itself, and every id field plus the issuing principal must be present.
func (b BranchRecord) Validate() error {
	if b.ParentThreadID == "" {
		return errors.New("branch: parent_thread_id must be non-empty")
	}
	if b.AnchorMessageID == "" {
		return errors.New("branch: anchor_message_id must be non-empty")
	}
	if b.ChildThreadID == "" {
		return errors.New("branch: child_thread_id must be non-empty")
	}
	if b.ChildThreadID == b.ParentThreadID {
		return errors.New("branch: child_thread_id must differ from parent_thread_id (a thread cannot branch to itself)")
	}
	if b.CreatedBy == "" {
		return errors.New("branch: created_by must be non-empty")
	}
	return nil
}
