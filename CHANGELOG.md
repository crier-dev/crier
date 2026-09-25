# Changelog

All notable changes to crier are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project carries one version per release tag (`vMAJOR.MINOR.PATCH`,
plus a pre-release suffix such as `-rc1` while the project is pre-1.0).

Entries are written BEFORE the tag is cut, and the tag point is the commit that
contains them — a tag cut first can never contain its own changelog entry. The
procedure, the gate requirements and the publish step are in
[docs/releases.md](docs/releases.md).

## [Unreleased]

### Added

- **Capability-routed delivery — the registry's capability index as a dialable
  worker pool** (CR-FEAT-026). `GET /agents?capability=solver` could already tell
  you who advertises a capability, but delivery still required naming ONE agent
  id, so a pool of interchangeable workers could not be addressed as a pool (the
  gap the external review `DISPATCH · CRI-001` by Carter measured live at
  `ca28523d`). `POST /capabilities/{capability}/inbox` takes the same body as
  `POST /agents/{id}/inbox` and picks the holder: candidates are every agent
  whose advertised `capabilities` include the name (matched exactly like the
  discovery filter), **live holders rank first** (liveness is the registry's own
  derived status, so a crashed worker absorbs no work while a live one exists),
  and the choice **round-robins** over that pool — one holder per delivery,
  agent-id ascending, one cursor per capability, advanced once per dispatched
  delivery. The accept names what it chose (`"capability":"solver"`,
  `"target":"worker-b"`) because the sender has to know which worker took the
  work; a by-id delivery's body is unchanged. **Zero holders is a named error,
  never a silent drop**: `404 NO_CAPABLE_AGENT` naming the capability and stating
  that selection is local to the relay (federation forwards by agent id) — while
  a capability whose holders are all merely stale still ACCEPTS, because the
  inbox is durable and an untaken message is carried by the existing expiry
  receipt. A holder that dies **mid-lease** needs no case of its own: the lease
  expires and the message returns to the queue it was delivered to. A retry
  carrying an `idempotency_key` is scoped to the CAPABILITY (the holder is not
  known when the key is resolved), so a repeated key is answered with the first
  attempt's accept — same id, same `target` — instead of dispatching the same job
  to a second worker, and it is answered even if the pool has since emptied.
  Everything after the choice is the existing deliver path (guard choke point,
  webhook driver, durable inbox, lease/ack/TTL, federation fallback), and the
  detection layer observes a routed delivery with the RESOLVED holder as its
  target and a zero-holder refusal with an empty one. Two counters
  (`capability_routed_total`, `capability_unheld_total`) make a pool that has
  gone empty visible. Proved by `TestCapabilityDelivery*` in
  `internal/registry` (landing + signed retrieve + ack, the exact rotation, a
  stale holder skipped, the named refusal with nothing stored, capability-scoped
  idempotency, lease requeue after a holder dies, one observation per request,
  and a concurrent 40-delivery rotation under `-race`). Source: the external
  review `DISPATCH · CRI-001` by Carter, delivered via Bane.
- **Priority lanes and real backpressure** (CR-FEAT-035) — the three gaps the
  external review (`DISPATCH · CRI-001`, by Carter) named in passing, closed
  without changing what an existing deployment does:
  - **an optional `priority` on a delivery** (`POST /agents/{id}/inbox`,
    integer 0..9, default 0) — `GET /agents/{id}/inbox` hands back the highest
    priority claimable messages first, ties keep arrival order, and a delivery
    that names no priority is exactly the FIFO message it always was (the key is
    omitted from the wire at the default, so a pre-existing client parses
    identical bytes). Out of range is a 400 naming the range, never a clamp. The
    ordering is stored with the message (PostgreSQL migration 007, `priority
    NOT NULL DEFAULT 0`, range-checked), so it survives a restart and every
    message an existing database already holds reads back in the order it has
    always had.
  - **a global ingest budget** (`CR_RATE_LIMIT_GLOBAL_PER_MINUTE`, default 0 =
    no budget) — a shed on the delivery path itself, across every agent and
    sender, which the per-agent publish cap could never be: a flood from many
    ids spends every agent's own budget. A delivery over budget is refused **429
    `RATE_LIMITED_GLOBAL`** with a `Retry-After` header (whole seconds) and the
    same wait in the body, before any transport, guard call or store write —
    and the check sits after decode/validation/containment, so a malformed
    delivery still answers 400 and a contained agent still answers 403
    `AGENT_QUARANTINED`. The pre-existing **per-agent** publish 429 gained the
    `Retry-After` header the review found missing; its body and its scope are
    unchanged. Per-namespace budgets remain CR-FEAT-029's half (the shed counter
    is already labelled by scope, so that change adds series rather than
    renaming them).
  - **queue depth where operators already look** — `GET /status` reports
    `queue_depth` (`pending` / `leased` / `oldest_age_s`) beside
    `global_rate_limit_per_minute`, and `GET /metrics` exposes
    `inbox_queue_depth`, `inbox_queue_leased` and
    `inbox_queue_oldest_age_seconds` (gauges; `NaN` when the serving store
    cannot report a depth) plus `inbox_shed_total{scope}`. The store-wide
    numbers use the same predicates the per-agent `.../inbox/stats` counters
    already apply, so they are the sum of those, not a second definition of
    "queued".
  - Both 429 lanes now shed through one sliding-window implementation
    (`internal/ratelimit`), so "how long do I wait" has exactly one answer in
    the codebase.
- **Detection & containment** (CR-FEAT-030) — the other half of attribution, and
  it is opt-in (`CR_DETECT_ENABLED`, default `false`; with the flag unset no
  route is registered, no file is written and the delivery path is unchanged):
  - an **append-only, ed25519-signed delivery log** (`CR_DETECT_LOG`,
    `CR_DETECT_KEY`) — one record per delivery outcome (who sent what to whom,
    when, what the bus decided), hash-chained, fsynced, keyed from a file so it
    survives restarts, and **refused at startup** if a record was edited,
    deleted or signed by another key. `GET /delivery-log` pages the window and
    reports the true total; `GET /delivery-log/verify` re-reads the file and
    names the first record that does not verify.
  - **behaviour baselines with alerts** — `fanout_spike` (N distinct targets in
    a window), `new_peer_burst` (N first-ever conversations), `odd_hour_volume`
    (N messages inside the quiet window) and `canary_trip`, each one alert per
    agent per window, each with its thresholds and evidence at `GET /alerts`.
  - a **single-call kill-switch** (`POST /agents/{id}/kill-switch`) — pause the
    outbound webhook lane (dropping what was queued), release the leases the
    agent held, quarantine it (both directions refused with
    `403 AGENT_QUARANTINED`) and remove its registry row, with every action
    reported separately so a partial containment cannot read as a clean one.
  - **canary tokens** (`CR_CANARY_TOKENS`, or two generated per boot and served
    at `GET /canaries`) — a delivery addressed to a canary id, or carrying a
    canary token in its payload, trips `canary_trip`.
  - Acceptance, captured live against the real server wiring:
    `TestDetectionCatchesAndContainsACompromisedAgent` fans a scripted
    compromised agent out to five new peers, captures the alert, contains it in
    one call, proves the enforcement and then proves the log survives a restart
    with its chain intact. Design authority: `specs/DETECTION.md`. Source: the
    external review `DISPATCH · CRI-001` by Carter.
- **Task ownership: idempotency keys, expiry receipts, a dead-letter
  destination, and transfer/reassign** (CR-FEAT-025) — the four gaps the
  external review (`DISPATCH · CRI-001`, by Carter) named under "the lease is
  the lock", closed inside the existing lease model rather than beside it.
  `POST /agents/{id}/inbox` now takes an optional `idempotency_key`: a repeated
  key for the same agent inside `CR_IDEMPOTENCY_WINDOW_S` (default 24h) is
  answered with the original accept — same id, same status,
  `"idempotent_replay":true` — and stores nothing, so a sender that retried
  after losing the response no longer duplicates the target's work (a blocking
  webhook delivery replays the reply its single endpoint call produced; a
  rejected delivery records nothing, so a corrected retry is delivered rather
  than answered with a replay of the rejection). A message that expires
  unacknowledged now produces exactly one `MESSAGE_EXPIRED` receipt in its
  SENDER's inbox — the same durable error-notification shape as `WEBHOOK_FAILED`
  and `FEDERATION_FAILED`, which is why the sender is now stored WITH the
  message — and the message itself is preserved in a dead-letter destination,
  `GET /agents/{id}/inbox/dead-letters` (newest first, bounded by 7-day
  retention on a persisting backend and by capacity in memory, keyed by message
  id so a message is dead-lettered once and reported once, and NOT tied to the
  agent row so it survives the registration it was addressed to).
  `POST /agents/{id}/inbox/transfer` rebalances a stuck lease: messages move to
  another inbox UNLEASED and immediately claimable, under their own lease (409
  and nothing moved otherwise), with `force:true` as the explicit override for a
  holder that is gone. Migration `006_add_task_ownership` adds the two
  provenance columns to `inbox_entries` and the `dead_letters` table; the
  in-memory and PostgreSQL backends implement the same `PurgeReporter`,
  `DeadLetterStore` and `Transferrer` capabilities, and `PurgeExpired` keeps
  its count-only contract for any backend that does not.
- **`crier keygen`** (CR-FEAT-027) — the signing ceremony, replaced. The server
  binary now generates an ed25519 keypair in-process (no openssl, no xxd),
  writes it as a PKCS#8 PEM at mode `0600` in the same shape
  `openssl genpkey -algorithm ED25519` writes, and prints the agent config: the
  agent id, the hex public key, the exact `POST /agents` body and the next
  command. Before printing anything it re-reads the file, parses it with the
  server's own key loader and signs+verifies a sample payload, so the printed
  public key is provably the file's key; `-force` is required to overwrite an
  existing private key, and `-json` prints the same facts machine-readably. The
  external review (`DISPATCH · CRI-001`) named the openssl+xxd+sig()-helper
  ceremony as the number-one adoption killer, and it is what the last three
  testers each tripped over.
- **First-party clients** (CR-FEAT-027) — `clients/python/crier_client.py`
  (stdlib only, no packages to install) and `clients/typescript/crier.ts`
  (Node 22.6+, no packages either) expose the bus as methods: `register`,
  `deliver`, `retrieve`, `ack`, `stats`, `publish`, `subscribe`, plus
  `unregister` and a `request()` escape hatch. Both build the `X-Agent-ID` /
  `X-Agent-Ts` / `X-Agent-Sig` trio for you, decode inbox payloads, and surface
  the server's own error text. Runnable transcripts
  (`clients/python/round_trip.py`, `clients/typescript/round-trip.ts`,
  `clients/python/two_agents.py`) complete the round-trip and prove the
  signatures are enforced with negative controls — an unsigned call and a
  wrongly-signed call must both be refused. The Python signer prefers the
  `cryptography` package when present and otherwise uses a bundled RFC 8032
  implementation verified against the RFC 8032 §7.1 vectors and byte-identical
  to the library-backed path.
- **`make client-roundtrip-check`** (CR-FEAT-027) — the acceptance drive for the
  above: it builds the server, starts it on a port the shared port selector
  chooses, asserts it owns that port, runs `crier keygen`, both clients, both
  signing backends, the two-agent exchange and a second server with
  `CR_AUTH_TOKEN` (refusing an unauthenticated round-trip and accepting an
  authenticated one), and reaps the servers on exit. It also runs in CI.
- **The release front door: prebuilt binaries + a curl-to-install path**
  (CR-FEAT-028) — `gh release view v0.1.0-rc2` answered `assets: []`: the
  Release object existed and carried notes only, so the only way in was
  `git clone` plus a Go toolchain plus `make build`. An external hands-on review
  (`DISPATCH · CRI-001`, by Carter) filed that as a tester-funnel problem. Every
  release now carries 8 assets — `crier` and `crier-mcp` cross-compiled for
  linux/amd64, linux/arm64 and darwin/arm64, the installer, and a `SHA256SUMS`
  manifest. `scripts/release-artifacts.sh` (and `make release-artifacts`) builds
  them with `CGO_ENABLED=0 -trimpath`, stripped, stamping the same
  `internal/buildinfo` identity the other build paths stamp; it re-verifies the
  manifest and runs the host artifact's `-version` before returning, so a set
  whose identity did not land is never left on disk. `scripts/install.sh` is the
  one-line front door (`curl -fsSL …/install.sh | sh`): it detects the platform,
  verifies both binaries against that manifest and installs them into
  `$HOME/.local/bin`, and it REFUSES an unverified download — no checksum tool,
  no manifest entry for the binary, or a digest that does not match installs
  nothing, and there is deliberately no flag to skip the check. `make release`
  builds the asset set for the tag it cuts, and `make release-upload` attaches it
  to the Release object (creating it from the version's own changelog section and
  the compare link when it does not exist yet) — the publish step that used to be
  a hand-written, forgettable `gh release create`, which is how rc2 shipped
  without binaries. `make install-path-selftest` is the acceptance drive: it
  builds the set, serves it as a release tree, installs into a clean box whose
  `PATH` carries a `go` shim that exits 127 (so "no toolchain" is proven
  positively, not assumed from an absent `go`), runs the installed binary through
  `/health`, `/version`, a registration and an inbox delivery, and proves both
  unverified-download shapes are refused by name.

- **A2A interoperability, as an opt-in extra** (INT-A2A-001/002/003) — crier
  can be reached by an [A2A](https://a2a-protocol.org) client **without becoming
  an A2A-first system**: the whole option is behind `CR_A2A_ENABLED` (default
  `false`) *and* a per-agent `a2a` block, and with either half absent nothing
  A2A-related is registered — the route table, response bodies, auth
  requirements and storage schema are exactly what they were. The binding
  decision is recorded in `specs/A2A-OPTION.md` (JSON-RPC 2.0 over HTTP with SSE
  is implemented; gRPC and HTTP+JSON/REST are declared permitted-but-absent):
  - **`GET /.well-known/agent-card.json?agent_id=<id>`** serves the Agent Card
    projected from that agent's registry row — no card store, no cache, `404`
    for an id that is not an opted-in row, `400` when the request names no
    agent, and `Cache-Control: private` + a body-hashed `ETag` for conditional
    GETs.
  - **`POST /a2a`** is the JSON-RPC binding. `SendMessage` translates an A2A
    message into crier's **existing** delivery path — it calls
    `POST /agents/{id}/inbox`'s own handler, so the guard, sender idempotency
    (an A2A `messageId` becomes the deduplication key), the detection layer,
    federation hold/retry, webhook push, the durable inbox and the lease/ack
    lifecycle all apply unchanged — and answers a `Task` (accepted: inbox,
    queued push, or a federation hold with a status message that says so) or a
    direct `Message` (a target that answered inline). `SendStreamingMessage`
    answers `text/event-stream`, carrying the task's own inbox lifecycle
    (submitted → working → completed/failed, closing on the terminal state) and
    every event published to the task's relay topic — an SSE **adapter** over
    the existing relay subscription, with the WebSocket path untouched. The part
    model maps faithfully in both directions: A2A `text`/`raw`/`url`/`data` plus
    `metadata.alt`/`tags`/`caption` ⇄ crier message parts with `alt`/`tags`/
    `caption` and reference-or-inline file parts (a `raw` part gains the size and
    SHA-256 of its decoded bytes; a `url` part stays a reference), and a
    round-trip test proves a multi-part message survives to a crier consumer
    with `alt`/`tags` intact. Errors use the specification's own JSON-RPC codes
    and detail objects (`google.rpc.BadRequest` fieldViolations,
    `google.rpc.ErrorInfo`), and a refusal raised by the delivery engine carries
    crier's own status and body verbatim so a client can see exactly what crier
    said. What is deliberately NOT here: the task lifecycle operations
    (`GetTask`, `ListTasks`, `CancelTask`, `SubscribeToTask`), the
    push-notification configs and the extended card (INT-A2A-004..006) — each
    answers `-32601 MethodNotFoundError` naming the row that lands it rather
    than pretending.

## [0.1.0-rc2] - 2026-09-21

### Added

- Release tooling (`RELEASE-001`): a `make release VERSION=vX.Y.Z` target that
  refuses anything but a clean `main` with a free `vX.Y.Z` tag, runs
  `make build && make lint && make test-short` and creates the annotated tag
  `crier VERSION` — printing the push commands for the configured remotes
  instead of pushing them; this changelog; and `docs/releases.md`.

## [0.1.0-rc1] - 2026-08-21

First release candidate — the feature-complete core carried by tag
`v0.1.0-rc1` (commit `aa96076`), packaged with an external-tester kit.

### Added

- **Relay (pub/sub)** — HTTP and WebSocket topic publish/subscribe with
  in-memory fan-out, a per-agent rate limiter keyed on `X-Agent-ID`, and
  at-most-once delivery to live subscribers (a publish to a topic with no live
  subscriber is accepted and dropped).
- **WebSocket mesh (peer-to-peer)** — agent-to-agent mesh connections with the
  framing, message types and correlation contract in
  [docs/mesh-protocol.md](docs/mesh-protocol.md).
- **Agent registry** — register, list and delete agents with their hex ed25519
  public keys and capability lists; agent-scoped endpoints require per-agent
  request signatures (`X-Agent-ID`, `X-Agent-Ts`, `X-Agent-Sig` over
  `METHOD\nPATH\nTS`) by default.
- **Durable inboxes** — deliver, signed retrieve and ack with a per-message
  `ttl_seconds` (24h default; `0` never expires and is reported as
  `"expires_at": null`), plus an optional PostgreSQL backend
  (`CR_DATABASE_URL`) so agents, their webhook/guard configuration and
  undelivered messages survive a server restart.
- **MCP bridge** (`cmd/crier-mcp`) — registry and inbox tools exposed over stdio
  MCP, in local and remote mode (`docs/specs/ci-007-mcp-server.md`).
- **Webhook delivery** — a target's configured webhook bypasses its inbox:
  blocking (`200`, the endpoint's reply returned to the sender) and queued
  `async`/`batch` (`202`) with retry, HMAC outbound signing, a dead-target
  circuit breaker, and exactly one durable `WEBHOOK_FAILED` in the sender's
  inbox when the retries are exhausted
  ([specs/WEBHOOK-DELIVERY.md](specs/WEBHOOK-DELIVERY.md)).
- **Message guard (LLM)** — prompt-injection classification at the delivery
  choke point (`CR_GUARD_ENABLED`, on by default): structured verdicts,
  fail-open with per-policy fail-closed, `X-Crier-Guard-*` response headers,
  provider failover and opt-in kanban cards
  ([specs/LLM-MESSAGE-GUARD.md](specs/LLM-MESSAGE-GUARD.md)).
- **Federation** — relay-to-relay links (`CR_FED_LINKS`) that forward a delivery
  for an unknown agent to each linked relay in order, hold and retry transient
  link failures for a named sender, share a link secret (`CR_FED_TOKEN`) and
  list peers on `GET /fed/peers` (CR-FEAT-006).
- **Single-source build identity** — `internal/buildinfo` stamped through
  `-ldflags`, surfaced identically by `./bin/crier -version`, `GET /version` and
  the startup log.
- **Observability** — opt-in Prometheus exposition on `GET /metrics`
  (`CR_ENABLE_METRICS`), the `GET /status` runtime posture document, and
  profiling.
- **PostgreSQL persistence** for the registry and inboxes (CI-003b).
- **Tester kit** — [TESTERS.md](TESTERS.md) (per-mode checklist), the
  docker-compose quickstart under `examples/agent-ecosystem`, and the committed
  E2E battery `scripts/e2e-battery.sh` (24 gates).
- **Review gates** — the gitreins Tier-1 pre-commit battery (secrets / build /
  lint / tests) plus the committed arms it does not cover: `gofmt` drift
  (DF-CRIER-189), tracked shell scripts and workflow YAML (DF-CRIER-206),
  Makefiles and Dockerfiles (DF-CRIER-209), the documented MCP launcher's stdout
  contract (DF-CRIER-137), and the prose claims executed by `make docs-check`
  (CR-GAP-055).

### Notes

- Gates measured at the tag point (`aa96076`): `go build`/`go vet` green, full
  suite 18/18 packages, gitreins Tier 1 PASS on both work commits, committed E2E
  battery 24/24 twice.
- v0.1.0-rc1 was cut by hand; `make release` (see [Unreleased](#unreleased))
  encodes that sequence as of the next cut.

[Unreleased]: https://github.com/crier-dev/crier/compare/v0.1.0-rc2...HEAD
[0.1.0-rc2]: https://github.com/crier-dev/crier/compare/v0.1.0-rc1...v0.1.0-rc2
[0.1.0-rc1]: https://github.com/crier-dev/crier/releases/tag/v0.1.0-rc1
