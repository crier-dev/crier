// Package reads implements per-member read cursors — "who has read what, and
// what is unread" — per specs/COURIER-READS.md (CRIER-READS-1).
//
// A read receipt is a CLAIM BY A MEMBER ABOUT ITS OWN PROGRESS. It is never a
// delivery guarantee and never an authority statement about another agent:
// §7 says a message can be `accepted` (delivered) and unread forever, so the
// two numbers must never be conflated in an API field name.
//
// Three rules are load-bearing here and are stated in code rather than implied:
//
//   - The ordering authority is the transcript's `seq` (CHAT-SESSIONS §3.1),
//     never `created_at`.
//   - A cursor may ADVANCE and never move backwards through Mark. A rewind is
//     refused (ErrRewindRefused), not silently ignored, and the stored value is
//     left exactly as it was.
//   - An ABSENT cursor means 0 — everything unread except the member's own
//     messages — so a historical session reports honestly with no migration
//     that invents state.
//
// The package depends on internal/session (the transcript and the participant
// roster) and on internal/permissions (the ACL the read-state gates consult).
// Nothing in internal/session imports it, which is why the HTTP surface below
// lives here too: the handlers need session.State, so the dependency runs one
// way only.
package reads

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crier-dev/crier/internal/session"
)

// Cursor is one member's read position in one session (spec §3.2/§4): the
// highest `seq` that member has read.
type Cursor struct {
	SessionID      string    `json:"session_id"`
	MemberType     string    `json:"member_type"`
	MemberID       string    `json:"member_id"`
	ReadThroughSeq int64     `json:"read_through_seq"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Store persists cursors. One implementation per backend; handlers depend on
// this (spec §3.2).
//
// Advance is the ONLY writer and it is monotonic: a value below the stored one
// is absorbed and the STORED cursor is returned, so two concurrent advances
// converge on the highest seq and neither errors (§2.2, §7).
type Store interface {
	Get(ctx context.Context, sessionID, memberType, memberID string) (*Cursor, error)
	List(ctx context.Context, sessionID string) ([]Cursor, error)
	Advance(ctx context.Context, c Cursor) (Cursor, error) // monotonic; returns stored value
}

// Closer is implemented by stores that own an open handle. It is deliberately
// NOT part of Store: the spec fixes that interface at three methods, and a
// JSONL store has nothing to close while a SQL store may be handed a borrowed
// handle it must not close.
type Closer interface{ Close() error }

// Errors the service and the handlers map to the §6 error catalog. They are
// sentinels so a caller branches with errors.Is rather than parsing prose; the
// wrapped detail names the member, the seq or the message that failed.
var (
	// ErrCursorNotFound is returned by a Store's Get when the member has no
	// cursor row. It is a normal state, not a failure (§2.5: absent = 0).
	ErrCursorNotFound = errors.New("read cursor not found")
	// ErrMemberRequired is READS_MEMBER_REQUIRED (400): member_type or
	// member_id is empty.
	ErrMemberRequired = errors.New("member_type and member_id are required")
	// ErrMemberNotParticipant is READS_MEMBER_NOT_PARTICIPANT (403): the
	// member is not (or is no longer) in the session.
	ErrMemberNotParticipant = errors.New("member is not a participant of the session")
	// ErrSeqInvalid is READS_SEQ_INVALID (400): a negative seq, or both
	// read_through_seq and message_ids supplied.
	ErrSeqInvalid = errors.New("read_through_seq is invalid")
	// ErrMessageNotInSession is READS_MESSAGE_NOT_IN_SESSION (404): a supplied
	// message id does not belong to this session.
	ErrMessageNotInSession = errors.New("message is not in this session")
	// ErrRewindRefused is READS_REWIND_REFUSED (409): the requested seq is
	// below the stored cursor. The stored value is never changed by it.
	ErrRewindRefused = errors.New("read cursor cannot move backwards")
	// ErrStoreError is READS_STORE_ERROR (500): a backend failure.
	ErrStoreError = errors.New("read cursor store error")
)

// Permission is one of the two read-state permissions the spec names (§3.1,
// §6). The strings are the spec's spelling, verbatim, because an operator
// configures them and an audit line quotes them.
type Permission string

const (
	// PermReadAll — session.read_state.all — lets a caller see every
	// participant's row on GET /sessions/{id}/reads.
	PermReadAll Permission = "session.read_state.all"
	// PermWriteAll — session.read_state.write_all — lets a caller mark a read
	// cursor on behalf of ANOTHER member (§3.1, error catalog §6).
	PermWriteAll Permission = "session.read_state.write_all"
)

// MemberState is one participant's read state as GET /sessions/{id}/reads
// serves it: the cursor (possibly absent, hence seq 0) plus the unread count
// derived from it. It is the service-side shape; the wire shape adds the
// nullable `last_read_at` the JSON contract requires.
type MemberState struct {
	Cursor Cursor
	// HasCursor is false for a participant that never marked anything, so a
	// caller can tell "read nothing yet" from "marked seq 0".
	HasCursor bool
	// Unread counts messages with seq > cursor.seq whose author is not the
	// member (§2).
	Unread int64
}

// messageTags returns the tags a message carries.
//
// COURIER-TAGS.md (CRIER-TAGS-1) has NOT landed in this tree: session.Message
// carries no Tags field, so there is no tag data to read and this returns nil.
// It exists so the tag filter has exactly ONE place to become real: when the
// field lands, this function is the only change needed and messageMatchesTags
// starts filtering instead of passing everything through. See the package note
// in Unread and the commit body for the reported deviation.
func messageTags(*session.Message) []string { return nil }

// messageMatchesTags is the tag-scoped half of the unread query (§3.1:
// `?tag=build`). An empty filter matches everything. A filter with no TAG DATA
// available also matches everything — deliberately: CRIER-TAGS-1 has not landed
// here, so filtering would silently return zero unread for a query the caller
// believes is answerable. Passing through is the honest fallback and is
// reported as a deviation rather than presented as working.
func messageMatchesTags(m *session.Message, filter []string) bool {
	if len(filter) == 0 {
		return true
	}
	tags := messageTags(m)
	if tags == nil {
		return true
	}
	for _, want := range filter {
		for _, have := range tags {
			if have == want {
				return true
			}
		}
	}
	return false
}

// isOwnMessage reports whether a message was authored by the member, typed so
// an agent id and a principal id that happen to collide are not the same
// author. An agent speaking AS another agent (as_agent) is that agent's own
// message: §2 says a member's own messages are never unread for that member.
func isOwnMessage(m *session.Message, memberType, memberID string) bool {
	switch session.MemberType(memberType) {
	case session.MemberAgent:
		return m.Author.Agent == memberID || m.Author.AsAgent == memberID
	case session.MemberPrincipal:
		return m.Author.Principal == memberID
	}
	// An unknown member type has no author shape to match; treat an exact
	// agent/principal id match as own so the default stays conservative (it
	// never hides someone else's message from a member).
	return m.Author.Agent == memberID || m.Author.Principal == memberID || m.Author.AsAgent == memberID
}

// memberKey is the (type, id) identity of a member — the cursor's primary key.
type memberKey struct {
	memberType string
	memberID   string
}

// findCursor returns the cursor for a member in a cursor set, or nil.
func findCursor(cursors []Cursor, memberType, memberID string) *Cursor {
	for i := range cursors {
		if cursors[i].MemberType == memberType && cursors[i].MemberID == memberID {
			return &cursors[i]
		}
	}
	return nil
}

// cursorSeq is a cursor set's seq for a member, 0 when absent (§2.5).
func cursorSeq(cursors []Cursor, memberType, memberID string) int64 {
	if c := findCursor(cursors, memberType, memberID); c != nil {
		return c.ReadThroughSeq
	}
	return 0
}

// lastSeq is the session's highest message seq — the seq the last message
// carries, 0 for an empty session (§7 "Empty session: last_seq: 0, no error").
func lastSeq(st *session.State) int64 {
	var max int64
	for _, m := range st.Messages {
		if m.Seq > max {
			max = m.Seq
		}
	}
	return max
}

// activeMember returns the participant row for a member that is present NOW, or
// nil. A removed member is not a participant (it keeps its cursor row but is
// excluded from the read-state surface, §7), and a re-add makes it one again.
func activeMember(st *session.State, memberType, memberID string, now time.Time) *session.Member {
	for _, m := range st.Members {
		if string(m.MemberType) == memberType && m.MemberID == memberID && m.ActiveAt(now) {
			return m
		}
	}
	return nil
}

// clampSeq clamps a requested seq to a session's last seq (§2.3): a value past
// the end is stored as the last seq and reported as clamped, never accepted as
// a larger number.
func clampSeq(want, last int64) (seq int64, clamped bool) {
	if want > last {
		return last, true
	}
	return want, false
}

// wrapStoreErr gives a backend failure the READS_STORE_ERROR identity while
// keeping the backend's own error reachable for a log line.
func wrapStoreErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrStoreError) {
		return err
	}
	return fmt.Errorf("%w: %s: %v", ErrStoreError, op, err)
}
