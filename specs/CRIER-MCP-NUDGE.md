# CRIER-MCP-NUDGE.md — the agent is told it has mail, and decides whether to read it

Status: **DRAFT v1 · 2026-11-03** · Owner: Bane · Row: **CRIER-MCP-NUDGE** (stretch)
Series: `specs/CRIER-READS.md` (unread numbers the nudge reports), `specs/CRIER-INGRESS.md`
(the traffic that produces mail).

## 1. Purpose

An agent that never polls still needs to know mail is waiting. Give MCP clients a **nudge
surface**: a zero-cost way to learn "you have N unread in Crier, categories X/Y" at the start of
a turn, plus the existing tools to fetch only if it chooses.

Design constraint from the owner, made normative here: **the nudge informs, it never auto-reads
and never injects message bodies.** Bodies arrive only when the agent calls a Crier tool.

## 2. Interface

### 2.1 MCP surface (additive, `internal/mcp`)

Three additions to the existing tool set (`register_agent`, `retrieve_inbox`, `ack_messages`,
`inbox_stats` already exist and are unchanged):

| Kind | Name | Purpose |
|---|---|---|
| Tool | `courier_unread_summary` | Counts only. `{agent_id?, session_id?}` → `{total, by_category, oldest_at, sessions:[{session_id, unread}]}`. Never returns text. |
| Resource | `courier://inbox/{agent_id}/unread` | Same JSON, readable as a resource for harnesses that attach resources to the system prompt. |
| Prompt | `courier_nudge` | A one-line template the client renders: `You have {n} unread messages in courier ({categories}). Call courier_unread_list to read them.` |

A fourth, read-only tool completes the loop:
`courier_unread_list` → `{count, messages:[{message_id, session_id, author, tags, text, seq}]}`,
bounded by `limit` (default 20, max 100). This is the "read them" the nudge names.

### 2.2 Delivery of the nudge (harness-dependent, all optional)

| Mechanism | When it applies | Contract |
|---|---|---|
| MCP prompt/resource | Any MCP client | Client decides when to surface it. No push. |
| Session-start hint | `CRIER-HARNESS` adapters | A **prefix line** prepended to the delivered message text: `[courier] 3 unread (build, urgent) — call courier_unread_list to read.` Never a separate hidden message. |
| SSE/websocket notification | Clients connected to `/relay/subscribe/{topic}` | `{"type":"Crier.unread","agent_id":"alpha","count":3}` — count only, no bodies. |

The session-start hint is the practical version of "tell the agent at the beginning". It is
embedded in the message the harness receives, so no harness cooperation is required beyond
accepting a message.

### 2.3 Go

```go
package mcp

// courier_unread_summary handler
type UnreadSummary struct {
    Total      int            `json:"total"`
    ByCategory map[string]int `json:"by_category"`
    OldestAt   *time.Time     `json:"oldest_at,omitempty"`
    Sessions   []SessionUnread `json:"sessions"`
}
type SessionUnread struct {
    SessionID string `json:"session_id"`
    Unread    int    `json:"unread"`
}

// nudgePrefix builds the one-line hint; pure, table-tested.
func nudgePrefix(total int, categories []string) string
```

`nudgePrefix` rules (exact):
- `total == 0` → `""` (no prefix, no noise).
- 1 category → `[courier] 3 unread (build) — call courier_unread_list to read.`
- 2 categories → `... (build, urgent) ...`
- 3 or more → `... (build, urgent +2) ...`
- total is capped at the configured `mcp.nudge_max_count` (default 999) with a `+` suffix.

## 3. Data Model

None new. Counts are computed from existing inbox state plus `CRIER-READS` cursors when the
message belongs to a session rather than an agent inbox:

- agent-inbox unread = existing inbox stats (`unread` already exists there)
- session unread = `CRIER-READS.Service.Unread`
- `by_category` = tags (CRIER-TAGS), with untagged counted as `uncategorized`

## 4. Wiring

- `internal/mcp/tools.go`: register the three tools; `internal/mcp/server.go`: register the
  resource and prompt alongside the existing `serverName` block.
- `spec_drift_test.go` already pins MCP tools against `docs/`; the new names must be added there
  and to `docs/openapi.yaml`'s MCP appendix if present, or the drift test fails — that is the
  gate working.
- Config: `mcp.nudge.enabled` (default true), `mcp.nudge.max_count` (999),
  `mcp.nudge.session_start` (default false — opt-in per deployment, because a prefix changes the
  agent's prompt and that is a product decision, not a default).
- `CRIER-HARNESS` adapters read `mcp.nudge.session_start` and prepend via `nudgePrefix`.

## 5. Error Catalog

| Code | Condition |
|---|---|
| `NUDGE_AGENT_REQUIRED` | Neither `agent_id` nor `session_id` supplied. |
| `NUDGE_AGENT_UNKNOWN` | Agent not registered → empty summary with `total:0`, **not** an error (a nudge must never fail a turn). |
| `NUDGE_SCOPE_DENIED` | Asking for another agent's summary without operator scope. |

The tool never returns an MCP protocol error for an unknown agent: silence is the correct answer
to "do I have mail" for an agent that is not registered yet.

## 6. Edge Cases

- **Zero unread** → `total:0`, empty categories, no prefix, no resource body beyond zeros.
- **Very large counts** → capped at `max_count`, reported with `+`.
- **Message arrives mid-turn** → not visible until the next call; no streaming into a running
  tool result.
- **Multiple agents in one session** → each agent's summary is scoped to its own member id.
- **Untagged messages** → `uncategorized` bucket, never omitted.
- **Nudge + inbox mode** → both numbers appear; the same message is never double-counted because
  agent-inbox and session unread are separate stores and the summary reports them separately
  (`sessions[]` vs the inbox total, with the total being inbox + sessions).
- **Prefix length** is bounded to 160 chars; an agent's context budget is not negotiable.

## 7. Testing

1. `TestNudgePrefixZeroIsEmpty`.
2. `TestNudgePrefixFormatting` — table of 1/2/3+ categories, cap behaviour.
3. `TestUnreadSummaryShape` — total, by_category, sessions all populated; bodies absent
   (assert no `text` key in the JSON).
4. `TestSummaryUnknownAgentIsEmptyNotError`.
5. `TestSummaryScopeDenied` — another agent's summary without operator scope → 403.
6. `TestUnreadListBounded` — limit default 20, max 100, over-max clamps.
7. `TestSessionStartHintPrepended` — with `mcp.nudge.session_start:true`, the delivered message
   text starts with the prefix and the original text is intact after it.
8. `TestSessionStartHintDisabled` — default config: no prefix, byte-identical text.
9. `TestMCPToolDriftGate` — the new tool names appear in the drift-pinned inventory.

## 8. Hilo Impact

Depends on: `internal/mcp`, `internal/session` inbox stats, `CRIER-READS` cursors,
`CRIER-TAGS` categories, `CRIER-HARNESS` adapters (for the prefix).
Depended on by: nothing — this is the outermost convenience layer. Disabled by config, the system
is unchanged. This row is explicitly a **stretch**: 1–3 and the harness adapters are useful
without it.
