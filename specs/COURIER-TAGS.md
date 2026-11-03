# COURIER-TAGS.md — message tags, the category vocabulary, and filtered reads

Status: **DRAFT v1 · 2026-11-03** · Owner: Bane · Row: **CRIER-TAGS-1**
Series: `specs/COURIER-INGRESS.md` (writes tags), `specs/COURIER-READS.md` (filters read state),
`specs/CHAT-SESSIONS.md` §4 (message kinds — a tag is NOT a kind).

## 1. Purpose

Let a message carry zero or more tags so a reader can ask "show me the urgent ones", "what
categories exist", and "the build lane's messages" without a full-text search — Gmail
categories, not a new addressing grammar.

A tag classifies; it never routes and never grants. Addressing still uses the existing
`@agent` / group / capability forms (CHAT-ADDRESSING), and a message's `kind` is unchanged.

## 2. Interface

### 2.1 Grammar (normative)

```
tag        := segment
segment    := [a-z0-9] ([a-z0-9_-]{0,62} [a-z0-9])?
namespace  := segment ":" segment          ; at most ONE colon, both sides valid segments
```

- lowercase ASCII, digits, `_`, `-`
- 1 to 64 characters total
- max **16 tags per message**
- no leading/trailing `-` or `_`
- a tag matching a reserved prefix `sys:` is accepted only from an operator principal

A caller sending `"Urgent"` gets `400 INGRESS_TAGS_INVALID` — tags are **not** silently
lowercased, because two spellings of one category is the drift this feature exists to prevent.

### 2.2 Message field

```go
// internal/session/session.go, on Message
// Tags classifies the message (COURIER-TAGS §2.1). Empty on historical records;
// a reader must treat absent and empty identically.
Tags []string `json:"tags,omitempty"`
```

Sort order is preserved as supplied; the store deduplicates exact repeats.

### 2.3 Vocabulary

```
GET /tags                         -> {"tags":[{"tag":"build","count":412,"last_used":"..."}]}
GET /tags?namespace=crier&since=2026-11-01T00:00:00Z&limit=200
```

Counts are over messages the caller may read (permission-scoped, not global).

### 2.4 Filters

Appended to existing read routes, never replacing their parameters:

| Route | New query | Semantics |
|---|---|---|
| `GET /sessions/{id}/messages` | `?tag=build&tag=urgent` | AND across repeated `tag`; unknown tag → empty set, not an error |
| `GET /sessions/{id}/search` | `?tag=` | same |
| `GET /agents/{id}/inbox` | `?tag=` | same; tags ride the route record (COURIER-INGRESS §3) |
| `GET /sessions/{id}/messages` | `?tags_any=build,urgent` | OR form; mutually exclusive with `tag` (400 if both) |

### 2.5 Go

```go
package tags

func Normalize(in []string) ([]string, error)   // validates §2.1, dedupes, preserves order
func Match(msgTags []string, want []string) bool // AND semantics
func MatchAny(msgTags []string, want []string) bool
var ErrInvalid = errors.New("tag invalid")      // wrap: fmt.Errorf("%w: %q (reason)", ...)
```

`internal/session` stores tags verbatim after `tags.Normalize`; the HTTP layer owns filtering so
every backend (JSONL, SQLite, Postgres) behaves identically without new indexes in this row.

## 3. Data Model

- **JSONL store:** tags live on the `message` record itself (no migration; absent = none).
- **SQL store:** `ALTER TABLE chat_messages ADD COLUMN tags TEXT[]` (Postgres) /
  `tags TEXT` holding JSON (SQLite), defaulted NULL. Migration is additive; existing rows read
  as "no tags". A companion table is NOT created in this row — filtering is done in Go over the
  already-fetched candidate set, matching current behaviour for `search`. Pathological volume is
  a named non-goal (see §6).

```sql
-- postgres
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS tags TEXT[];
CREATE INDEX IF NOT EXISTS chat_messages_tags_gin ON chat_messages USING GIN (tags)
  WHERE tags IS NOT NULL;
```

The GIN index is used by `GET /tags` counts and by session-scoped filters; the inbox/route path
uses the existing route records.

## 4. Wiring

- `cmd/server/main.go`: `r.HandleFunc("/tags", tagsHandler.HandleList).Methods("GET")` in the
  authenticated subrouter.
- `ingressHandler` (COURIER-INGRESS) calls `tags.Normalize` before storing; a validation error
  becomes `400 INGRESS_TAGS_INVALID`.
- `HandlePostMessage` accepts the same `tags` field, so both write paths agree.
- `docs/openapi.yaml`: `tags` on `Message`, on the ingress request, and the `/tags` path.
- `chat.html`: a tag chip row above the composer (input: comma separated, submitted in the same
  POST) and a filter chip bar above the transcript. A chip toggles `?tag=`. No new page.
- `make readme-api-check` counts must be bumped in the same commit.

## 5. Error Catalog

| Code | Status | Condition |
|---|---|---|
| `TAGS_INVALID` | 400 | Grammar violation (§2.1), reporting the offending tag and reason. |
| `TAGS_TOO_MANY` | 400 | More than 16. |
| `TAGS_RESERVED_PREFIX` | 403 | `sys:` from a non-operator principal. |
| `TAGS_FILTER_CONFLICT` | 400 | Both `tag` and `tags_any` supplied. |
| `TAGS_QUERY_TOO_BROAD` | 400 | `GET /tags` with `limit` > 1000. |

## 6. Edge Cases

- **Unknown tag in a filter** → empty result set (200), never 404: a reader polling a category
  that has not appeared yet must not see an error.
- **Case and whitespace** → refused, not normalized (see §2.1). The CLI and web UI trim; the
  server does not guess.
- **Duplicates** → collapsed; `["build","build"]` stores one.
- **Untagged message** → `tags` absent on the wire (`omitempty`), `[]` in Go.
- **Tag rename** is out of scope: there is no vocabulary table, so a rename is a new tag. Named
  non-goal, recorded here so nobody "fixes" it by adding a mutation endpoint.
- **Scale limit, stated honestly:** a session with >100k messages and a tag filter is a full
  scan in the JSONL backend. Acceptable while sessions are bounded; the GIN index serves the SQL
  backends. If a session exceeds that, the answer is a bounded time window, not a new index.

## 7. Testing

1. `TestNormalizeAcceptsGrammar` — table: `build`, `build:urgent`, `a1`, 64-char segment → ok.
2. `TestNormalizeRejects` — table: `Build` (case), `-lead` (leading dash), `a:b:c` (two colons),
   65 chars, empty string, 17 tags → each error names the input.
3. `TestNormalizeDedupes` — `["x","x"]` → `["x"]`.
4. `TestPostMessageStoresTags` — POST with 3 tags, GET transcript returns the same 3 in order.
5. `TestTranscriptFilterAND` — 3 messages, tags a/b/ab; `?tag=a&tag=b` returns only the ab one.
6. `TestTranscriptFilterAny` — `?tags_any=a,b` returns all three.
7. `TestUnknownTagFilterIsEmptyNot404`.
8. `TestTagsVocabularyCounts` — counts reflect only visible messages (permission case included).
9. `TestInboxTagFilter` — inbox entry created via ingress with tags filters correctly.
10. `TestTagFilterAndAnyConflict` — 400.
11. JSONL↔SQLite↔Postgres parity: the same fixture produces the same filtered ids on all three.

## 8. Hilo Impact

Depends on: `internal/session` (Message + stores), `internal/permissions` (visibility scoping for
`/tags` counts). Depended on by: `COURIER-INGRESS.md` (validates and carries tags),
`COURIER-READS.md` (unread counts filterable by tag), `chat.html` chip UI.
No existing query changes shape; filters are additive query parameters.
