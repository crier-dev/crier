# CRIER-READS.md — read cursors: who has read what, and what is unread

Status: **DRAFT v1 · 2026-11-03** · Owner: Bane · Row: **CRIER-READS-1**
Series: `specs/CHAT-SESSIONS.md` §4 (transcript, seq authority), `specs/CRIER-TAGS.md`
(tag-scoped unread), `specs/CHAT-PERMISSIONS.md` (who may mark for whom).

## 1. Purpose

Delivery is already tracked; reading is not. Add a per-member read cursor so a group inbox can
answer "who has read this", "how many are unread for agent X", and "which messages has nobody
opened" — the accounting a shared bus needs to be trustworthy.

A read receipt is a **claim by a member about its own progress**, never a delivery guarantee and
never an authority statement about another agent.

## 2. Semantics (normative)

- A cursor is **per (session, member)** and stores the highest `seq` that member has read.
  `Seq` is already the ordering authority in a session (CHAT-SESSIONS §3.1) — read state uses it
  and never `created_at`.
- `unread(member) = count(messages in session with seq > cursor.seq, author ≠ member)`. A member's
  own messages are never unread for that member.
- A cursor may advance; it **cannot** move backwards through `POST` (a rewind is an operator
  action with its own route, §2.4). `seq` equal to the current cursor is a no-op `200`.
- Marking `read_through_seq` beyond the session's last seq is clamped to the last seq, and the
  response reports the clamped value — never silently accepted as a larger number.
- Absent cursor = `seq: 0` = everything unread (except its own messages). Historical sessions
  therefore report honestly without a migration that invents state.

## 3. Interface

### 3.1 HTTP

```
POST /sessions/{id}/reads
{
  "member_type": "agent",
  "member_id": "alpha",
  "read_through_seq": 42,          // optional; absent = "read through the latest"
  "message_ids": ["m_1","m_2"]     // alternative to seq; server resolves to max(seq)
}
-> 200 {"member_type":"agent","member_id":"alpha","read_through_seq":42,"clamped":false,
        "unread":7,"updated_at":"..."}

GET /sessions/{id}/reads
-> 200 {
  "members": [
    {"member_type":"agent","member_id":"alpha","read_through_seq":42,"unread":7,
     "last_read_at":"2026-11-03T21:12:00Z"},
    {"member_type":"agent","member_id":"bravo","read_through_seq":0,"unread":49,
     "last_read_at":null}
  ],
  "last_seq": 49
}

GET /sessions/{id}/reads/unread?member_id=alpha&tag=build
-> 200 {"unread":[{"message_id":"m_50","seq":50,"tags":["build"],"author":{"agent":"ci-bot"}}],
        "count":1}
```

`GET /sessions/{id}/reads` returns every participant. A caller without
`session.read_state.all` sees only its own row — the rest are omitted, not zeroed.

### 3.2 Go

```go
package reads

type Cursor struct {
    SessionID      string    `json:"session_id"`
    MemberType     string    `json:"member_type"`
    MemberID       string    `json:"member_id"`
    ReadThroughSeq int64     `json:"read_through_seq"`
    UpdatedAt      time.Time `json:"updated_at"`
}

// Store persists cursors. One implementation per backend; handlers depend on this.
type Store interface {
    Get(ctx context.Context, sessionID, memberType, memberID string) (*Cursor, error)
    List(ctx context.Context, sessionID string) ([]Cursor, error)
    Advance(ctx context.Context, c Cursor) (Cursor, error) // monotonic; returns stored value
}

// Service composes Store with the transcript.
type Service struct {
    Repo     session.Repository
    Cursors  Store
    Now      func() time.Time
}

func (s *Service) Mark(ctx context.Context, sessionID, memberType, memberID string,
    through int64, messageIDs []string) (Cursor, int64, error) // cursor, unread, err
func (s *Service) State(ctx context.Context, sessionID string) ([]Cursor, int64, error)
func (s *Service) Unread(ctx context.Context, sessionID, memberID string, tagFilter []string) ([]session.Message, error)
```

`Advance` is the only writer and it is monotonic: implementation must read-modify-write under the
same lock the transcript append uses, or use `UPDATE ... WHERE read_through_seq < $1`.

## 4. Data Model

```sql
CREATE TABLE chat_read_cursors (
  session_id      TEXT        NOT NULL,
  member_type     TEXT        NOT NULL,
  member_id       TEXT        NOT NULL,
  read_through_seq BIGINT     NOT NULL DEFAULT 0,
  updated_at      TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (session_id, member_type, member_id)
);
CREATE INDEX chat_read_cursors_member ON chat_read_cursors (member_type, member_id);
```

JSONL store: `reads/{session_id}.jsonl`, one line per advance (append-only), last line wins at
replay. Compaction of that file is out of scope; the replay folds to the highest seq anyway.

## 5. Wiring

- `cmd/server/main.go`, beside the participant routes:
  `POST /sessions/{id}/reads`, `GET /sessions/{id}/reads`, `GET /sessions/{id}/reads/unread`.
- `chat.html`: a "seen by 3/5" affordance on each message group and an unread pill in the session
  list. Marking is automatic on scroll-to-bottom, debounced 2s, posting the highest visible seq.
- `internal/mcp`: `mark_read` and `unread_count` tools so an agent can answer "what do I owe you"
  without reading the whole transcript. (Tool names match the existing snake_case surface.)
- `docs/openapi.yaml` + README counts.

## 6. Error Catalog

| Code | Status | Condition |
|---|---|---|
| `READS_MEMBER_REQUIRED` | 400 | `member_type`/`member_id` empty. |
| `READS_MEMBER_NOT_PARTICIPANT` | 403 | The member is not in the session. |
| `READS_FOR_OTHER_MEMBER_DENIED` | 403 | A caller marking on behalf of another member without `session.read_state.write_all`. |
| `READS_SEQ_INVALID` | 400 | Negative, or both `read_through_seq` and `message_ids` supplied. |
| `READS_MESSAGE_NOT_IN_SESSION` | 404 | A supplied `message_ids` entry does not belong to this session. |
| `READS_REWIND_REFUSED` | 409 | `read_through_seq` below the stored cursor. |
| `READS_STORE_ERROR` | 500 | Backend failure. |

## 7. Edge Cases

- **Concurrent advances** from two processes: monotonic `UPDATE ... WHERE < $1` means the highest
  wins and neither errors.
- **Clamping:** `read_through_seq: 999999` on a 49-message session stores 49 and returns
  `"clamped": true`.
- **Own messages** never count toward own unread (verified per member, not globally).
- **Member removed from the session** keeps its cursor row (historical fact) but is excluded from
  `GET /reads`; re-adding resumes where it stopped.
- **New member joins** gets cursor 0 → all unread, which is correct: they were not there.
- **Empty session**: `last_seq: 0`, unread 0, no error.
- **Read state is not delivery**: a message can be `accepted` (delivered) and unread forever; the
  two numbers must never be conflated in UI copy or API field names.

## 8. Testing

1. `TestCursorAbsentMeansZero` — no row → unread == all non-own messages.
2. `TestAdvanceMonotonic` — 10 → 20 → 15 refused 409, stored stays 20.
3. `TestAdvanceClampsToLastSeq` — 999999 → 49, `clamped:true`.
4. `TestMessageIDsResolveToMaxSeq`.
5. `TestUnreadExcludesOwnMessages`.
6. `TestUnreadTagFilter` — with tags from CRIER-TAGS.
7. `TestReadsStatePermissionScoped` — a non-privileged caller sees one row only.
8. `TestConcurrentAdvanceHighestWins` — 20 goroutines, final seq is the max, no error.
9. `TestMemberRemovedKeepsCursorButHidden`.
10. `TestMarkOnBehalfDenied` — 403 without the permission.
11. Backend parity: JSONL / SQLite / Postgres produce identical `unread` for the same fixture.

## 9. Hilo Impact

Depends on: `internal/session` (transcript, `Seq`, participants), `internal/permissions`.
Depended on by: `chat.html` seen-by UI, MCP `unread_count`, and the group-inbox accounting the
owner asked for. No existing endpoint changes; `GET /sessions/{id}/reads` is additive.
