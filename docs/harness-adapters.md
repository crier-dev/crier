# Harness adapters

Push delivery: hand a stored Crier message to a live agent harness session —
continuing an existing one or creating a new one — and record a receipt that
says what actually happened. The inbox stays; this is the push half.

Binding contract: [`specs/CRIER-HARNESS.md`](../specs/CRIER-HARNESS.md). All five
adapters are built and registered — hermes and opencode (CRIER-HARNESS-1), pi
(CRIER-HARNESS-2), codex (CRIER-HARNESS-3), claude-code (CRIER-HARNESS-11) — and
the route that offers a stored message to them, `POST /ingress/messages`, is
mounted in `cmd/server/main.go` with `cmd/server/harnesswiring.go` as the joint
to the registry. What is **not** landed is the public surface that binds a
session to a harness: see [Binding a harness to a
session](#binding-a-harness-to-a-session). The codex provider knob and
`HARNESS_PROVIDER_UNREACHABLE` are CRIER-HARNESS-14.

Delivery is **off by default** (`harness.enabled: false`), so with the block
absent every existing code path behaves exactly as before.

## What ships

`crier harness list` on this build (`bin/crier`, `v0.3.0-25-g1587a03f`):

| Name | Available | Version | Features |
|---|---|---|---|
| hermes | true | `0.22.5+3030.g770a952` | `cli,continue` |
| opencode | true | `1.18.29` | `continue,create,json` |
| pi | true | `1.5.0` | `continue,create,json,print,session_id` |
| codex | true | `0.166.0` | `continue,create,json,resume,sandbox,stdin` |
| claude-code | false | — | `continue,create,json,print,resume,stdin` |

Five adapter ids, five rows, no placeholder: every row is an adapter that
probes its own binary. `claude-code` is registered and probed too; it reports
`available:false` **only** because this host has no `claude` binary
(`reason: binary not found: claude`) — install the CLI and the same row turns
true. `pi-agent` is accepted as an alias of `pi` in a config block, a session
binding or a probe; the canonical id, the metric label and the persisted binding
stay `pi`.

## Verified surfaces

Only these injection surfaces exist; no adapter opens a TUI, drives a pty or
scrapes a screen, and no adapter interpolates message text into a shell — the
message is always one argv element (or one HTTP field).

| Harness | Continue | Create |
|---|---|---|
| hermes | `hermes chat -Q --resume <id> -q <text>` | gateway `POST /v1/responses` |
| opencode | `opencode run -s <id> --format json <text>` | `opencode run --title <t> --format json <text>` |
| opencode (`server_url` set) | same + `--attach <url>` | same |
| pi | `pi -p --mode json --session-id <id> -- <text>` | same call — `--session-id` creates an absent session |
| codex | `codex exec resume --json <thread-id> -- <text>` | `codex exec --json -s <sandbox> -- <text>` |
| claude-code | `claude -p --resume <id> --output-format json <text>` | `claude -p --output-format json <text>` |

### pi

`--session-id` is documented by `pi --help` as "use the current directory's
existing session with the ID, creating it otherwise", so create and continue are
**one** surface and an id pi has never seen is the create path — not an error.
The adapter therefore never answers `HARNESS_SESSION_UNKNOWN` for a fresh id: it
mints one, records the id it used on the receipt, and reports `created` from
pi's own "creating a new session" line. Two deviations from the bare argv form:
text over 64 KiB goes on stdin (ARG_MAX), and so does text starting with `@`,
because pi expands `@`-prefixed arguments as file references while a Crier
message must arrive as text. `--session-dir` is passed only when configured.

### codex

The thread id always comes from the CLI (`--json` prints it in the first event),
never from a flag, so a create binds the id codex minted. The sandbox mode is
always passed explicitly from config: `-s <mode>` on create, and
`-c 'sandbox_mode="<mode>"'` on resume because `codex exec resume` has no `-s`.
A prompt over 64 KiB replaces `-- "<text>"` with `-` and travels on stdin. The
provider override is described under [codex provider
endpoint](#codex-provider-endpoint-cr_codex_base_url-crier-harness-14).

### claude-code

There is no create-with-a-chosen-id flag, so like codex the adapter takes the id
the CLI minted and binds it on first delivery. An unknown `--resume` id is
`HARNESS_SESSION_UNKNOWN`; exit 0 with `is_error:true` in the JSON result is a
**failed** receipt, because "the process ran" is not "the agent answered"; a
create that mints no id downgrades to `HARNESS_OUTPUT_INVALID`.
`--permission-mode` is passed only when configured — the posture is never
widened silently.

### hermes

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
      binary: pi                    # CR_HARNESS_PI_BINARY
      session_dir: ""               # CR_HARNESS_PI_SESSION_DIR (--session-dir)
    codex:
      binary: codex                 # CR_HARNESS_CODEX_BINARY
      sandbox: workspace-write      # CR_HARNESS_CODEX_SANDBOX (shipped default)
    claude-code:
      binary: claude                # CR_HARNESS_CLAUDE_CODE_BINARY
      permission_mode: ""           # CR_HARNESS_CLAUDE_CODE_PERMISSION_MODE
```

The block is a union: `workdir` (the delivery cwd, which must exist at delivery
time) and `model` (passed where the verified surface takes one; empty means the
harness default) apply to every adapter, while the adapter-specific keys above
are used only by the adapter they name. An unknown key inside a block is a
malformed block, not a silently ignored line.

Every adapter field also has a `CR_HARNESS_<ADAPTER>_<FIELD>` override
(`BINARY`, `WORKDIR`, `MODEL`, `GATEWAY_URL`, `GATEWAY_TOKEN`, `SERVER_URL`,
`USERNAME`, `PASSWORD_FILE`, `SESSION_DIR`, `SANDBOX`, `PERMISSION_MODE`). An
adapter id's hyphen becomes an underscore in the variable name, so claude-code
occupies `CR_HARNESS_CLAUDE_CODE_*`. `CR_HARNESS_CONFIG` names a YAML file; the
file may be the whole relay document (a `harness:` key) or just the block.

A malformed adapter block disables **that adapter only**, records the reason
and logs `harness adapter disabled`; the daemon still starts. Bad top-level
numbers fall back to their defaults with a warning. An `adapters.<name>` block
naming something no adapter serves is refused at boot, before any route is
mounted, and the error lists the accepted set (so a typo is fixed without a doc
lookup); a `default:` (or `CR_HARNESS_DEFAULT`) naming an unknown adapter is a
startup warning that names the consequence — `HARNESS_UNKNOWN` on a request that
names no harness.

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

## Binding a harness to a session

A Crier session carries the binding in two fields
(`internal/session/session.go`): `harness` names the adapter, and
`harness_session_id` is the id in **that** harness. Empty means store-only,
which is the default. The `session.harness.bind` record that persists them
requires an id (it refuses a record that carries none), so a session that names
a harness and has no id yet is not a broken state — it is the create path. The
binding survives a restart, is projected into the `chat_sessions` view (SQLite
and PostgreSQL) and appears on the session object (`GET /sessions` omits both
keys while the session is store-only; an absent binding is not an empty
string). `specs/CRIER-HARNESS.md` §4.3 is the contract; this section is what
this build does with it.

What the dispatch does with a stored message (`cmd/server/harnesswiring.go`):

1. no session, no enabled registry, or no `harness` on the session → the
   message stays store-only and the report is `{"mode":"store","state":"stored"}`;
2. `harness` set and `harness_session_id` empty → the adapter **creates** the
   harness session, and the id it minted is recorded on the Crier session, so
   every later message continues that one instead of minting a second;
3. both set → the message continues exactly that harness session;
4. the harness does not know the id → `HARNESS_SESSION_UNKNOWN`. The adapter
   never creates a replacement: a silent fork splits the transcript.

The documented way to set the binding (spec §4.2/§4.3) is a `new_session`
ingress with the harness named on the target:

```json
{"mode":"new_session",
 "target":{"template":{"title":"deploy review","kind":"channel"},"harness":"pi"},
 "author":{"agent":"bob"},
 "text":"review the deploy"}
```

**The request surface for that key is not landed in this build (DF-CRIER-500).**
Measured against `bin/crier` at `v0.3.0-25-g1587a03f`, the target object accepts
exactly four keys and strict JSON refuses the rest — both spellings, and
`POST /sessions` too:

```
target.harness            -> 400 INGRESS_INVALID_JSON
  target: unknown field "harness" (accepted: session_id, template, agent_id, capability)
target.template.harness   -> 400 INGRESS_INVALID_JSON
  target: template: unknown field "harness" (accepted: title, kind, namespace, members)
POST /sessions {harness}  -> 400 INVALID_REQUEST
```

The same push without the key answers `200` with
`"delivery":{"mode":"store","state":"stored"}` and `created_session:true` — the
dispatch joint is reached and declines because the session names no harness, and
nothing in this build can name one. So today every session created through the
API is store-only and harness delivery runs only in-process (the Go suite). That
gap is DF-CRIER-500's, not a configuration mistake to chase.

Once DF-CRIER-500 lands, the sequence above is the whole recipe: enable delivery,
confirm the adapter with `crier harness list`, post the `new_session` ingress
with `target.harness`, read `delivery` in the response, and read the bound pair
back off the session before sending the next message to it with
`target.session_id`.

## CLI

```
crier harness list          # name, available, version, features, reason
crier harness probe <name>  # one capability as JSON; exit 1 when unavailable
```

Both are read-only. There is no delivery verb: delivery is an ingress action,
not an agent action. `probe` never starts a session — it resolves the binary
and runs `--version`. An unregistered name is a refusal that names the accepted
set, and the `pi-agent` alias resolves to `pi`. The same introspection is on the
MCP server as `harness_list` and `harness_probe` (read-only; no delivery tool).

## Receipts

```json
{"harness":"opencode","session_id":"ses_…","created":false,"state":"dispatched",
 "exit_code":0,"duration_ms":812,"stdout_bytes":96,"error":"","at":"2026-11-03T21:40:00Z"}
```

States are exactly three:

- `dispatched` — the process was launched and exited 0; the harness has the message.
- `accepted` — a server-mode acknowledgement (opencode `--attach`, the Hermes gateway).
- `failed` — launch/exit failure or a rejected session id.

The receipt rides the ingress response as the `delivery` object (`mode`,
`harness`, `state`, `error`); it is not written to a receipt log of its own. A
failed delivery never changes the HTTP status of the call — the message is
already durable, so the failure is reported inside a 2xx.

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
`harness` / `harness_session_id`), persisted as the `session.harness.bind`
record and in the `chat_sessions` view (SQLite and PostgreSQL). Empty means
store-only. See [Binding a harness to a
session](#binding-a-harness-to-a-session) for how the binding is written and
what the shipped request surface accepts.

## Not in this build

- The public binding surface (DF-CRIER-500): `target.harness` on the ingress
  body, and a harness field on `POST /sessions`. The dispatch joint that
  consumes a binding is landed (`cmd/server/harnesswiring.go`).
- A receipt log of its own. `GET /delivery-log` is the detection layer's signed
  delivery audit (CR-FEAT-030) and has nothing to do with harness receipts.
