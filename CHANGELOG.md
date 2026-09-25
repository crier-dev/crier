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
