# Harness adapters

Push delivery: hand a stored Crier message to a live agent harness session —
continuing an existing one or creating a new one — and record a receipt that
says what actually happened. The inbox stays; this is the push half.

Binding contract: `specs/CRIER-HARNESS.md`. Row: CRIER-HARNESS-1 (interface,
registry, Hermes and opencode adapters; pi and codex are CRIER-HARNESS-2/3).

Delivery is **off by default** (`harness.enabled: false`), so with the block
absent every existing code path behaves exactly as before.

## Verified surfaces

Only these injection surfaces exist; no adapter opens a TUI, drives a pty or
scrapes a screen, and no adapter interpolates message text into a shell — the
message is always one argv element (or one HTTP field).

| Harness | Continue | Create |
|---|---|---|
| hermes | `hermes chat -Q --resume <id> -q <text>` | gateway `POST /v1/responses` |
| opencode | `opencode run -s <id> --format json <text>` | `opencode run --title <t> --format json <text>` |
| opencode (server) | same + `--attach <url>` | same |
| claude-code | `claude -p --resume <id> --output-format json <text>` | `claude -p --output-format json <text>` |

The Hermes CLI cannot create a session through a verified surface, so when
`gateway_url` is empty the adapter reports `create:false` / `gateway:false` in
its capability features and a create delivery fails `HARNESS_UNAVAILABLE`
instead of inventing a flag.

## Configuration

```yaml
harness:
  enabled: false                    # CR_HARNESS_ENABLED
  default: hermes                   # CR_HARNESS_DEFAULT
  timeout_seconds: 900              # CR_HARNESS_TIMEOUT (per delivery)
  max_concurrent: 4                 # CR_HARNESS_MAX_CONCURRENT
  adapters:
    hermes:
      binary: hermes                # CR_HARNESS_HERMES_BINARY
      workdir: /home/kara           # CR_HARNESS_HERMES_WORKDIR
      model: ""                     # CR_HARNESS_HERMES_MODEL (gateway create only)
      gateway_url: ""               # CR_HARNESS_HERMES_GATEWAY_URL (create path)
      gateway_token: ""             # CR_HARNESS_HERMES_GATEWAY_TOKEN
    opencode:
      binary: opencode              # CR_HARNESS_OPENCODE_BINARY
      server_url: ""                # CR_HARNESS_OPENCODE_SERVER_URL (--attach)
      username: opencode            # CR_HARNESS_OPENCODE_USERNAME
      password_file: ""             # CR_HARNESS_OPENCODE_PASSWORD_FILE
    pi:
      binary: pi                    # CR_HARNESS_PI_BINARY (block accepted, adapter CRIER-HARNESS-2)
    codex:
      binary: codex                 # CR_HARNESS_CODEX_BINARY (block accepted, adapter CRIER-HARNESS-3)
      sandbox: workspace-write
```

Every adapter field also has a `CR_HARNESS_<ADAPTER>_<FIELD>` override
(`WORKDIR`, `MODEL`, `GATEWAY_URL`, `GATEWAY_TOKEN`, `SERVER_URL`, `USERNAME`,
`PASSWORD_FILE`, `SESSION_DIR`, `SANDBOX`). `CR_HARNESS_CONFIG` names a YAML
file; the file may be the whole relay document (a `harness:` key) or just the
block.

A malformed adapter block disables **that adapter only**, records the reason
and logs `harness adapter disabled`; the daemon still starts. Bad top-level
numbers fall back to their defaults with a warning.

The delivery working directory must exist at delivery time
(`HARNESS_WORKDIR_MISSING` otherwise) and is never created.

## CLI

```
crier harness list          # name, available, version, features, reason
crier harness probe <name>  # one capability as JSON; exit 1 when unavailable
```

Both are read-only. There is no delivery verb: delivery is an ingress action,
not an agent action. `probe` never starts a session — it resolves the binary
and runs `--version`.

## Receipts

```json
{"harness":"opencode","session_id":"ses_…","created":false,"state":"dispatched",
 "exit_code":0,"duration_ms":812,"stdout_bytes":96,"error":"","at":"2026-11-03T21:40:00Z"}
```

States are exactly three:

- `dispatched` — the process was launched and exited 0; the harness has the message.
- `accepted` — a server-mode acknowledgement (opencode `--attach`, the Hermes gateway).
- `failed` — launch/exit failure or a rejected session id.

Error codes: `HARNESS_UNKNOWN`, `HARNESS_UNAVAILABLE`,
`HARNESS_SESSION_UNKNOWN` (never auto-creates a replacement),
`HARNESS_TIMEOUT` (process group killed), `HARNESS_EXIT_NONZERO` (2 KiB stderr
tail), `HARNESS_OUTPUT_INVALID` (delivered; only the receipt is downgraded),
`HARNESS_DISABLED`, `HARNESS_CONCURRENCY_FULL`, `HARNESS_WORKDIR_MISSING`.

Deliveries are serialised per Crier session (two concurrent turns would
interleave one transcript) and bounded by `max_concurrent`.

## Where the harness session id lives

`internal/session.Session.Harness` / `HarnessSessionID` (JSON
`harness` / `harness_session_id`), persisted on the `session.create` record and
in the `chat_sessions` view (SQLite and PostgreSQL). Empty means store-only.

## Not in this row

- `pi` and `codex` adapters (config blocks are accepted and report unavailable).
- The `POST /ingress/messages` route that calls the registry (`internal/ingress`
  is a later row); `Registry.Deliver` is the exported hook it will call.
- MCP `harness_list` / `harness_probe` tools.
- A `GET /delivery-log` route; receipts are returned by `Registry.Deliver` and
  persisted by the ingress row.
