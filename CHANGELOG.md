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
