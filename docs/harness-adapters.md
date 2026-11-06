# Harness adapters

Push delivery: hand a stored Crier message to a live agent harness session —
continuing an existing one or creating a new one — and record a receipt that
says what actually happened. The inbox stays; this is the push half.

Binding contract: `specs/CRIER-HARNESS.md`. Row: CRIER-HARNESS-1 (interface,
registry, Hermes and opencode adapters; pi and codex are CRIER-HARNESS-2/3).
The codex provider knob and `HARNESS_PROVIDER_UNREACHABLE` are CRIER-HARNESS-14.

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

### codex provider endpoint (`CR_CODEX_BASE_URL`, CRIER-HARNESS-14)

codex takes its provider from `~/.codex/config.toml`, so without this knob the
endpoint a delivery talks to depends on the host — and a router that is down
reaches you as a bare `HARNESS_TIMEOUT`, because codex retries "waiting for
network" until the delivery deadline expires. Point it at your own endpoint:

```sh
export CR_CODEX_BASE_URL=http://127.0.0.1:8080/v1
```

- It is read once, when the adapters are built at startup: restart the daemon to
  change it. An explicit `Config.BaseURL` (embedders, tests) wins over it.
- It is not a block key. `adapters.codex` has no `base_url:` field, and the
  block refuses unknown keys — writing one there disables codex instead of
  moving the endpoint. `CR_HARNESS_CODEX_BASE_URL` is not read either; the name
  is `CR_CODEX_BASE_URL`.
- `crier harness list` shows `base_url` in codex's features while it is set;
  that is the quickest confirmation the daemon picked it up.
- The endpoint must speak the OpenAI Responses API, and the credential is still
  whatever the host exports as `OPENAI_API_KEY`: the adapter defines its own
  provider (`crier`) and leaves the built-in `openai` one alone — codex 0.166.0
  refuses to let the built-in one be redirected.

Both paths carry it as one complete provider definition, TOML-quoted, one value
per argv element:

```
create:  codex exec --json -s "<sandbox>" [-m <model>]
           -c 'model_provider="crier"'
           -c 'model_providers.crier.name="Crier"'
           -c 'model_providers.crier.base_url="<url>"'
           -c 'model_providers.crier.wire_api="responses"'
           -c 'model_providers.crier.env_key="OPENAI_API_KEY"'
           [session args] -- "<text>"
resume:  codex exec resume --json -c 'sandbox_mode="<sandbox>"' [-m <model>]
           (the same five -c provider pairs) <thread-id> -- "<text>"
```

On resume the sandbox mode moves from `-s` to the `-c` override, because
`codex exec resume` has no `-s`. A prompt over 64 KiB ends the argv with `-`
instead and travels on stdin.

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
`HARNESS_DISABLED`, `HARNESS_CONCURRENCY_FULL`, `HARNESS_WORKDIR_MISSING`,
`HARNESS_PROVIDER_UNREACHABLE` (codex; the provider refused the connection).

`HARNESS_PROVIDER_UNREACHABLE` means one thing: the provider endpoint the CLI
was pointed at refused the connection — no turn reached the model, the
adapter's own diagnostic TCP connect was refused, and the `tail` quotes the
target URL plus the operating system's dial error and codex's own connection
line. It is not a delivery timeout (a turn that ran and hit `timeout_seconds`
is `HARNESS_TIMEOUT`; this code replaces it only when the endpoint also refuses
TCP, since codex retries in silence and the deadline is the symptom, not the
cause), not a missing binary or an old version (`HARNESS_UNAVAILABLE`), and not
a config error (an unknown or malformed adapter block disables the adapter at
load). The connect runs on failed deliveries only, is capped at 2 s, and never
on the success path.

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
