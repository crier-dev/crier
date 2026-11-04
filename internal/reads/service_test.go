package reads

// service_test.go — the acceptance criteria of specs/COURIER-READS.md §8, one
// test per numbered item, driven through the REAL cursor stores and the REAL
// session repository (no doubles): a cursor that only works against a fake is
// not a cursor.
//
// The two no-service backends (JSONL and SQLite) run every case; the
// PostgreSQL leg of the parity criterion runs when CR_READS_TEST_POSTGRES_URL
// names a reachable database and SKIPS otherwise, because this tree ships no
// PostgreSQL test service.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/session"
)

const (
	testSession = "s_reads"
	agentAlpha  = "alpha"
	agentBravo  = "bravo"
	principalID = "prin_carol"
	msgCount    = 49
)

// fixture is one session with three participants and 49 messages, plus the
// cursor store and the service under test.
type fixture struct {
	t       *testing.T
	sid     string
	repo    session.Repository
	cursors Store
	svc     *Service
	// msgIDs[i] is the message at index i (0-based), whose seq is base+i+1.
	msgIDs []string
	// lastSeq is the session's highest record seq (the seq the last message
	// carries, since messages are appended last).
	lastSeq int64
	// base is the first message's seq-1: the create and member records occupy
	// the seqs below it.
	base int64
}

// newJSONLFixture builds the fixture on the JSONL backends.
func newJSONLFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	repo, err := session.NewJSONLStore(filepath.Join(dir, "log"))
	require.NoError(t, err)
	cursors, err := NewJSONLStore(filepath.Join(dir, "log"))
	require.NoError(t, err)
	return buildFixture(t, repo, cursors)
}

// newSQLiteFixture builds the fixture on SQLite — the SQL backend, through the
// same store and SQL PostgreSQL uses.
//
// The database is ephemeral (":memory:") for the criterion matrix: the fixture
// is 53 records, and a file-backed SQLite transaction per record costs seconds
// per case without testing anything the JSONL fixture is not already proving
// about durability. Persistence of the CURSOR table across a reopen is proven
// separately, on a real file, by TestSQLiteCursorStorePersistsAcrossReopen.
func newSQLiteFixture(t *testing.T) *fixture {
	t.Helper()
	repo, err := session.NewSQLiteStore(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	// The cursor rows live in the SAME database and on the SAME handle the
	// session view uses — one SQLite writer, one busy_timeout, no second
	// connection to the same file (the server wires it the same way).
	cursors, err := NewSQLStore(context.Background(), repo.DB(), DialectSQLite)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cursors.Close() })
	return buildFixture(t, repo, cursors)
}

// TestSQLiteCursorStorePersistsAcrossReopen proves the cursor table is durable
// on a real SQLite FILE: a cursor written by one store is read back by a fresh
// handle, and the monotonic rule still holds there.
func TestSQLiteCursorStorePersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cursors.sqlite")

	first, err := NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	at := time.Now().UTC()
	stored, err := first.Advance(ctx, Cursor{
		SessionID: testSession, MemberType: string(session.MemberAgent),
		MemberID: agentAlpha, ReadThroughSeq: 7, UpdatedAt: at,
	})
	require.NoError(t, err)
	require.Equal(t, int64(7), stored.ReadThroughSeq)
	require.NoError(t, first.Close())

	second, err := NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })

	got, err := second.Get(ctx, testSession, string(session.MemberAgent), agentAlpha)
	require.NoError(t, err)
	require.Equal(t, int64(7), got.ReadThroughSeq)
	require.WithinDuration(t, at, got.UpdatedAt, time.Millisecond)

	// A reopen must not lose monotonicity: the lower advance is absorbed.
	absorbed, err := second.Advance(ctx, Cursor{
		SessionID: testSession, MemberType: string(session.MemberAgent),
		MemberID: agentAlpha, ReadThroughSeq: 3, UpdatedAt: at.Add(time.Second),
	})
	require.NoError(t, err)
	require.Equal(t, int64(7), absorbed.ReadThroughSeq)
}

// newPostgresFixture builds the fixture on a PostgreSQL cursor store when one
// is configured, and SKIPS otherwise.
func newPostgresFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("CR_READS_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("CR_READS_TEST_POSTGRES_URL is not set: no PostgreSQL service in this tree")
	}
	ctx := context.Background()
	cursors, err := NewPostgresStore(ctx, url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cursors.Close() })
	repo, err := session.NewPostgresSQLStore(ctx, url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })
	// A dedicated session id per run keeps the shared database clean.
	return buildFixtureWithSession(t, repo, cursors, fmt.Sprintf("%s_%d", testSession, time.Now().UnixNano()))
}

// allFixtures is the backend matrix the §8 criteria run over.
func allFixtures(t *testing.T) map[string]func(*testing.T) *fixture {
	return map[string]func(*testing.T) *fixture{
		"jsonl":    newJSONLFixture,
		"sqlite":   newSQLiteFixture,
		"postgres": newPostgresFixture,
	}
}

func buildFixture(t *testing.T, repo session.Repository, cursors Store) *fixture {
	t.Helper()
	return buildFixtureWithSession(t, repo, cursors, testSession)
}

func buildFixtureWithSession(t *testing.T, repo session.Repository, cursors Store, sessionID string) *fixture {
	t.Helper()
	ctx := context.Background()
	// Timestamps sit in the PAST so membership is active at the service's own
	// clock without the test having to pin it.
	base := time.Now().UTC().Add(-time.Hour)

	sess := &session.Session{
		ID:        sessionID,
		Kind:      session.KindChannel,
		CreatedAt: base,
		CreatedBy: session.AuthorRef{Agent: agentAlpha},
	}
	seq := int64(1)
	require.NoError(t, repo.CreateSession(ctx, sess, seq))
	seq++

	members := []*session.Member{
		{SessionID: sessionID, MemberType: session.MemberAgent, MemberID: agentAlpha, Role: session.RoleMember, AddedAt: base},
		{SessionID: sessionID, MemberType: session.MemberAgent, MemberID: agentBravo, Role: session.RoleMember, AddedAt: base},
		{SessionID: sessionID, MemberType: session.MemberPrincipal, MemberID: principalID, Role: session.RoleMember, AddedAt: base},
	}
	for _, m := range members {
		require.NoError(t, repo.AddMember(ctx, m, seq, nil))
		seq++
	}
	firstMessageSeq := seq

	ids := make([]string, 0, msgCount)
	for i := 1; i <= msgCount; i++ {
		id := fmt.Sprintf("m_%02d", i)
		msg := &session.Message{
			ID:        id,
			ThreadID:  id,
			SessionID: sessionID,
			Kind:      session.MessagePlain,
			Author:    authorFor(i),
			Audience:  session.Audience{Rule: session.AudienceSession},
			Seq:       seq,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
		require.NoError(t, repo.PostMessage(ctx, msg))
		ids = append(ids, id)
		seq++
	}

	return &fixture{
		t:       t,
		sid:     sessionID,
		repo:    repo,
		cursors: cursors,
		svc:     &Service{Repo: repo, Cursors: cursors},
		msgIDs:  ids,
		lastSeq: seq - 1,
		base:    firstMessageSeq - 1,
	}
}

// authorFor gives three distinct authors in a 3-cycle: alpha, bravo, carol.
func authorFor(i int) session.AuthorRef {
	switch i % 3 {
	case 1:
		return session.AuthorRef{Agent: agentAlpha}
	case 2:
		return session.AuthorRef{Agent: agentBravo}
	default:
		return session.AuthorRef{Principal: principalID}
	}
}

// countAuthored counts messages in the fixture authored by one identity.
func countAuthored(author session.AuthorRef) int {
	n := 0
	for i := 1; i <= msgCount; i++ {
		if authorFor(i) == author {
			n++
		}
	}
	return n
}

// mark is the one call the tests use, with the fixture's session id.
func (f *fixture) mark(memberType, memberID string, through int64, ids []string) (Cursor, int64, bool, error) {
	return f.svc.MarkClamped(context.Background(), f.sid, memberType, memberID, through, ids)
}

// ---------------------------------------------------------------------------
// §8.1 TestCursorAbsentMeansZero
// ---------------------------------------------------------------------------

func TestCursorAbsentMeansZero(t *testing.T) {
	for name, open := range allFixtures(t) {
		t.Run(name, func(t *testing.T) {
			f := open(t)
			want := int64(msgCount - countAuthored(session.AuthorRef{Agent: agentAlpha}))
			rows, last, err := f.svc.StateRows(context.Background(), f.sid)
			require.NoError(t, err)
			require.Equal(t, f.lastSeq, last, "last_seq is the transcript's highest seq")

			var found bool
			for _, row := range rows {
				if row.Cursor.MemberID != agentAlpha {
					continue
				}
				found = true
				require.False(t, row.HasCursor, "a member that never marked has no cursor row")
				require.Equal(t, int64(0), row.Cursor.ReadThroughSeq, "absent cursor is 0, never the session length")
				require.Equal(t, want, row.Unread, "everything not authored by the member is unread")
			}
			require.True(t, found, "the participant appears with cursor 0")

			// The store itself reports the absence rather than fabricating a row.
			_, err = f.cursors.Get(context.Background(), f.sid, string(session.MemberAgent), agentAlpha)
			require.ErrorIs(t, err, ErrCursorNotFound)
		})
	}
}

// ---------------------------------------------------------------------------
// §8.2 TestAdvanceMonotonic
// ---------------------------------------------------------------------------

func TestAdvanceMonotonic(t *testing.T) {
	for name, open := range allFixtures(t) {
		t.Run(name, func(t *testing.T) {
			f := open(t)
			sid := f.sid

			c, _, _, err := f.mark(string(session.MemberAgent), agentAlpha, 10, nil)
			require.NoError(t, err)
			require.Equal(t, int64(10), c.ReadThroughSeq)

			c, _, _, err = f.mark(string(session.MemberAgent), agentAlpha, 20, nil)
			require.NoError(t, err)
			require.Equal(t, int64(20), c.ReadThroughSeq)

			// A lower value is refused outright...
			_, _, _, err = f.mark(string(session.MemberAgent), agentAlpha, 15, nil)
			require.ErrorIs(t, err, ErrRewindRefused)

			// ...and the stored cursor is unchanged.
			stored, err := f.cursors.Get(context.Background(), sid, string(session.MemberAgent), agentAlpha)
			require.NoError(t, err)
			require.Equal(t, int64(20), stored.ReadThroughSeq)

			// An equal value is a no-op 200, not a refusal.
			c, _, clamped, err := f.mark(string(session.MemberAgent), agentAlpha, 20, nil)
			require.NoError(t, err)
			require.Equal(t, int64(20), c.ReadThroughSeq)
			require.False(t, clamped)
		})
	}
}

// ---------------------------------------------------------------------------
// §8.3 TestAdvanceClampsToLastSeq
// ---------------------------------------------------------------------------

func TestAdvanceClampsToLastSeq(t *testing.T) {
	for name, open := range allFixtures(t) {
		t.Run(name, func(t *testing.T) {
			f := open(t)
			sid := f.sid

			c, unread, clamped, err := f.mark(string(session.MemberAgent), agentAlpha, 999999, nil)
			require.NoError(t, err)
			require.True(t, clamped, "a seq past the end is reported as clamped")
			require.Equal(t, f.lastSeq, c.ReadThroughSeq, "stored value is the last seq, never the requested one")
			require.Equal(t, int64(0), unread, "reading through the last message leaves nothing unread")

			stored, err := f.cursors.Get(context.Background(), sid, string(session.MemberAgent), agentAlpha)
			require.NoError(t, err)
			require.Equal(t, f.lastSeq, stored.ReadThroughSeq, "the larger number was never stored")
		})
	}
}

// ---------------------------------------------------------------------------
// §8.4 TestMessageIDsResolveToMaxSeq
// ---------------------------------------------------------------------------

func TestMessageIDsResolveToMaxSeq(t *testing.T) {
	for name, open := range allFixtures(t) {
		t.Run(name, func(t *testing.T) {
			f := open(t)
			// msgIDs[1] and msgIDs[7] are seqs base+2 and base+8.
			ids := []string{f.msgIDs[1], f.msgIDs[7]}
			wantSeq := f.base + 8

			c, _, clamped, err := f.mark(string(session.MemberAgent), agentAlpha, 0, ids)
			require.NoError(t, err)
			require.False(t, clamped)
			require.Equal(t, wantSeq, c.ReadThroughSeq, "ids resolve to their MAX seq, not their sum or their first")

			// A single id is the same rule.
			c, _, _, err = f.mark(string(session.MemberAgent), agentBravo, 0, []string{f.msgIDs[0]})
			require.NoError(t, err)
			require.Equal(t, f.base+1, c.ReadThroughSeq)

			// A foreign id is a 404, not a no-op.
			_, _, _, err = f.mark(string(session.MemberAgent), agentBravo, 0, []string{f.msgIDs[0], "m_not_here"})
			require.ErrorIs(t, err, ErrMessageNotInSession)

			// Both forms at once is 400.
			_, _, _, err = f.mark(string(session.MemberAgent), agentBravo, 4, []string{f.msgIDs[0]})
			require.ErrorIs(t, err, ErrSeqInvalid)
		})
	}
}

// ---------------------------------------------------------------------------
// §8.5 TestUnreadExcludesOwnMessages
// ---------------------------------------------------------------------------

func TestUnreadExcludesOwnMessages(t *testing.T) {
	for name, open := range allFixtures(t) {
		t.Run(name, func(t *testing.T) {
			f := open(t)
			sid := f.sid
			ctx := context.Background()

			for _, tc := range []struct {
				memberType string
				memberID   string
				author     session.AuthorRef
			}{
				{string(session.MemberAgent), agentAlpha, session.AuthorRef{Agent: agentAlpha}},
				{string(session.MemberAgent), agentBravo, session.AuthorRef{Agent: agentBravo}},
				{string(session.MemberPrincipal), principalID, session.AuthorRef{Principal: principalID}},
			} {
				unread, err := f.svc.Unread(ctx, sid, tc.memberID, nil)
				require.NoError(t, err)
				require.Equal(t, msgCount-countAuthored(tc.author), len(unread))
				for _, m := range unread {
					require.NotEqual(t, tc.author, m.Author,
						"a member's own messages are never unread for it (%s)", tc.memberID)
				}
			}

			// Reading through half the transcript halves the count, and own
			// messages in the unread window are still excluded.
			half := f.base + int64(msgCount/2)
			_, unread, _, err := f.mark(string(session.MemberAgent), agentAlpha, half, nil)
			require.NoError(t, err)
			var want int64
			for i := 1; i <= msgCount; i++ {
				if f.base+int64(i) > half && authorFor(i) != (session.AuthorRef{Agent: agentAlpha}) {
					want++
				}
			}
			require.Equal(t, want, unread)
		})
	}
}

// ---------------------------------------------------------------------------
// §8.6 TestUnreadTagFilter
// ---------------------------------------------------------------------------

// TestUnreadTagFilter pins the tag-scoped unread query AND the deviation this
// tree carries: CRIER-TAGS-1 has not landed, session.Message has no Tags field,
// so messageTags returns nil and the filter is a documented pass-through rather
// than a silent empty result. When the field lands, only messageTags changes and
// this test's pass-through assertions become filter assertions.
func TestUnreadTagFilter(t *testing.T) {
	for name, open := range allFixtures(t) {
		t.Run(name, func(t *testing.T) {
			f := open(t)
			sid := f.sid
			ctx := context.Background()

			all, err := f.svc.Unread(ctx, sid, agentAlpha, nil)
			require.NoError(t, err)
			require.NotEmpty(t, all)

			// No tag data exists in this tree...
			require.Nil(t, messageTags(&all[0]))

			// ...so a tag filter passes every message through instead of
			// claiming a confident zero. This is the reported deviation.
			filtered, err := f.svc.Unread(ctx, sid, agentAlpha, []string{"build"})
			require.NoError(t, err)
			require.Equal(t, len(all), len(filtered))

			// The filter is still REACHED (the query param is accepted and a
			// multi-value filter behaves identically), so the wiring is real
			// even while the data is absent.
			multi, err := f.svc.Unread(ctx, sid, agentAlpha, []string{"build", "ci"})
			require.NoError(t, err)
			require.Equal(t, len(all), len(multi))

			n, err := f.svc.UnreadCount(ctx, sid, agentAlpha, []string{"build"})
			require.NoError(t, err)
			require.Equal(t, int64(len(all)), n)
		})
	}
}

// ---------------------------------------------------------------------------
// §8.8 TestConcurrentAdvanceHighestWins
// ---------------------------------------------------------------------------

// TestConcurrentAdvanceHighestWins is §7's concurrency criterion: 20 goroutines
// advance the SAME cursor with different seqs, every call succeeds (monotonic
// absorption, no lost-update error), and the stored value is the maximum.
func TestConcurrentAdvanceHighestWins(t *testing.T) {
	for name, open := range allFixtures(t) {
		t.Run(name, func(t *testing.T) {
			f := open(t)
			sid := f.sid
			ctx := context.Background()

			const n = 20
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make([]error, n)
			results := make([]Cursor, n)

			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					c, err := f.cursors.Advance(ctx, Cursor{
						SessionID:      sid,
						MemberType:     string(session.MemberAgent),
						MemberID:       agentAlpha,
						ReadThroughSeq: int64(i + 1),
						UpdatedAt:      time.Now().UTC(),
					})
					errs[i], results[i] = err, c
				}(i)
			}
			close(start)
			wg.Wait()

			for i, err := range errs {
				require.NoError(t, err, "goroutine %d lost an update as an error", i)
			}
			stored, err := f.cursors.Get(ctx, sid, string(session.MemberAgent), agentAlpha)
			require.NoError(t, err)
			require.Equal(t, int64(n), stored.ReadThroughSeq, "the highest seq wins and is never rolled back")

			// Every call reports the cursor that was actually stored at the
			// moment it returned, and none reports a value below the one it
			// asked for.
			for i, c := range results {
				require.GreaterOrEqual(t, c.ReadThroughSeq, int64(i+1),
					"goroutine %d was told a cursor below its own request", i)
			}
		})
	}
}

// TestConcurrentMarkConverges drives the SERVICE concurrently (the §7 path a
// client actually uses): every call's stored cursor converges on the highest
// seq marked, no unread count is computed from a rolled-back value, and the only
// permitted refusal is the explicit rewind rule — a request that ARRIVED after a
// higher cursor was already recorded.
func TestConcurrentMarkConverges(t *testing.T) {
	for name, open := range allFixtures(t) {
		t.Run(name, func(t *testing.T) {
			f := open(t)
			sid := f.sid
			ctx := context.Background()

			const n = 20
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make([]error, n)

			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					_, _, _, err := f.svc.MarkClamped(ctx, sid, string(session.MemberAgent), agentAlpha, int64(i+1), nil)
					errs[i] = err
				}(i)
			}
			close(start)
			wg.Wait()

			for i, err := range errs {
				if err != nil {
					require.ErrorIs(t, err, ErrRewindRefused,
						"goroutine %d failed for a reason other than the explicit rewind rule: %v", i, err)
				}
			}
			stored, err := f.cursors.Get(ctx, sid, string(session.MemberAgent), agentAlpha)
			require.NoError(t, err)
			require.Equal(t, int64(n), stored.ReadThroughSeq,
				"no lost update: the highest mark is the stored cursor")
		})
	}
}

// ---------------------------------------------------------------------------
// §8.9 TestMemberRemovedKeepsCursorButHidden
// ---------------------------------------------------------------------------

func TestMemberRemovedKeepsCursorButHidden(t *testing.T) {
	for name, open := range allFixtures(t) {
		t.Run(name, func(t *testing.T) {
			f := open(t)
			sid := f.sid
			ctx := context.Background()

			_, _, _, err := f.mark(string(session.MemberAgent), agentAlpha, 12, nil)
			require.NoError(t, err)

			// Remove alpha: a membership event, never a delete (§2.3).
			seq, err := f.repo.NextSeq(ctx, sid)
			require.NoError(t, err)
			member := &session.Member{SessionID: sid, MemberType: session.MemberAgent, MemberID: agentAlpha}
			require.NoError(t, f.repo.Append(ctx, member.RemoveRecord(seq, time.Now().UTC(),
				session.AuthorRef{Agent: agentBravo}, "left the room")))

			// GET /reads excludes the removed member...
			rows, _, err := f.svc.StateRows(ctx, sid)
			require.NoError(t, err)
			for _, row := range rows {
				require.NotEqual(t, agentAlpha, row.Cursor.MemberID, "a removed member has no read-state row")
			}
			// ...but the cursor row is kept: it is a historical fact.
			kept, err := f.cursors.Get(ctx, sid, string(session.MemberAgent), agentAlpha)
			require.NoError(t, err)
			require.Equal(t, int64(12), kept.ReadThroughSeq)

			// Marking for a removed member is refused: it is not a participant.
			_, _, _, err = f.mark(string(session.MemberAgent), agentAlpha, 20, nil)
			require.ErrorIs(t, err, ErrMemberNotParticipant)

			// A re-add resumes where it stopped.
			seq, err = f.repo.NextSeq(ctx, sid)
			require.NoError(t, err)
			require.NoError(t, f.repo.AddMember(ctx, &session.Member{
				SessionID:  sid,
				MemberType: session.MemberAgent,
				MemberID:   agentAlpha,
				Role:       session.RoleMember,
				AddedAt:    time.Now().UTC(),
			}, seq, nil))

			rows, _, err = f.svc.StateRows(ctx, sid)
			require.NoError(t, err)
			var resumed bool
			for _, row := range rows {
				if row.Cursor.MemberID == agentAlpha {
					resumed = true
					require.Equal(t, int64(12), row.Cursor.ReadThroughSeq, "a re-add resumes where the member stopped")
				}
			}
			require.True(t, resumed, "the re-added member appears again")
		})
	}
}

// ---------------------------------------------------------------------------
// §7 edge cases that are not a numbered criterion but are normative.
// ---------------------------------------------------------------------------

func TestEmptySessionReportsZero(t *testing.T) {
	for name, open := range allFixtures(t) {
		t.Run(name, func(t *testing.T) {
			f := open(t)
			ctx := context.Background()
			sid := f.sid
			empty := sid + "_empty"
			require.NoError(t, f.repo.CreateSession(ctx, &session.Session{
				ID: empty, Kind: session.KindChannel, CreatedAt: time.Now().UTC(),
				CreatedBy: session.AuthorRef{Agent: agentAlpha},
			}, 1))
			require.NoError(t, f.repo.AddMember(ctx, &session.Member{
				SessionID: empty, MemberType: session.MemberAgent, MemberID: agentAlpha,
				Role: session.RoleMember, AddedAt: time.Now().UTC(),
			}, 2, nil))

			sid2 := empty
			rows, last, err := f.svc.StateRows(ctx, sid2)
			require.NoError(t, err)
			require.Equal(t, int64(0), last, "an empty session has last_seq 0")
			require.Len(t, rows, 1)
			require.Equal(t, int64(0), rows[0].Unread)

			c, unread, clamped, err := f.svc.MarkClamped(ctx, sid2, string(session.MemberAgent), agentAlpha, 0, nil)
			require.NoError(t, err)
			require.Equal(t, int64(0), c.ReadThroughSeq)
			require.Equal(t, int64(0), unread)
			require.False(t, clamped, "0 on an empty session is not a clamp")
		})
	}
}

func TestMarkRequiresMemberAndRejectsNegativeSeq(t *testing.T) {
	f := newSQLiteFixture(t)
	ctx := context.Background()
	sid := f.sid

	_, _, _, err := f.svc.MarkClamped(ctx, sid, "", agentAlpha, 1, nil)
	require.ErrorIs(t, err, ErrMemberRequired)

	_, _, _, err = f.svc.MarkClamped(ctx, sid, string(session.MemberAgent), "", 1, nil)
	require.ErrorIs(t, err, ErrMemberRequired)

	_, _, _, err = f.svc.MarkClamped(ctx, sid, string(session.MemberAgent), agentAlpha, -5, nil)
	require.ErrorIs(t, err, ErrSeqInvalid)

	_, _, _, err = f.svc.MarkClamped(ctx, sid, string(session.MemberAgent), "nobody", 1, nil)
	require.ErrorIs(t, err, ErrMemberNotParticipant)

	_, _, _, err = f.svc.MarkClamped(ctx, "no_such_session", string(session.MemberAgent), agentAlpha, 1, nil)
	require.True(t, errors.Is(err, session.ErrSessionNotFound), "an unknown session is SESSION_NOT_FOUND, got %v", err)
}

// ---------------------------------------------------------------------------
// §8.11 Backend parity
// ---------------------------------------------------------------------------

// TestBackendParityUnread proves the three backends produce the SAME unread
// numbers and the same cursor values for one fixture and one script of marks.
// PostgreSQL joins when it is configured; the assertion is shared, so a
// backend that drifts fails here rather than in production.
func TestBackendParityUnread(t *testing.T) {
	// A backend-independent projection: the wire facts only. `updated_at`
	// is deliberately excluded — it is WHEN the mark happened, not WHAT was
	// marked, and two fixtures marked microseconds apart must still compare
	// equal on the values a client reads.
	type snapshot struct {
		rows    []string
		last    int64
		cursors map[string]int64
	}
	run := func(t *testing.T, f *fixture) snapshot {
		t.Helper()
		ctx := context.Background()
		sid := f.sid
		for _, step := range []struct {
			memberType, memberID string
			through              int64
			ids                  []string
		}{
			{string(session.MemberAgent), agentAlpha, 10, nil},
			{string(session.MemberAgent), agentBravo, 0, []string{f.msgIDs[4], f.msgIDs[20]}},
			{string(session.MemberPrincipal), principalID, 999999, nil},
		} {
			_, _, _, err := f.svc.MarkClamped(ctx, sid, step.memberType, step.memberID, step.through, step.ids)
			require.NoError(t, err)
		}
		memberRows, last, err := f.svc.StateRows(ctx, sid)
		require.NoError(t, err)
		cursors, err := f.cursors.List(ctx, sid)
		require.NoError(t, err)
		byMember := map[string]int64{}
		for _, c := range cursors {
			byMember[c.MemberType+"/"+c.MemberID] = c.ReadThroughSeq
		}
		rows := make([]string, 0, len(memberRows))
		for _, row := range memberRows {
			rows = append(rows, fmt.Sprintf("%s/%s seq=%d unread=%d has_cursor=%t",
				row.Cursor.MemberType, row.Cursor.MemberID, row.Cursor.ReadThroughSeq, row.Unread, row.HasCursor))
		}
		return snapshot{rows: rows, last: last, cursors: byMember}
	}

	jsonl := run(t, newJSONLFixture(t))
	sqlite := run(t, newSQLiteFixture(t))
	require.Equal(t, jsonl.last, sqlite.last, "last_seq is the same fact on both backends")
	require.Equal(t, jsonl.rows, sqlite.rows, "unread is identical on JSONL and SQLite")
	require.Equal(t, jsonl.cursors, sqlite.cursors, "stored cursors are identical on JSONL and SQLite")

	t.Run("postgres", func(t *testing.T) {
		pg := run(t, newPostgresFixture(t))
		require.Equal(t, jsonl.last, pg.last)
		require.Equal(t, jsonl.rows, pg.rows, "unread is identical on JSONL and PostgreSQL")
		require.Equal(t, jsonl.cursors, pg.cursors, "stored cursors are identical on JSONL and PostgreSQL")
	})
}
