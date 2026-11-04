# CRIER-HARNESS.md — harness adapters: a message can start or continue a session in a real agent

Status: **DRAFT v1 · 2026-11-03** · Owner: Bane · Row: **CRIER-HARNESS-1** (interface + Hermes +
opencode), **CRIER-HARNESS-2** (pi-agent), **CRIER-HARNESS-3** (codex)
Series: `specs/CRIER-INGRESS.md` (the call that triggers delivery), `specs/CRIER-READS.md`
(harness sessions read through cursors too).

## 1. Purpose

Crier currently stores messages; an agent has to poll. This adds an adapter layer so a message
can be **delivered into a live harness session** — continuing an existing one or creating a new
one — with a receipt that says what actually happened. The inbox stays; this is the push half.

## 2. Verified harness surfaces (2026-11-03, on this control host)

These are measured, not assumed. Each is the *only* injection surface the adapter may use.

| Harness | Verified surface | Session continue | Session create |
|---|---|---|---|
| **pi** (`~/.local/bin/pi`) | `pi -p` (non-interactive), `--mode rpc|json|text`, `--session-id <id>` | `pi -p --session-id <id> "msg"` | same call — `--session-id` **creates the session if absent** (documented in `pi --help`) |
| **opencode** 1.18.29 | `opencode run` with `-s/--session <id>`, `--format json`, `--attach <url>`, `--title` | `opencode run -s <id> --format json "msg"` | `opencode run --title "<t>" --format json "msg"` |
| **opencode (server)** | `opencode serve --port N --hostname H`, then `--attach http://host:port` | attach form above | same |
| **codex** (CLI) | `codex exec [PROMPT]` (prompt arg or `-` for stdin), `codex exec resume <id>`, `codex exec fork <id>` | `codex exec resume <id> "msg"` | `codex exec "msg"` (new session) |
| **Hermes** | gateway `POST /v1/responses` (Bearer); `hermes chat -Q --resume <session> -q <prompt>` | `hermes chat -Q --resume <id> -q <msg>` | gateway call with no resume id |

Anything not in this table is **not** an injection surface for this row. Notably: no adapter may
open a TUI, drive a pty, or scrape a screen.

## 3. Interface

```go
package harness

// Adapter delivers one message into one harness session.
type Adapter interface {
    // Name is the stable harness id used in config, receipts and metrics.
    Name() string

    // Probe reports whether this adapter can run here. It must not start
    // anything: it inspects the binary path, version and required flags.
    Probe(ctx context.Context) (Capability, error)

    // Deliver sends msg into session sess. When sess.ID is empty the adapter
    // creates a session and reports its id; when set, it continues that one
    // and MUST fail (never silently create a different session) if the
    // harness does not know it.
    Deliver(ctx context.Context, sess SessionRef, msg Message) (Receipt, error)
}

type Capability struct {
    Available bool              `json:"available"`
    Version   string            `json:"version,omitempty"`
    Binary    string            `json:"binary,omitempty"`
    Reason    string            `json:"reason,omitempty"` // why unavailable, in words
    Features  map[string]bool   `json:"features,omitempty"` // e.g. {"continue":true,"create":true,"json":true}
}

type SessionRef struct {
    Harness   string `json:"harness"`     // "pi" | "opencode" | "codex" | "hermes"
    ID        string `json:"id,omitempty"` // empty => create
    Workdir   string `json:"workdir,omitempty"`
    Model     string `json:"model,omitempty"`
    Args      []string `json:"args,omitempty"`
}

type Message struct {
    ID       string   `json:"id"`        // Crier message id, for correlation
    Text     string   `json:"text"`
    Tags     []string `json:"tags,omitempty"`
    Author   string   `json:"author"`
    SessionID string  `json:"session_id"` // Crier session
}

// Receipt is evidence, not a promise. It is persisted with the message.
type Receipt struct {
    Harness      string    `json:"harness"`
    SessionID    string    `json:"session_id"`     // harness-side session id
    Created      bool      `json:"created"`
    State        string    `json:"state"`          // dispatched | accepted | failed
    ExitCode     int       `json:"exit_code,omitempty"`
    DurationMS   int64     `json:"duration_ms"`
    StdoutBytes  int       `json:"stdout_bytes"`
    Tail         string    `json:"tail,omitempty"` // last 2 KiB of stderr on failure
    Error        string    `json:"error,omitempty"`
    At           time.Time `json:"at"`
}

// Registry resolves a harness name to an adapter.
type Registry struct{ /* ... */ }
func (r *Registry) Get(name string) (Adapter, bool)
func (r *Registry) Capabilities(ctx context.Context) map[string]Capability
```

`State` vocabulary, exactly three values:
- `dispatched` — the process was launched and exited 0; the harness has the message.
- `failed` — launch/exit non-zero, or the harness rejected the session id. `Error` names why.
- `accepted` — reserved for adapters with a server-mode ack (opencode `--attach`); a 2xx from the
  server, no process.

## 4. Wiring

### 4.1 Config (`/etc/crier/config.yaml`; env override in parentheses)

```yaml
harness:
  enabled: true                     # CR_HARNESS_ENABLED
  default: hermes                   # CR_HARNESS_DEFAULT
  timeout_seconds: 900              # CR_HARNESS_TIMEOUT  (per delivery)
  max_concurrent: 4                 # CR_HARNESS_MAX_CONCURRENT
  adapters:
    hermes:
      binary: hermes                # CR_HARNESS_HERMES_BINARY
      workdir: /home/kara
      model: ""                     # empty = harness default
    opencode:
      binary: opencode
      server_url: ""                # when set, use --attach instead of a local run
      username: opencode            # basic auth for --attach
      password_file: /etc/crier/opencode.pass
    pi:
      binary: pi
      session_dir: ""               # --session-dir; empty = pi default
    codex:
      binary: codex
      sandbox: workspace-write      # codex sandbox mode passed to exec
```

A malformed adapter block disables **that adapter only** and logs `harness adapter disabled`
with the reason; the daemon still starts.

### 4.2 Route hook

`internal/ingress.Handler` calls `Dispatch` after the message is durably appended:

```go
if h.Dispatch != nil {
    report := h.Dispatch.Deliver(ctx, DispatchRequest{
        Harness:   req.Harness,        // from target.harness, else config default
        SessionRef: refFromSession(sess), // harness id stored on the Crier session
        Message:   msg,
    })
    result.Delivery = report
}
```

`internal/delivery` (extend, do not fork): one goroutine per delivery, bounded by
`max_concurrent`, each with its own `context.WithTimeout`. A failed delivery never rolls back the
stored message.

### 4.3 Where the harness session id lives

On the Crier session record, in a new field:

```go
// internal/session/session.go
// Harness names the adapter bound to this session and HarnessSessionID is the
// id in THAT harness. Empty means store-only (the default).
Harness          string `json:"harness,omitempty"`
HarnessSessionID string `json:"harness_session_id,omitempty"`
```

A `new_session` ingress with `target.harness:"pi"` creates the Crier session **and** records
the harness id on first successful delivery. Subsequent messages to that session continue it.
If the harness reports the session unknown (`opencode run -s <missing>` exits non-zero with a
"session not found" class message, `codex exec resume <missing>` likewise), the adapter returns
`failed` with `Error:"HARNESS_SESSION_UNKNOWN"` and **does not** create a replacement — a silent
fork would split the transcript.

### 4.4 CLI + MCP

- `crier harness list` → the capability table (`name, available, version, features, reason`).
- `crier harness probe <name>` → one capability row, exit 1 when unavailable.
- MCP: `harness_list`, `harness_probe` (read-only; no delivery tool — delivery is an ingress
  action, not an agent action).

## 5. Error Catalog

| Code | Status / State | Condition |
|---|---|---|
| `HARNESS_UNKNOWN` | `failed` | Config or request names an adapter that is not registered. |
| `HARNESS_UNAVAILABLE` | `failed` | Binary missing / version too old; `Error` carries `Probe`'s reason. |
| `HARNESS_SESSION_UNKNOWN` | `failed` | Continuing a session the harness does not have. Never auto-creates. |
| `HARNESS_TIMEOUT` | `failed` | Exceeded `timeout_seconds`; the child is killed (process group, not just the pid). |
| `HARNESS_EXIT_NONZERO` | `failed` | Exit ≠ 0; `Tail` carries the last 2 KiB of stderr. |
| `HARNESS_OUTPUT_INVALID` | `failed` | `--format json` output did not parse; the message was still delivered, so `State` is `dispatched` and this only downgrades the receipt to `Error`. |
| `HARNESS_DISABLED` | `failed` | `harness.enabled: false`. |
| `HARNESS_CONCURRENCY_FULL` | `failed` | `max_concurrent` saturated and the wait exceeded the timeout. |

A `failed` receipt never changes the HTTP status of the ingress call: storage succeeded.

## 6. Edge Cases

- **Binary present but not logged in** (codex 401, opencode no provider): `Probe` reports
  `available:true` (the binary runs) and `Deliver` returns `failed` with the harness's own stderr
  tail. Do not pre-flight credentials; that is the harness's job and it drifts.
- **Very long message:** pass on stdin where the CLI supports it (`codex exec -`, `pi -p` with the
  message as the final arg is bounded by ARG_MAX — use stdin when `len(text) > 64 KiB`).
- **Shell safety:** the adapter uses `exec.CommandContext` with an argument vector. **No `sh -c`,
  no string interpolation of message text.** This is a hard requirement: message text is
  attacker-influenced.
- **Workdir:** must exist and be a directory at delivery time; otherwise `failed`
  (`HARNESS_WORKDIR_MISSING`). Never auto-created — a wrong cwd silently changes what the agent
  does.
- **Concurrency:** the same Crier session must not get two concurrent deliveries (interleaved
  turns corrupt a harness transcript). Serialize per session with a keyed mutex; the second waits.
- **Restart with in-flight deliveries:** receipts are written on completion; an interrupted
  delivery is recorded as `failed` with `Error:"HARNESS_INTERRUPTED"` by the next start.

## 7. Testing

Per adapter (four adapters × the same contract):

1. `TestProbeReportsBinaryAndVersion` — with a fake script on `PATH` printing `--version`.
2. `TestProbeMissingBinary` — `binary: /nonexistent` → `available:false` with a reason naming it.
3. `TestDeliverCreatesSession` — fake harness records argv; assert `--session-id`/`-s`/`resume`
   absent (create path) and stdout parsed.
4. `TestDeliverContinuesSession` — argv contains the exact session id.
5. `TestDeliverUnknownSessionFails` — fake exits 1 with "session not found" → `State:"failed"`,
   `Error:"HARNESS_SESSION_UNKNOWN"`, and **no second invocation** occurred (no silent create).
6. `TestDeliverTimeoutKillsProcessGroup`.
7. `TestMessageTextNeverInterpolatedIntoShell` — a message containing `; rm -rf /` and `$(id)`
   arrives as one argv element, byte-identical.
8. `TestPerSessionSerialization` — two deliveries to one session never overlap (fake harness
   writes start/end timestamps).
9. `TestConcurrencyCap` — 10 deliveries with `max_concurrent: 2` never exceed 2 concurrent.
10. `TestReceiptPersistedOnMessage` — the receipt is readable from `GET /delivery-log`.

Contract test that runs against all adapters via a table, so a new adapter cannot ship with a
different contract: `TestAdapterContract` in `internal/harness/contract_test.go`.

Live proof (row evidence, not CI): deliver one message into a **real** pi session and one into a
real opencode session on a scratch host, then show the harness's own `session list` / transcript
containing the text. Codex and Hermes adapters ship in the same shape; a codex live proof needs a
logged-in CLI and is recorded separately.

## 8. Hilo Impact

Depends on: `internal/session` (Harness fields), `internal/delivery` (concurrency + timeout
plumbing), `internal/config`.
Depended on by: `CRIER-INGRESS.md` (§4 hook), the Crier UI (per-session harness badge), and
the future control plane that attaches harnesses to sessions.
Nothing existing changes: with `harness.enabled: false` every code path behaves exactly as today.
