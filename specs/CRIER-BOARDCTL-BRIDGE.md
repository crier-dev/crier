# CRIER-BOARDCTL-BRIDGE — Crier as a client of a boardctl board (the board-manager agent)

Status: DRAFT v1 · 2026-11-08 · Owner ruling 2026-11-08 ("crier directly attached to managing
a boardctl board") · Row: **INT-014**

Series: `specs/CRIER-WEBHOOK-RSS-INPUT.md` (the ingest path board events can also arrive by),
`specs/CRIER-GITHOOK-DISPATCH.md` (the outbound half: rows leaving the board into crier),
`specs/HERMES-SESSION-DELIVERY.md` (the server-side delivery-transport precedent this mirrors),
`specs/CHAT-AGENT-CLASSES.md` (`service` class agents that OWN systems).

## 1. Purpose

The fleet's task board is a boardctl board (`.coding-hermes/board/`, canonical branch
`master` of `coding-hermes-tools`). Today the only way to touch it from a conversation is a
human typing `boardctl` in a shell. This spec gives crier a **board-manager agent**: a
`service`-class agent whose delivery transport is the boardctl CLI — a message addressed to it
becomes an allowlisted boardctl invocation, and boardctl's structured answer comes back as the
reply message. Crier becomes a chat client OF the board without crier ever learning the JSONL
format.

**Premise verification (measured 2026-11-08).**

| Claim | Measured fact |
|---|---|
| boardctl CLI surface | `boardctl --help` at `/home/kara/.local/bin/boardctl` (repo `/home/kara/coding-hermes-tools`, branch `master` @ `e7a17ab`): `init / list / show / next-id / create / update / event / header / validate / sweep-status / normalize-statuses / normalize-board / doctor / version / stats / initiatives / render / import / serve / install`. `create` takes `--id --title --priority --complexity --depends-on --reasoning --capability-tags --acceptance-criteria …`; `update --status complete REQUIRES --evidence-run-id` (TR-251) — a completion without a bound run is REFUSED by the CLI itself |
| Crier repo board | `/home/kara/crier/.coding-hermes/board/` (tasks.jsonl 891 rows, events.jsonl 2305 events; `boardctl doctor` parses clean at HEAD `95fd40b0`) |
| Server-side transport precedent | `internal/hermesession` — a delivery TRANSPORT over an external CLI/HTTP surface, per-agent config on the registry `Agent` (`internal/registry/types.go:79` `Hermes *hermesession.Config`), credentials path-only/0600/read-per-delivery/never-logged |
| No bridge exists | no `boardctl` reference anywhere in tracked Go sources (grep 2026-11-08) |

## 2. Ownership split (binding)

- **R1** Crier NEVER parses or writes `tasks.jsonl` / `events.jsonl` directly. Every read and
  every write goes through the boardctl binary as a subprocess (argument vector only — no
  `sh -c`, the CRIER-HARNESS §cross-process rule). The file format, locking, normalization and
  validation remain boardctl's alone; crier owns only the chat surface and the execution
  discipline around the CLI.
- **R2** `specs/CRIER-GITHOOK-DISPATCH.md` is the OUTBOUND half (rows leaving the board into
  crier's queue). This spec is the INBOUND half (conversation → board). They share no state.

## 3. Interface

### 3.1 The agent and its transport

```go
// internal/boardbridge — a delivery transport, mirroring internal/hermesession.
type Config struct {
    BoardDir       string   `json:"board_dir"`                  // required
    BinPath        string   `json:"bin_path,omitempty"`         // default "boardctl", pinned on PATH at startup
    AllowedOps     string   `json:"allowed_ops,omitempty"`      // default "show,list,next-id,create,update,event"
    TimeoutSecs    int      `json:"timeout_secs,omitempty"`     // default 30, range 5..120
    SenderAllowlist []string `json:"sender_allowlist,omitempty"` // default empty; execution fails closed
}
```

- **R3** The bridge is a per-agent config block `boardctl` on the registry `Agent` (same
  position and lifecycle as the `hermes_session` block, `internal/registry/types.go:79`):
  registration-time validated (`Validate()` refuses an empty `board_dir`, a `timeout_secs`
  outside 5..120, or an `allowed_ops` token outside the closed verb set).
- **R4** Delivery trigger: a message delivered to an agent whose config carries `boardctl`
  executes INSTEAD of inbox-landing — exactly the `hermes_session` precedence (session block
  outranks webhook, HERMES-SESSION-DELIVERY §branch). Order: `boardctl` outranks
  `hermes_session` outranks webhook outranks inbox (one transport executes per message;
  this order is the spec's answer).

### 3.2 Typed command envelope

The delivered payload MUST be a versioned JSON object. Raw text is never tokenized or used as argv:

```json
{"v":1,"op":"show","args":{"id":"INT-007"}}
```

`v` must be 1; `op` must be in the agent's `allowed_ops`; and `args` must satisfy the closed, typed schema for that operation. Unknown keys, wrong value types, empty required values, and any user-controlled value beginning with `-` are refused before spawn. The bridge constructs the argument vector from the validated fields only, appends `--json`, and uses `exec` directly (never a shell). Positional arguments are passed only after the operation's fixed verb; option values are passed as individual argv elements.

The default allowed operations remain `show,list,next-id,create,update,event`. Each board-manager agent also carries `sender_allowlist`; it defaults empty, and a delivery from a sender not explicitly listed is stored in the inbox unchanged as inert text. This sender ACL is separate from transport selection.

- **R5** An unsupported envelope version, op outside `allowed_ops`, or invalid per-op args is refused with a named error and NO subprocess spawn.
- **R6** `--json` is appended by the bridge to every invocation and is not caller-controlled.
- **R7** Every invocation is serialized per agent; boardctl's own file locking stays the last line of defense, not the first.
- **R8** Timeout kills the subprocess process group and returns the named timeout failure.

### 3.3 Reply contract

- **R9** Exit 0 → reply message with boardctl's JSON output verbatim in `payload`, plus a
  one-line human summary in `text` (row id + resulting status for `create`/`update`; row
  count for `list`).
- **R10** Non-zero exit → reply carries the structured error mapping of §10; `boardctl_text` contains boardctl's own refusal text verbatim. CLI refusals (duplicate id, TR-251 evidence-run requirement, off-vocabulary status) are passed through, never reinterpreted or retried.
- **R11** Every executed command appends a board event for audit:
  `boardctl event --type audit --actor board-manager --detail-text "<verb> <args…>"` —
  run by the bridge after each successful mutation (create/update/event), skipped for
  read-only verbs.

## 4. Wiring

- **R12** Gates: `CR_BOARDCTL_BRIDGE_ENABLED` (tolerant bool, **default false** — no transport
  registered, agents carrying a `boardctl` block deliver to inbox unchanged) and
  `CR_BOARDCTL_BIN` (path; default `boardctl` on PATH; a configured path that is missing or
  not executable ⇒ fail-closed startup exit 1, R13).
- **R13** Startup doctor: when enabled, the bridge runs `boardctl -C <board_dir> doctor` and
  `<bin> version --json`; a doctor exit ≠ 0 does NOT block startup (drift warnings are normal,
  cf. crier's own board) but IS logged as `boardctl doctor exit=N`; a missing/unparsable board
  header at `<board_dir>` DOES block registration of any agent carrying a `boardctl` block
  (the agent registers, the transport refuses with the named reply of §5 on every delivery).
- **R14** Binary pinning: `BinPath` is resolved ONCE at startup (absolute path recorded);
  PATH re-resolution per delivery is refused (a swapped binary between deliveries is a
  supply-chain hazard, not a feature).
- **R15** No credentials cross the boundary: the bridge passes no secrets to boardctl and
  reads none from the board dir; message bodies are never logged (guard discipline).

## 5. Error catalog

| Condition | Reply to sender |
|---|---|
| Verb outside `allowed_ops` | `refused: op "<verb>" not allowed for this agent` — no spawn (R5) |
| Verb inside set but bad args | `boardctl refused: <boardctl's own stderr>` (R10) |
| Board dir missing/unparsable | `boardctl refused: board header unparsable at <board_dir>` (R13) |
| Invocation timeout | `boardctl failed: timeout after Ns, process group killed` (R8) |
| Binary missing at delivery time | `boardctl failed: pinned binary <path> no longer executable` — pinned path named (R14) |
| Bridge disabled (gate off) | no transport — message inbox-lands unchanged (R12) |

## 6. Edge cases

- **R16** Empty message text → `refused: empty command` (no spawn).
- **R17** `create` without `--acceptance-criteria` → boardctl refuses (undispatchable row);
  the bridge does NOT pre-validate or auto-fill — the CLI is the discipline (R10).
- **R18** Concurrent deliveries from two sessions to the same board-manager → serialized by
  R7; second waits; no interleaved partial output.
- **R19** Output size: `list --all` on an 891-row board → the bridge truncates the reply
  payload at 64 KiB with a `… (N bytes truncated; use a narrower filter)` note.
- **R20** Board on a git branch (canonical `master`): the bridge performs NO git operations —
  commit/push of board files remains the owner's/lane's flow (`CRIER-GITHOOK-DISPATCH.md`
  covers the hook side). The bridge works on the working tree as-is and says so in replies
  that create/update: `… (uncommitted; board files commit via your board flow)`.

## 7. Testing

| Test | Expected |
|---|---|
| Register agent with `boardctl` block → delivery of `show INT-007` → exit-0 reply with `--json` payload; subprocess argv exactly `[bin, -C, dir, show, INT-007, --json]` | R2, R6 |
| `serve` (not in allowed_ops) → refusal reply, exec NEVER invoked (assert via fake bin counter) | R5 |
| `update --status complete` without `--evidence-run-id` → verbatim boardctl refusal text | R10, R17 |
| Two concurrent deliveries → serialized (timestamps non-overlapping in fake-bin log) | R7 |
| Slow fake bin (sleep 60, timeout 5) → timeout reply, no orphan process after 6s | R8 |
| Gate off → agent with block still inbox-lands | R12 |
| Missing bin path at startup → exit 1 naming `CR_BOARDCTL_BIN` | R13 |
| 200 KiB `list` reply → truncated at 64 KiB with note | R19 |

## 8. Non-goals

- No natural-language command parsing. Messages are typed JSON envelopes; crier never parses prose into argv.
- No multi-board routing (one `board_dir` per agent; register two agents for two boards).
- No closing rows without evidence, no status aliasing, no board repair — `boardctl` owns all
  of it and the bridge forwards its refusals (R10).
- No scheduling, dispatching or worker supervision — crier is a client of the board here, not
  a foreman.

## 9. Open questions for review quorum

1. **Q-1** Precedence when an agent carries BOTH `boardctl` and `hermes_session` (R4 picks
   boardctl-first). Alternative: registration-time refusal of a second transport on the same
   agent. Default: precedence, no refusal (mirrors webhook+hermes_session coexistence today).
2. **Q-2** Should read-only verbs bypass the audit event (R11 skips them) or is full
   read-audit wanted? Default: mutations only — events.jsonl grows 2305 rows/2.5 weeks
   already.
3. **Q-3** Response latency: `list` on a 900-row board via CLI is ~100ms; is a per-agent
   response cache worth it? Default: no cache until measured slow.

## 10. Quorum amendments (2026-11-08)

A chimera quorum adversarial review returned **RECONSIDER** for this spec, with a
fatal flaw: parsing message TEXT as argv is unpatchable at a trust boundary — flag
injection through allowlisted subcommands (`add --config=/tmp/evil.json`,
`show --output /tmp/exfil`) and positional values beginning with `-` parsed as
flags, both due to unspecified text-to-argv tokenization (whitespace splitting
also breaks quoted paths). Seats: redteam `gpt-5.6-sol`, architect `grok-4.5`,
challenger `kimi-k2.7-code`, merge (aggregator) `qwen3.7-max` — total cost $0.33.
Full verdict: `/home/kara/.hermes/cache/spillover/chatcmpl-tool-9efc3d70c8fcf76d.txt`.
The v1 §3.2 text-as-argv grammar is REPLACED by the contract below.

1. **Typed command envelope (replaces R5/R6).** The delivered message MUST be a
   versioned JSON/CBOR command envelope: `{"v":1,"op":"show","args":{"id":"INT-007"}}`.
   An envelope with an unknown `v`, an `op` outside the agent's `allowed_ops`, or
   args failing the strict per-op schema ALLOWLIST (closed key set, typed values,
   no embedded flag syntax — an args value beginning with `-` is a refusal, never
   a token; `--` is forced before any user-controlled value) is refused with a
   named error BEFORE any subprocess spawns. The bridge alone constructs argv
   from validated fields: no invocation token originates from raw message text.
2. **Sender-command ACL (new).** Inbox delivery does NOT imply execution
   permission: each board-manager agent carries an explicit allowlist of senders
   whose command envelopes it executes. Deliveries from senders not on the
   allowlist inbox-land as inert text. Default: EMPTY allowlist (fail-closed —
   nobody may execute until explicitly granted at registration).
3. **Structured error mapping (extends R10).** boardctl exit codes map to typed
   structured errors in the reply — `{"error_kind":"refused"|"timeout"|"crash"|
   "unavailable","exit_code":N,"boardctl_text":"…"}` — instead of free-text
   passthrough only; boardctl's verbatim refusal text is carried inside the
   structure, never lost.
