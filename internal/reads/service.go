// service.go — the read-cursor SERVICE: the transcript plus the cursor store,
// which together answer mark / who-read-what / unread (spec §3.2).
//
// The service owns every semantic decision the spec states and the store owns
// only durability:
//
//   - membership and clamping are decided against the session's OWN record
//     stream, so a cursor can never name a member or a seq the room does not
//     have;
//   - the rewind rule is decided here (a lower value is refused and the stored
//     value is unchanged) while MONOTONICITY lives in the store, so a lost
//     update under concurrency is absorbed rather than reported as a failure
//     (§2.2, §7);
//   - unread is always derived from `seq`, never from a timestamp.
package reads

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/crier-dev/crier/internal/session"
)

// Service composes Store with the transcript (§3.2).
type Service struct {
	// Repo is the session repository the transcript and roster are read from.
	Repo session.Repository
	// Cursors is the read-cursor store.
	Cursors Store
	// Now is the clock. Nil means time.Now().UTC(); it is a field so a test
	// can pin the instant a cursor was recorded.
	Now func() time.Time
}

// Mark records a read receipt (§2, §3.1) and returns the STORED cursor and the
// member's unread count after the advance.
//
// `through` and `messageIDs` are alternatives: message ids resolve to their
// max seq, an absent pair means "read through the latest", and supplying both
// is refused (READS_SEQ_INVALID). The response's `clamped` flag reports that
// the requested seq was past the end of the transcript (§2.3) — the caller is
// never told a larger number than the room holds.
func (s *Service) Mark(ctx context.Context, sessionID, memberType, memberID string, through int64, messageIDs []string) (Cursor, int64, error) {
	c, unread, _, err := s.MarkClamped(ctx, sessionID, memberType, memberID, through, messageIDs)
	return c, unread, err
}

// MarkClamped is Mark plus the §2.3 clamping flag the response must report.
// It exists so the HTTP surface never re-derives a decision the service made:
// one call decides the stored seq AND whether the request was clamped.
func (s *Service) MarkClamped(ctx context.Context, sessionID, memberType, memberID string, through int64, messageIDs []string) (Cursor, int64, bool, error) {
	if err := s.validate(memberType, memberID, through, messageIDs); err != nil {
		return Cursor{}, 0, false, err
	}
	st, err := s.load(ctx, sessionID)
	if err != nil {
		return Cursor{}, 0, false, err
	}
	if activeMember(st, memberType, memberID, s.now()) == nil {
		return Cursor{}, 0, false, fmt.Errorf("%w: %s %q is not an active participant of session %q",
			ErrMemberNotParticipant, memberType, memberID, sessionID)
	}

	last := lastSeq(st)
	want := through
	if len(messageIDs) > 0 {
		maxSeq, err := resolveMessageIDs(st, messageIDs)
		if err != nil {
			return Cursor{}, 0, false, err
		}
		want = maxSeq
	}
	if want == 0 && len(messageIDs) == 0 && through == 0 {
		want = last
	}
	target, clamped := clampSeq(want, last)

	existing, err := s.getCursor(ctx, sessionID, memberType, memberID)
	if err != nil {
		return Cursor{}, 0, false, err
	}
	if existing != nil && target < existing.ReadThroughSeq {
		return Cursor{}, 0, false, fmt.Errorf("%w: %s %q is at seq %d; %d is below it (a rewind is an operator action, §2.2)",
			ErrRewindRefused, memberType, memberID, existing.ReadThroughSeq, target)
	}
	if existing != nil && target == existing.ReadThroughSeq {
		return *existing, s.count(ctx, st, memberType, memberID, existing.ReadThroughSeq), clamped, nil
	}
	if existing == nil && target == 0 {
		return Cursor{SessionID: sessionID, MemberType: memberType, MemberID: memberID}, 0, clamped, nil
	}

	stored, err := s.Cursors.Advance(ctx, Cursor{
		SessionID:      sessionID,
		MemberType:     memberType,
		MemberID:       memberID,
		ReadThroughSeq: target,
		UpdatedAt:      s.now(),
	})
	if err != nil {
		return Cursor{}, 0, false, wrapStoreErr("advance cursor", err)
	}
	return stored, s.count(ctx, st, memberType, memberID, stored.ReadThroughSeq), clamped, nil
}

// State returns one cursor per ACTIVE participant plus the session's last seq
// (§3.1). A participant that never marked anything appears with seq 0 and a
// zero UpdatedAt — the caller renders `last_read_at: null` from it, never a
// fabricated timestamp. A member that was removed is excluded, and its cursor
// ROW is kept (§7: historical fact; a re-add resumes where it stopped).
func (s *Service) State(ctx context.Context, sessionID string) ([]Cursor, int64, error) {
	rows, last, err := s.StateRows(ctx, sessionID)
	if err != nil {
		return nil, 0, err
	}
	cursors := make([]Cursor, 0, len(rows))
	for _, row := range rows {
		cursors = append(cursors, row.Cursor)
	}
	return cursors, last, nil
}

// StateRows is State plus each participant's unread count, computed from ONE
// load of the transcript. The HTTP surface uses it so a room with N members
// costs one replay, not N.
func (s *Service) StateRows(ctx context.Context, sessionID string) ([]MemberState, int64, error) {
	st, err := s.load(ctx, sessionID)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.Cursors.List(ctx, sessionID)
	if err != nil {
		return nil, 0, wrapStoreErr("list cursors", err)
	}
	now := s.now()
	out := make([]MemberState, 0, len(st.Members))
	for _, m := range st.Members {
		// text: an inactive (removed) member has no read-state row (§7)
		if !m.ActiveAt(now) {
			continue
		}
		row := MemberState{Cursor: Cursor{
			SessionID:  sessionID,
			MemberType: string(m.MemberType),
			MemberID:   m.MemberID,
		}}
		if c := findCursor(rows, string(m.MemberType), m.MemberID); c != nil {
			row.Cursor = *c
			row.HasCursor = true
		}
		row.Unread = countUnread(st, row.Cursor.ReadThroughSeq, string(m.MemberType), m.MemberID)
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Cursor.MemberType != out[j].Cursor.MemberType {
			return out[i].Cursor.MemberType < out[j].Cursor.MemberType
		}
		return out[i].Cursor.MemberID < out[j].Cursor.MemberID
	})
	return out, lastSeq(st), nil
}

// Unread returns the messages that are unread FOR ONE MEMBER, in seq order
// (§2, §3.1): seq > cursor.seq, author ≠ member, optionally tag-scoped.
//
// The cursor is keyed by (session, member_type, member_id) in storage but this
// signature names only the member id (spec §3.2). Resolution is therefore
// explicit: the AGENT row for that id when one exists, else the principal row,
// else no cursor (everything is unread). A member id that names both an agent
// and a principal is the only case where the caller cannot say which it meant,
// and preferring the agent row is the same precedence the cursor lookup uses
// everywhere else in this package.
func (s *Service) Unread(ctx context.Context, sessionID, memberID string, tagFilter []string) ([]session.Message, error) {
	if memberID == "" {
		return nil, fmt.Errorf("%w: member_id is required", ErrMemberRequired)
	}
	st, err := s.load(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	cursors, err := s.Cursors.List(ctx, sessionID)
	if err != nil {
		return nil, wrapStoreErr("list cursors", err)
	}
	memberType := resolveMemberType(cursors, st, memberID)
	seq := cursorSeq(cursors, memberType, memberID)

	out := make([]session.Message, 0, len(st.Messages))
	for _, m := range st.Messages {
		if m.Seq <= seq {
			continue
		}
		if isOwnMessage(m, memberType, memberID) {
			continue
		}
		if !messageMatchesTags(m, tagFilter) {
			continue
		}
		out = append(out, *m)
	}
	return out, nil
}

// UnreadCount is the count form of Unread, for a caller that only needs the
// number (and for the mark response).
func (s *Service) UnreadCount(ctx context.Context, sessionID, memberID string, tagFilter []string) (int64, error) {
	msgs, err := s.Unread(ctx, sessionID, memberID, tagFilter)
	if err != nil {
		return 0, err
	}
	return int64(len(msgs)), nil
}

// ---------------------------------------------------------------------------
// Internals.
// ---------------------------------------------------------------------------

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) load(ctx context.Context, sessionID string) (*session.State, error) {
	if s.Repo == nil || s.Cursors == nil {
		return nil, fmt.Errorf("%w: the read-cursor service is not configured", ErrStoreError)
	}
	if sessionID == "" {
		return nil, fmt.Errorf("%w: session id is empty", session.ErrSessionNotFound)
	}
	st, err := s.Repo.Load(ctx, sessionID)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return nil, err
		}
		return nil, wrapStoreErr("load session", err)
	}
	return st, nil
}

func (s *Service) getCursor(ctx context.Context, sessionID, memberType, memberID string) (*Cursor, error) {
	c, err := s.Cursors.Get(ctx, sessionID, memberType, memberID)
	if err != nil {
		if errors.Is(err, ErrCursorNotFound) {
			return nil, nil
		}
		return nil, wrapStoreErr("get cursor", err)
	}
	return c, nil
}

// count is unread for a member whose cursor seq is already known.
func (s *Service) count(ctx context.Context, st *session.State, memberType, memberID string, seq int64) int64 {
	_ = ctx
	return countUnread(st, seq, memberType, memberID)
}

func countUnread(st *session.State, seq int64, memberType, memberID string) int64 {
	var n int64
	for _, m := range st.Messages {
		if m.Seq <= seq {
			continue
		}
		if isOwnMessage(m, memberType, memberID) {
			continue
		}
		n++
	}
	return n
}

func (s *Service) validate(memberType, memberID string, through int64, messageIDs []string) error {
	if memberType == "" || memberID == "" {
		return fmt.Errorf("%w: member_type %q, member_id %q", ErrMemberRequired, memberType, memberID)
	}
	if through < 0 {
		return fmt.Errorf("%w: negative read_through_seq %d", ErrSeqInvalid, through)
	}
	if through > 0 && len(messageIDs) > 0 {
		return fmt.Errorf("%w: read_through_seq and message_ids are alternatives, not both", ErrSeqInvalid)
	}
	return nil
}

// resolveMessageIDs maps the supplied ids to their max seq, refusing the FIRST
// id that does not belong to this session (READS_MESSAGE_NOT_IN_SESSION, 404):
// a foreign id is a client mistake about which room it is writing to, not a
// no-op.
func resolveMessageIDs(st *session.State, ids []string) (int64, error) {
	var max int64
	for _, id := range ids {
		m := st.Message(id)
		if m == nil {
			return 0, fmt.Errorf("%w: %q", ErrMessageNotInSession, id)
		}
		if m.Seq > max {
			max = m.Seq
		}
	}
	return max, nil
}

// resolveMemberType picks the member-type row a member id names: a stored
// cursor row first (agent preferred), then the roster (active members first,
// agent preferred), then agent — the type a send addresses by default.
func resolveMemberType(cursors []Cursor, st *session.State, memberID string) string {
	has := func(mt string) bool {
		for _, c := range cursors {
			if c.MemberID == memberID && c.MemberType == mt {
				return true
			}
		}
		return false
	}
	if has(string(session.MemberAgent)) {
		return string(session.MemberAgent)
	}
	if has(string(session.MemberPrincipal)) {
		return string(session.MemberPrincipal)
	}
	for _, want := range []session.MemberType{session.MemberAgent, session.MemberPrincipal} {
		for _, m := range st.Members {
			if m.MemberID == memberID && m.MemberType == want && m.RemovedAt == nil {
				return string(want)
			}
		}
	}
	return string(session.MemberAgent)
}
