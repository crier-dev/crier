# Crier — Agent-to-Agent Message Bus

[![Go Version](https://img.shields.io/badge/Go-1.26.6%2B-00ADD8?logo=go)](https://go.dev)
[![CI](https://github.com/crier-dev/crier/actions/workflows/ci.yml/badge.svg)](https://github.com/crier-dev/crier/actions/workflows/ci.yml)
[![bunker-e2e](https://github.com/crier-dev/crier/actions/workflows/bunker-e2e.yml/badge.svg)](https://github.com/crier-dev/crier/actions/workflows/bunker-e2e.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**Communication backbone for the autonomous agent economy** — extracted and
generalized from Hivemind. Crier lets bots, scripts, and AI agents exchange
messages over topics, direct mesh RPC, durable inboxes, webhooks, and
bus-to-bus federation — self-hosted, in one Go binary.

> **Testing crier?** Start with [TESTERS.md](TESTERS.md) — a per-mode checklist,
> the known rough edges, and how to report. A **static** spec browser ships at
> **/docs** on any running server: one self-contained HTML page (no CDN, no
> Swagger-UI — it works offline) that links the machine-readable
> **/openapi.json** and **/openapi.yaml**. It is *not* an interactive request
> console — fire your requests with curl or any HTTP client, as TESTERS.md §1 does.

![The crier fleet — agents on their own platforms, passing glowing messages along luminous paths](docs/img/agents-wide.png)

<p align="center"><i>The fleet: every agent gets a mailbox, a voice, and a place in the mesh.
Meet the mascots → <a href="#who-its-for">builder · home · business · courier</a></i></p>

Crier provides four primitives for agent communication:

## Architecture

### 1. Relay (Pub/Sub)

Central message relay. Agents publish events to topics; subscribers receive them over WebSocket.

- HTTP + WebSocket transport
- Topic-based routing with subscriber counts
- Every subscription frame names the literal published topic:
  `{"topic":"<published topic>","event":<event>}` — the event is delivered
  exactly as published (never string-encoded), and the topic field is what lets
  a wildcard subscriber know which topic matched
- Publishing is fire-and-forget: `POST /relay/publish` returns 202 as soon as the event is accepted, and if the topic has **zero subscribers** the event is dropped by design — publishes to empty topics are not queued or retained. A topic only appears in `GET /relay/topics` after at least one subscriber connects (subscribe first, then publish; a publish to an empty topic does not create it)
- Rate limiting per agent (100 events/minute) — requires the `X-Agent-ID` header when enabled; `0` disables both
- Bearer token authentication

### 2. WebSocket Mesh (Peer-to-Peer)

Direct agent-to-agent communication layer with discovery, keepalive, and request/response correlation.

- Peer discovery via registry
- One-way REGISTER on connect (fire-and-forget; the server never sends REGISTER_ACK — see `docs/mesh-protocol.md`)
- 30-second keepalive loop — a heartbeat is also LIVENESS EVIDENCE for the sender's registry row (see [§3](#3-agent-registry))
- Concurrent request/response with timeout tracking
- Clean shutdown with WebSocket close frames

### 3. Agent Registry

Every agent has a discoverable identity with capability cards.

- Register/unregister with ed25519 public key
- List and detail endpoints, with a `status` derived from mesh heartbeats (see note below)
- Capability-based routing (future)

> **What `status` means — heartbeat-derived presence.** `status` is DERIVED per
> read from the row's liveness evidence (`last_seen`) and ONE documented window
> (`CR_PRESENCE_STALE_AFTER_S`, default `90` seconds = three missed mesh
> heartbeats):
>
> | Reported | When |
> |----------|------|
> | `online` | liveness evidence inside the window |
> | `stale` | no evidence inside the window (or none ever) — a crashed agent stops looking alive |
> | `offline` | the row STATES it is offline (nothing in the server sets this; a stated `offline` outranks the derivation) |
>
> The evidence is: a mesh socket **accepted** for that agent
> (`/mesh/connect/{agentID}` — someone presented itself as that id), a
> `KEEPALIVE` heartbeat on that socket (the 30s loop every shipped mesh client
> runs), or a successful signed `PATCH /agents/{id}`. `last_seen` is the instant
> of the most recent one of those — nothing else refreshes it.
>
> The derivation is READ-TIME ONLY: no sweeper runs and nothing is stored, so the
> row's `status` column keeps the stored `online`/`offline` values it always had,
> and a dead agent goes `stale` because its evidence AGES rather than because
> something rewrote the row. A window shorter than the keepalive interval makes a
> live agent flap to `stale` between beats, so the server warns at startup if you
> set one. An agent that reconnects is `online` again on the accept, with no
> manual intervention.
>
> Two limits, stated rather than left to be discovered. First, this is a MESH
> presence statement, not a process-liveness probe: an agent that never opens a
> mesh socket and never PATCHes reports `stale` once the window passes even while
> its process is healthy — delivering to it, retrieving its inbox or publishing
> as it does not refresh the row, because those paths cannot attribute the caller
> to the agent as confidently as a socket handshake can. Second, the mesh does NOT
> disconnect a peer that stops heartbeating: it keeps its connection and stays
> listed by `GET /mesh/peers` — only its registry row goes stale. For
> live-connection truth with no window at all, read the connection table instead:
> `GET /mesh/peers` lists agents with an open socket (see
> [Try the Mesh](#try-the-mesh)).

### 4. Inboxes

Durable per-agent FIFO queues with lease-based delivery. The documented configuration is the durable one — [Run](#run) starts PostgreSQL and passes `CR_DATABASE_URL`, and then agents (including their `webhook` and `guard` configs) and undelivered messages survive server restarts. Run with no `CR_DATABASE_URL` and the in-memory backend is used instead, which is **demo-only**: the registry and every inbox live in process memory and are gone when the process exits. The backend actually serving is readable at `GET /status` (`"registry_backend"`).

- Lease prevents double-delivery: messages are leased for N seconds on retrieval
- ACK confirms delivery; un-ACKed messages return to queue after lease expiry
- TTL expiry auto-purges stale messages (default 24h; optional per-message `ttl_seconds`, `0` = never expires, reported on the wire as `"expires_at":null`)
- Every message is leased to exactly one retriever; a concurrent retriever receives only what the first did not lease (`queue_depth`/`leased_count` on the retrieve body disambiguate an empty `messages` array: `leased_count` > 0 means HELD, not lost — DF-CRIER-177)

**Waking a sleeping agent (CR-FEAT-023).** Durable inboxes are poll-only by history, so the one lane that promises durability was the one lane that could not wake a sleeping agent — a 60-second poll loop learned about urgent work up to a minute late. Both halves of the fix are additive, and neither changes lease, ack or TTL behaviour:

- **Long-poll:** `GET /agents/{id}/inbox?wait_seconds=N` (0..120, default `0`) parks the read and answers the moment a message is claimable, or — when the budget expires — with the SAME empty body a poll-only read gets, so a client treats the two identically and simply re-polls. Absent or `0` is today's read, byte-for-byte: no timer, no subscription. A budget outside 0..120, or a non-integer, is a `400` naming the parameter (a documented parameter is honored or rejected, never silently ignored — DF-CRIER-180). A delivery through the relay's deliver endpoint wakes a parked read immediately; inbox writes that bypass it (the federation hold queue, webhook-failure notices, a second relay process on the same store) surface within about a second. Errors are never waited out: an unregistered agent still answers `404` at once.
- **New-message ping:** an agent that cannot hold a request open — or would rather not — can connect to the mesh with `ws://…/mesh/connect/{agentID}?inbox_notify=1` and receive one `INBOX_NOTIFY` frame on that socket per delivery into its inbox, naming the message id and the sender and carrying no payload. It is opt-in per connection (a client that did not ask receives nothing), best effort (no queue, no retry, no ack — the durable read remains the contract), and ignored if echoed back. Wire format: [`docs/mesh-protocol.md`](docs/mesh-protocol.md) §INBOX_NOTIFY.

- **Task ownership (CR-FEAT-025).** The lease answers "who owns this message *now*"; it says nothing about a sender's retry, and a TTL expiry used to be a silent delete. Four surfaces close that, all inside the lease model:
  - **Sender idempotency keys.** `POST /agents/{id}/inbox` accepts an optional `idempotency_key`. A second delivery of the same key to the same agent within the deduplication window (`CR_IDEMPOTENCY_WINDOW_S`, default 24h) is answered with the ORIGINAL accept — same `id`, same status, `"idempotent_replay":true` — and stores nothing (a blocking webhook delivery replays the reply its single endpoint call produced), so a sender that retried after losing the response does not duplicate work. Only an accept is replayed: a rejected delivery records nothing, so a corrected retry under the same key is delivered rather than answered with a replay of the rejection. The key is also recorded on the stored message, so a dead letter can name the key that produced it.
  - **Expiry receipts.** A message that expires unacknowledged produces exactly one `MESSAGE_EXPIRED` receipt in its SENDER's inbox — the same durable error-notification shape as `WEBHOOK_FAILED`/`FEDERATION_FAILED`: `{"kind":"error","code":"MESSAGE_EXPIRED","message_id":…,"target":…,"expires_at":…,"dead_lettered":true,"dead_letter_path":"/agents/{id}/inbox/dead-letters",…}`. It is a direct store write, never routed through webhook or federation delivery, so it cannot recurse.
  - **A dead-letter destination.** The message itself is preserved: `GET /agents/{id}/inbox/dead-letters?limit=N` (1..100, default 20) lists the messages that expired in that inbox, newest first, with the payload that was delivered, the sender, the expiry that elapsed and the reason. The record is keyed by message id (dead-lettered once, reported once) and is NOT tied to the agent row — a dead letter outlives the registration it was addressed to, which is exactly when it matters. A persisting backend sweeps records older than 7 days on the same pass that produces them; the in-memory backend keeps the most recent 1024.
  - **Transfer / reassign.** `POST /agents/{id}/inbox/transfer` moves messages out of a STUCK lease into another agent's inbox, where they land UNLEASED and immediately claimable — the alternative is waiting out the lease and racing every other consumer. A currently-leased message must be moved under its own `lease_id` (`409` otherwise, and nothing moves); an unleased one needs no lease; `force:true` is the explicit operator override for a holder that is gone. Messages move as a unit (an unknown id moves nothing) and keep their id, payload, creation time and expiry.

### 5. Webhook delivery (bypasses the inbox)

An agent that registers a `webhook` config (`PATCH /agents/{id}` with
`{"webhook":{"url":…}}`) receives its messages at that endpoint instead of its
durable inbox (CR-FEAT-001, [`specs/WEBHOOK-DELIVERY.md`](specs/WEBHOOK-DELIVERY.md) §4).

That PATCH is an agent-scoped endpoint: with signature enforcement on (the
default, `CR_REQUIRE_AGENT_SIG=true`) it requires the same
`X-Agent-ID`/`X-Agent-Ts`/`X-Agent-Sig` trio as inbox retrieve/ack — sign
`PATCH\n/agents/{id}\n<unix-seconds>` with the agent's ed25519 key, exactly as
in the [Quick Start](#quick-start) signing helper. Sent unsigned it answers
`401 {"error":"missing agent signature headers (X-Agent-ID, X-Agent-Ts, X-Agent-Sig)"}`.

That `401` is only the first of the checks a signed request passes through, and
the ORDER is part of the contract (measured at HEAD on a registered agent and on
an unregistered id): a missing trio is the `401` quoted above; an **unregistered**
agent id is `404 {"error":"agent not found"}` — the existence check runs once the
trio is present and BEFORE the timestamp and signature checks, so a negative-path
test against an unknown id must expect `404`, not `401`; a timestamp outside the
±30s window is `401 {"error":"request timestamp outside allowed window (±30s)"}`;
a signature that does not verify is `401 {"error":"signature verification failed"}`.
Step 4 of the [Quick Start](#quick-start) carries the reproducible pair
(`GET /agents/ghost-agent-xyz/inbox` → `404`, the same call on a registered id → `401`).

The inbox is **not** written: `GET /agents/{id}/inbox` for a webhook-configured
agent is empty by design, so an empty retrieve is not evidence that a message
was never sent — the message may have gone to the endpoint (or failed there).
The same holds for leased messages on a non-webhook agent: an empty `messages`
array with `leased_count` > 0 means another retriever holds an unexpired lease
(the message returns after lease expiry or ack), not that it was never
delivered (DF-CRIER-177).

The deliver accept names the transport, so a sender never has to infer it from
the status code (DF-CRIER-157):

| Accept | `transport` | `delivery_mode` | Meaning |
|--------|-------------|-----------------|---------|
| `200` | `webhook` | — | blocking: the endpoint's reply is in `reply` |
| `202` | `webhook` | `async` \| `batch` | accepted for **webhook delivery, queued — not stored** |
| `201` | `inbox` | — | stored in the durable inbox (`expires_at` present: RFC 3339, or `null` when it never expires) |

A `202` is a promise about the queue, not about delivery: the endpoint can
still fail afterwards. A blocking delivery the endpoint permanently rejects
(a non-retryable 4xx, or a 2xx whose body the agent's reply schema cannot map)
answers `502` — retrying the identical message cannot succeed. Only a timeout
or an exhausted budget answers `504`, where a later attempt can still land.

**Recovery when an async/batch delivery dies.** After the queue exhausts its
bounded retries (`CR_WEBHOOK_MAX_RETRIES` failing attempts, or the endpoint's
own smaller `webhook.retries` budget, one per `CR_WEBHOOK_REDELIVER_S` tick —
5 failed attempts / 30s by default, i.e. roughly two to three minutes after the
accept), the item is dropped and
exactly one durable notification is written into the **sender's own inbox**. It
is a direct store write: it never routes back through webhook delivery, so it
cannot recurse — even when the sender itself is a webhook-configured agent.

```json
{"kind":"error","code":"WEBHOOK_FAILED","message_id":"<original>","target":"<recipient agent>","retries":6,"status_code":503,"error":"status 503"}
```

`status_code` is omitted and `error` carries the transport error string when the
failure was transport-level (no HTTP response). The notification is
best-effort and never retried: a missing `sender` on the rejected message, or an
unregistered sender, is logged instead. While an endpoint is degraded (circuit
opened after `CR_WEBHOOK_CIRCUIT_THRESHOLD` consecutive failures) queued
redeliveries pause instead of POSTing and resume when a probe
(`CR_WEBHOOK_PROBE_S`) succeeds, so a poisoned endpoint can take much longer
than the default cadence to reach exhaustion.

## Quick Start

### Install the prebuilt binary (no Go toolchain)

Every release publishes 8 assets: cross-compiled `crier` and `crier-mcp` for
linux/amd64, linux/arm64 and darwin/arm64, plus the installer and a `SHA256SUMS`
manifest. One line fetches the pair for this box, verifies each against that
manifest, and installs both into `$HOME/.local/bin` — nothing to compile:

```bash
curl -fsSL https://raw.githubusercontent.com/crier-dev/crier/main/scripts/install.sh | sh
```

Pin a release instead of the newest, or install somewhere else (`sh -s --`
passes the flags through the pipe):

```bash
curl -fsSL https://raw.githubusercontent.com/crier-dev/crier/main/scripts/install.sh | sh -s -- --version v0.1.0-rc3 --dir "$HOME/bin"
```

The installer REFUSES an unverified download: a manifest with no entry for the
binary, a digest that does not match, or a box with neither `sha256sum` nor
`shasum` is a loud failure that installs nothing — there is deliberately no flag
to skip the check. It needs `curl` (or `wget`) and nothing else; every artifact
is a static binary (`CGO_ENABLED=0`), so it does not have to match the libc of
the machine that built it. Confirm what landed:

```bash
crier -version        # crier v0.1.0-rc3-<commit> — the tag's own build identity
crier -port 8767      # the server; crier -help prints every flag
```

Tester? [TESTERS.md](TESTERS.md) §1 starts from this same install. Prefer to
build? Everything below is the from-source path, and it produces the same binary
with the same identity: the release assets are built by
`scripts/release-artifacts.sh`, which stamps `internal/buildinfo` exactly as
`make build` does (a `make docs-check` claim fails if either path stops
stamping).

### Prerequisites

- Go 1.26.6 or later
- **Docker with the Compose plugin — for the durable path, which is the one this
  README documents** (`docker compose up -d postgres`, step 1 of [Run](#run)).
  Any PostgreSQL 14+ server works instead of the compose service: point
  `CR_DATABASE_URL` at it and skip that one line. Start without either and crier
  falls back to the in-memory backend, which is **demo-only** — see [Run](#run)
- **Nothing else for the normal path**: `crier keygen` and the first-party clients
  need no OpenSSL and no `xxd` — the keypair is generated in-process and the
  signature is built by the client library (see
  [Try it](#try-it) and [clients/README.md](clients/README.md))
- OpenSSL 3.x or later with `xxd` on PATH — **only for the hand-written curl
  recipes** and the `sig()` helper below, which use `openssl pkeyutl -sign -rawin`,
  an OpenSSL 3+ flag. On older OpenSSL the helper fails loudly instead of signing
  (see below). That flag is also a ONE-SHOT operation: the payload must be a
  seekable file (the helper writes it and signs it with `-in`), because a piped or
  redirected payload makes `pkeyutl` fail with a zero-byte signature — the helper
  refuses that too, instead of sending it. If the box has no `xxd` at all (a fresh
  Debian install does not), **Installing xxd without root** below is the recipe —
  it needs no root, exactly like the Go install that follows

#### Installing Go without root

A bare machine with no root has no Go at all, and the distro package may be older
than this repo requires — install the official tarball into your home directory.
Unpack it somewhere other than `$HOME/go`: the archive unpacks a single `go/`
directory, and `$HOME/go` is the default `GOPATH` (see the warning below).

```bash
mkdir -p "$HOME/sdk" && cd "$HOME/sdk"
curl -LO https://go.dev/dl/go1.26.6.linux-amd64.tar.gz   # linux-arm64 on an ARM machine
tar -C "$HOME/sdk" -xzf go1.26.6.linux-amd64.tar.gz      # -> $HOME/sdk/go/bin/go
export PATH="$HOME/sdk/go/bin:$PATH"
go version                                               # go version go1.26.6 linux/amd64
```

The archive is named `go<version>.linux-<arch>.tar.gz` and always unpacks one
`go/` directory. `go1.26.6` is this repo's minimum; the current stable release is
`go1.27.1`, and any later version works the same way — download the one you want
and pass that same file name to `tar`. `export PATH` lasts for one shell, so make
it permanent in your shell rc (`$HOME/.bashrc` for bash, `$HOME/.profile`, or
`$HOME/.zshrc` for zsh):

```bash
echo 'export PATH="$HOME/sdk/go/bin:$PATH"' >> "$HOME/.bashrc"
```

> **Do not set `GOPATH` to the Go installation directory.** `GOROOT` is the
> toolchain (`$HOME/sdk/go` above) and `GOPATH` is your workspace; pointing one at
> the other is the classic fresh-install mistake, and the toolchain flags it on
> every command. Captured verbatim on go1.26.6 — it prints your own absolute path
> in place of `$HOME`:
>
> ```
> $ export GOPATH="$HOME/sdk/go"
> $ go env GOROOT GOPATH
> warning: both GOPATH and GOROOT are the same directory ($HOME/sdk/go); see https://go.dev/wiki/InstallTroubleshooting
> ```
>
> **The fix: do not set `GOPATH` at all.** With the toolchain under `$HOME/sdk/go`
> the default `GOPATH` is `$HOME/go` — a workspace, distinct from the toolchain —
> and the warning is gone. The collision needs no env var either: untar the
> archive into `$HOME` and it unpacks to `$HOME/go`, which IS the default `GOPATH`,
> so `GOROOT` and `GOPATH` are the same directory from the very first command. On
> go1.26.6 this is a warning rather than a fatal error (run the two commands above
> to see your own toolchain's wording), but it is not cosmetic: with `GOBIN` unset
> the toolchain then installs into itself — `go install` writes the binary into the
> toolchain's own `bin` beside `go` and `gofmt`, and the module cache lands under
> `GOROOT`. The Go wiki page the warning links to states the rule directly: the
> `GOPATH` directory should not be set to, or contain, the `GOROOT` directory.
> Measured clean: toolchain at `$HOME/sdk/go` with `GOPATH` unset (recommended,
> as above), or the toolchain under `$HOME` with `export GOPATH="$HOME/gopath"`.
>
> **Already unpacked the archive into `$HOME` (so the toolchain IS `$HOME/go`)?**
> Then "do not set `GOPATH` at all" does not help you — `$HOME/go` IS the default
> `GOPATH`, so both live on the same directory from the very first command, with
> no env var set either way. Move your workspace off the toolchain with the
> one-liner below and the warning is gone (belt-and-braces for anyone who would
> rather not move the toolchain):
>
> ```bash
> export GOPATH="$HOME/gopath"                            # this shell
> echo 'export GOPATH="$HOME/gopath"' >> "$HOME/.bashrc"  # every shell
> ```
>
> Measured on go1.26.6: the warning above appears whenever `GOROOT` and `GOPATH`
> resolve to the same directory (`GOPATH="$(go env GOROOT)" go env GOROOT GOPATH`
> prints it), and the same command prints no warning at all with `GOPATH` set to
> `$HOME/gopath`. Either fix works — a toolchain under `$HOME/sdk/go` as in the
> recipe above, or this export; you do not need both.

A distro package — `sudo apt install golang-go` on Debian/Ubuntu, `sudo dnf install golang` on Fedora — is a one-line alternative, but the packaged Go can be older than the 1.26.6 this repo requires: check `go version` afterwards.

#### Installing xxd without root (Debian 13)

`xxd` is the other half of the quickstart, and a fresh box usually has none: the
signing helper hex-encodes the ed25519 public key and signature with `xxd -p`,
and `examples/demo.sh` refuses to start without it (`ERROR: xxd required (hex
encoding)`, exit 1) — so an xxd-less machine fails the first documented demo
before any crier code runs. `xxd` is not in coreutils and not in a minimal Debian
install.

**The obvious guess is wrong on Debian 13 (trixie):** `vim-common`, the package
`xxd` is usually said to come from, contains **no `xxd` binary** there. Measured
against Debian's own trixie package file listings, `vim-common`'s payload is
`etc/vim/vimrc`, `usr/bin/helpztags`, mime/desktop entries, icons and man pages —
no `xxd` anywhere in it — while the `xxd` package's payload carries the binary at
`usr/bin/xxd` (plus its man pages and copyright). It is its own package, and
fetching plus unpacking it needs no root, exactly like the Go install above:

```bash
mkdir -p "$HOME/extract-xxd" && cd "$HOME/extract-xxd"
apt-get download xxd                      # no root: downloads the .deb only
dpkg -x xxd_*.deb "$HOME/extract-xxd"     # unpack in place, still no root
export PATH="$HOME/extract-xxd/usr/bin:$PATH"
printf 'crier' | xxd -p                   # sanity: prints 6372696572
```

`apt-get download` only writes the `.deb` into the current directory (the name it
reports is the one to unpack, `xxd_*.deb`); it installs nothing and changes no
system file. `dpkg -x <deb> <dir>` unpacks a package into a directory without
touching the system package database, so the binary lands at
`$HOME/extract-xxd/usr/bin/xxd` — no `dpkg -i`, no `sudo`, no root anywhere in
the recipe. As with `go`, `export PATH` lasts for one shell; make it permanent in
your shell rc:

```bash
echo 'export PATH="$HOME/extract-xxd/usr/bin:$PATH"' >> "$HOME/.bashrc"
```

Check the result rather than trusting a package name — `command -v xxd` names the
path, and `xxd -p < /dev/null` exits 0 printing nothing on a working binary. Not
every release ships it under this exact name: when one answers `E: Unable to
locate package xxd` (the message apt prints for a package it cannot find), locate
that release's own `xxd` package — its package file listing names it, as does
`dpkg -S` on a box that already has the binary (it prints the owning package and
its path) — instead of reaching for `vim-common`. The `dpkg -x` step is unchanged
either way.

### Build

```bash
make build
```

Or build directly:

```bash
go build -o bin/crier ./cmd/server
```

`make build` stamps the build identity (`version`, `commit`, `build_time`) into
the binary via `-ldflags`. A build with no ldflags at all — the bare `go build`
above — still reports the git commit, because `internal/buildinfo` falls back to
the VCS metadata the Go toolchain embeds. Ask a binary or a running server what
it is:

```bash
./bin/crier -version          # crier v1.2.3-1a2b3c4d   (TAGGED build: tag v1.2.3 at commit 1a2b3c4d)
curl -s localhost:8767/version # {"version":"1.2.3","commit":"1a2b3c4d",...}
```

The `v1.2.3-1a2b3c4d` line is what a TAGGED build prints, so it is not what a
fresh untagged clone shows. One source (`internal/buildinfo`) and one format
(`v<version>-<commit>[-dirty]`, the `v` glued on only for a real stamped
version) produce these, and no artifact of one checkout can report a different
identity — all 6 build paths that compile a crier binary (`make build`,
`make build-mcp`, the two cross-compile lines in `scripts/release-artifacts.sh`
that produce the release assets, `Dockerfile`, `Dockerfile.mcp`) stamp it, and a
`make docs-check` claim fails if one of them stops:

| Build | `-version` prints | Version segment comes from |
|-------|-------------------|----------------------------|
| `make build`, tagged commit | `crier v1.2.3-1a2b3c4d` | the tag, via `git describe --tags` |
| `make build`, untagged checkout | `crier v<describe>-<commit>`, e.g. `crier v9c74185-dirty-9c741850` | `git describe --always --dirty`: the short commit, `-dirty` when the tree had uncommitted changes |
| bare `go build -o bin/crier ./cmd/server` | `crier dev-<commit>`, e.g. `crier dev-9c741850-dirty` | nothing stamped, so the version segment is the `dev` sentinel — rendered bare, never `vdev` — and the commit comes from the Go toolchain's VCS metadata |
| `docker build .` / `make docker-build` | the same string `make build` prints for the same tree | the image build derives the same `git describe` values and stamps them; `--build-arg VERSION=… COMMIT=… BUILD_TIME=…` overrides |

(`<describe>` and `<commit>` are placeholders — the shas shown are one example
checkout, so yours will differ; the shape is the stable part.)

The MCP server carries no identity of its own either: `crier-mcp --version`
prints the full identity, and its `initialize` result answers
`serverInfo.version` with the version segment of that same identity (never a
literal version), so a client and the CLI can never disagree about which build
is running.

Override the version with `make build VERSION=1.2.3`; `make build` with no
override uses `git describe`.

### Make a keypair — `crier keygen`

The server binary is also the keypair tool, so the two can never disagree about
what a key looks like:

```bash
./bin/crier keygen -out alice.key -id alice   # -> alice.key (PKCS#8 PEM, mode 0600) + the agent config
./bin/crier keygen -h                         # -out, -id, -server, -json, -force
```

It generates an ed25519 keypair in-process (**no openssl, no xxd, no pip
install**), writes the private key as a PKCS#8 PEM file — the same shape
`openssl genpkey -algorithm ED25519` writes, so openssl can still read it and an
existing key keeps working — and prints the agent id, the hex public key, the
exact `POST /agents` body and the command that finishes the job. `-json` prints
the same facts as one machine-readable object for an orchestrator.

Before it prints anything it re-reads the file from disk, parses it with the
**server's own key loader**, and signs and verifies a sample payload with the
parsed key — so a printed public key that does not match the file cannot happen.
It refuses to overwrite an existing key unless you pass `-force`, because a
keypair is not regenerable: an agent registered with the old public key could
never sign again.

### Run

**The documented path is the durable one.** Crier's inbox is durable by design,
so the first two commands start the PostgreSQL backend and then point the server
at it — there is no backend decision to make:

```bash
docker compose up -d postgres
CR_DATABASE_URL='postgres://crier:crier@localhost:5437/crier?sslmode=disable' make run
```

`CR_DATABASE_URL` is the switch that selects the PostgreSQL backend (precedence:
`CR_DATABASE_URL` → `DATABASE_URL` → `CRIER_DATABASE_URL`); migrations apply
automatically on startup, and agents — including their `webhook` and `guard`
configs — and undelivered inbox messages then survive a restart of the server.
`GET /status` names the backend actually serving, so the posture is readable as
`"registry_backend":"postgres"`.

**The in-memory backend is demo-only.** Start the server with no
`CR_DATABASE_URL` (a bare `./bin/crier` or `make run`) and the registry and every
inbox live in process memory: restart the process and the agents and any
undelivered messages are gone. That is fine for a throwaway demo, a unit test or
a five-minute look at the API — it is NOT the configuration this README
documents, and it is not what to hand a tester who expects an inbox to keep
their message until they read it.

On a shared host the default port is often already held by a leftover server
from an earlier session — and the compose host port (5437) can be held too.
Check both before starting:

```bash
ss -tlnp | grep :8767         # who holds the default port? (empty output = free)
ss -tlnp | grep :5437         # who holds the postgres host port?
```

If it is taken, start on a free port and confirm which build answered —
`GET /version` is one of the five unauthenticated paths:

```bash
# Free port for the server instead of the default 8767:
CR_DATABASE_URL='postgres://crier:crier@localhost:5437/crier?sslmode=disable' CRIER_PORT=8768 ./bin/crier
curl -s localhost:8768/version # {"version":"...","commit":"..."}

# Host port 5437 taken as well? Move the container, and follow it with the URL:
CRIER_PG_HOST_PORT=5493 docker compose up -d postgres
CR_DATABASE_URL='postgres://crier:crier@localhost:5493/crier?sslmode=disable' ./bin/crier
```

If the bind fails anyway, the server exits non-zero naming the port, the
holder-check command and the `-port`/`CRIER_PORT` alternative, plus the build
identity of the binary that failed to start.

### Stop / restart

```bash
make stop
```

`make run` starts the server with a pidfile (`.crier.pid` at the repo root —
gitignored; override with `PIDFILE=<path>` for `make run`/`make stop`, or set
`CR_PIDFILE`). The pidfile is written only after the port is actually bound —
a failed bind leaves no pidfile — and records the pid, the port and the
absolute binary path. `make stop` reads it, verifies the recorded pid is
still running that exact binary (via `/proc/<pid>/exe`), and sends one
SIGTERM, which triggers the server's graceful shutdown. With no pidfile
`make stop` is a clean no-op success, so it is safe in any state:

```bash
$ make stop
./bin/crier -stop -pidfile .crier.pid
crier: nothing to stop — no pidfile at .crier.pid
```

A stale pidfile (the server died without cleanup) is removed and reported;
a pidfile whose pid now runs a *different* binary is refused with both
paths printed and **nothing is signalled** — the stop command never
SIGKILLs and never matches by port or process name, so it cannot kill an
unrelated process.

The pidfile is JSON (`{pid, port, binary}`), not a bare pid, so it cannot be
fed to `kill`: `kill $(cat .crier.pid)` hands bash the literal `{` and bash
answers `kill: '{': not a pid or valid job spec`. Read the field yourself, or
just use `make stop` / `-stop`, which parse the JSON for you and check
ownership first:

```bash
kill $(jq -r '.pid' .crier.pid)   # works — jq extracts the pid field
kill $(cat .crier.pid)            # does NOT work — the file is JSON, not a pid
```

A failed start says which case you are in. When the bind fails *and* a pidfile
exists at the configured path, the failure line reports what that file
records: `pidfile_state=live` with the recorded pid and the exact stop command
when that process is still serving (the restart did not take the port over),
or `pidfile_state=stale` when the recorded pid is gone — nothing to stop, and
the port belongs to someone else. A failed takeover is therefore visible in
its own output, not only in the plausible-looking file it left behind.

Lost the launcher? A server started detached (`setsid make run …`, then the
launcher exits) keeps holding its port — that server is exactly what
`make stop` reaches, because the pidfile names the server process, not the
launcher:

```bash
$ setsid make run &        # launcher exits; server keeps running
$ make stop
./bin/crier -stop -pidfile .crier.pid
crier: stopping pid 4169877 (SIGTERM)
crier: pid 4169877 stopped
```

Older servers started without a pidfile (before this existed) are not
reachable by `make stop`. Find them by port and stop them manually —
SIGTERM (the default `kill`) and Ctrl-C both trigger the same graceful
shutdown:

```bash
ss -tlnp | grep :8767         # who holds the default port?
kill <pid>                    # SIGTERM — graceful shutdown
```

> **The LLM message guard is ON by default.** Every inbound delivery is
> classified by a guard LLM (default model `deepseek-v4-flash`, 10s
> per-message budget — `CR_GUARD_TIMEOUT_MS`) before it is webhook-POSTed
> or inbox-stored. For local dev without an API key, set
> `CR_GUARD_ENABLED=false`; to exercise the guard, set `DEEPSEEK_API_KEY`.
> Without a key the guard call fails (`reason: guard_error: all providers
> failed: no provider api key`) and the guard fails OPEN — the delivery
> proceeds, the deliver response carries `"guard":{…,"errored":true}` and the
> outbound webhook POST carries `X-Crier-Guard-Error: true`. Note the over-cap
> path is deterministic: it runs regardless of the guard LLM's health, so a
> keyless deployment still gets it. If a workload legitimately sends
> machine-generated bodies above the cap, raise
> `CR_GUARD_MAX_PAYLOAD_BYTES`, or silence the noisy class per policy
> (`"checks":{"masquerade":false}` — the class's prematch patterns are then
> suppressed); extra patterns appended via `CR_GUARD_PATTERNS_EXTRA` are
> high-confidence (able to block an over-cap payload) unless they declare
> `"confidence":"low"`, which opts them into the report-only treatment.
> See
> [Message guard (LLM)](#message-guard-llm).

### Try it

Before the curl recipes below, note that **you do not need any of the signing
ceremony they describe**. Registering an agent used to mean an openssl keypair,
a DER-offset recipe to extract the public half, and a hand-written `sig()` shell
helper; that is the step external testers keep tripping over (`DISPATCH ·
CRI-001`, the dogfood xxd trap), and it is now optional the way a manual
transmission is optional to a car:

```bash
./bin/crier keygen -out alice.key -id alice   # no openssl, no xxd, no hex surgery
# …prints the agent id, the public key, the exact POST /agents body, and what to run next.

docker compose up -d postgres                 # durable backend (see Run above)
CR_DATABASE_URL='postgres://crier:crier@localhost:5437/crier?sslmode=disable' make run

python3 clients/python/round_trip.py --server http://localhost:8767 --id alice --key alice.key
node    clients/typescript/round-trip.ts --server http://localhost:8767 --id alice --key alice.key
```

Those two scripts are the whole signed round-trip — register, deliver, signed
retrieve, signed ack, signed stats, relay publish/subscribe — with a printed
transcript and two negative controls (an unsigned call and a wrongly-signed call
must both be refused), so the green means the signatures were verified rather
than ignored. The clients are stdlib-only Python and dependency-free Node, and
`clients/README.md` documents the method surface and the wire contract.
`make client-roundtrip-check` runs both of them, plus a two-agent exchange and a
bearer-token arm, against a server it starts itself — with no openssl and no xxd
anywhere.

The curl recipes that follow are the same round-trip by hand. They are the
protocol reference — every header the clients set is spelled out — and they are
what the docs gate executes on every build, so they stay true. If you are here to
*use* crier rather than to inspect it, use `crier keygen` and a client and read
the rest later.

A minimal register → deliver → retrieve round-trip with the default signed configuration. If you started the server with `CR_AUTH_TOKEN` set (auth enabled), every request except the **five exempt paths** — `/health`, `/version`, `/openapi.json`, `/openapi.yaml`, `/docs` (the list is the switch in `internal/middleware/auth.go`) — needs the Bearer header shown below; if `CR_AUTH_TOKEN` is unset, auth is disabled and the header can be dropped. Measured on a running server: all five answer `200` with no token, and `GET /agents` answers `401`:

```bash
AUTH=(-H "Authorization: Bearer ${CR_AUTH_TOKEN:-}")

# 0. One-time setup: generate an ed25519 keypair for agent-1 (needs openssl 3.x + xxd)
openssl genpkey -algorithm ED25519 -out /tmp/crier-agent.key >/dev/null 2>&1
PUBKEY_HEX=$(openssl pkey -in /tmp/crier-agent.key -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64)
# sig helper: hex(ed25519_sign("METHOD\nPATH\nTS", key)) — same wire format as examples/demo.sh.
# Requires OpenSSL >= 3 for pkeyutl -sign -rawin; on older OpenSSL it errors loudly
# instead of producing an empty (silently-401-rejected) signature. That flag is also a
# ONE-SHOT operation: the payload must be a seekable file passed with -in, so the helper
# refuses a zero-byte signature rather than sending it (a piped payload fails with
# "unable to determine file size for oneshot operation" and would 401 blaming the headers).
sig() { if ! openssl pkeyutl -help 2>&1 | grep -q -- '-rawin'; then echo "ERROR: this signing helper requires OpenSSL >= 3 (pkeyutl -sign -rawin); found $(openssl version)" >&2; return 1; fi; printf '%s\n%s\n%s' "$1" "$2" "$3" > /tmp/crier-payload.txt; _sig=$(openssl pkeyutl -sign -rawin -inkey /tmp/crier-agent.key -in /tmp/crier-payload.txt 2>/dev/null | xxd -p -c 128); if [ -z "$_sig" ]; then echo "ERROR: signing produced an EMPTY signature. pkeyutl -sign -rawin is a one-shot operation and needs a SEEKABLE payload passed with -in <file> — a piped or redirected payload fails with 'unable to determine file size for oneshot operation' and yields zero bytes, which the server rejects 401 naming the empty X-Agent-Sig header." >&2; return 1; fi; printf '%s\n' "$_sig"; }

# 1. Register an agent (public_key = hex-encoded ed25519 public key; required
#    whenever signature enforcement is on — the default. Only a server run
#    with CR_REQUIRE_AGENT_SIG=false accepts registration without it.)
curl -s -X POST localhost:8767/agents "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d "{\"id\":\"agent-1\",\"public_key\":\"${PUBKEY_HEX}\",\"capabilities\":[\"demo\"]}"
# 201 — but a SECOND register with the same id answers 409 {"error":"agent already
# registered: \"agent-1\""}: idempotency-refused, the existing agent and its key are
# untouched (DF-CRIER-288). Re-running the quickstart is safe — skip the register
# step, or DELETE /agents/agent-1 first to start over with a fresh key.

# 2. Deliver a message to its inbox. `ttl_seconds` is optional: absent keeps the
#    24h default, 0 means the message never expires and is reported as
#    "expires_at":null (the key is present, the value is null — never the zero
#    time). `transport` in the body is
#    always present: "inbox" here, "webhook" when the target has a webhook
#    configured (then the inbox is bypassed — see §5 of the architecture notes).
curl -s -X POST localhost:8767/agents/agent-1/inbox "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"payload":{"hello":"world"},"ttl_seconds":3600}'
# 201 {"id":"...","transport":"inbox","expires_at":"..."}  (expires_at = created_at + ttl_seconds)

# 3. Publish to a relay topic (pub/sub). `X-Agent-ID` is the header the relay
#    requires — the per-agent rate limiter keys on it and a publish without it
#    is 401. The signature trio is NOT verified on this endpoint (measured at
#    HEAD: a stale `X-Agent-Ts` and a bogus `X-Agent-Sig` both still answer
#    202); it is enforced on the agent-scoped endpoints in steps 4-6, which is
#    why the shared `sig` helper is used here too. A publish to a topic with no
#    live subscriber still answers 202 and drops the event (see §1).
TS=$(date +%s)
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:8767/relay/publish "${AUTH[@]}" \
  -H 'Content-Type: application/json' -H 'X-Agent-ID: agent-1' \
  -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: $(sig POST /relay/publish "$TS")" \
  -d '{"topic":"demo-topic","event":{"hello":"subscribers"}}'
# 202 — drop the trio and it is still 202; drop `X-Agent-ID` and it is 401.

# 4. Retrieve — agent-scoped endpoints require per-agent request signatures by
#    default (CR_REQUIRE_AGENT_SIG=true). This covers inbox retrieve/ack/stats
#    AND DELETE /agents/{id}. Headers:
#      X-Agent-ID  agent id
#      X-Agent-Ts  unix seconds (must be within ±30s of the server clock)
#      X-Agent-Sig hex ed25519 signature over "METHOD\nPATH\nTS"
#    Timestamps are generated fresh below — never hardcode them (stale timestamps
#    are rejected with 401).
#    Those four checks run in this order, so the FIRST one to fail names the status:
#      1. signature trio present?  no -> 401 "missing agent signature headers (X-Agent-ID, X-Agent-Ts, X-Agent-Sig)"
#      2. agent exists?            no -> 404 "agent not found"
#      3. timestamp within ±30s?   no -> 401 "request timestamp outside allowed window (±30s)"
#      4. signature valid?         no -> 401 "signature verification failed"
#    The agent EXISTENCE check (2) sits between the trio check and the timestamp /
#    signature checks, so an UNREGISTERED agent id answers 404 "agent not found"
#    — never 401 — as soon as a signature trio is present. The stale-timestamp 401
#    in (3) is measured on a REGISTERED agent (agent-1): an unknown id never gets
#    that far.
#    Reproduce both sides — same call, same id in X-Agent-ID and in the path, a
#    fresh X-Agent-Ts and a well-formed 128-hex X-Agent-Sig that cannot verify:
#      GET /agents/agent-1/inbox         -> 401 "signature verification failed"
#      GET /agents/ghost-agent-xyz/inbox -> 404 "agent not found"
TS=$(date +%s)
curl -s localhost:8767/agents/agent-1/inbox "${AUTH[@]}" \
  -H 'X-Agent-ID: agent-1' -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: $(sig GET /agents/agent-1/inbox "$TS")"
# 200 {"messages":[{"id":"...","payload":"eyJoZWxsbyI6IndvcmxkIn0=","lease_id":"..."}],"lease_id":"...","queue_depth":1,"leased_count":1}
# queue_depth/leased_count disambiguate an empty batch: messages=[] with
# leased_count>0 = everything queued is HELD under a lease (DF-CRIER-177)
# Note: message payloads are base64-encoded on the wire ([],byte form)

# 5. Ack the message — message_ids is REQUIRED (an ack without it is rejected
#    with 400: it would otherwise be a silent no-op and the message would be
#    redelivered after lease expiry). Sign "POST\n/agents/agent-1/inbox/ack\n<ts>".
TS=$(date +%s)
curl -s -X POST localhost:8767/agents/agent-1/inbox/ack "${AUTH[@]}" -H 'Content-Type: application/json' \
  -H 'X-Agent-ID: agent-1' -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: $(sig POST /agents/agent-1/inbox/ack "$TS")" \
  -d '{"lease_id":"<lease_id from retrieve>","message_ids":["<id from retrieve>"]}'
# 204 — message permanently removed (never redelivered after lease expiry)

# Dev shortcut: disable signing for trusted single-user setups. The durable
# backend is still the documented one; drop the CR_DATABASE_URL assignment and
# the server runs the demo-only in-memory backend instead.
CR_REQUIRE_AGENT_SIG=false CR_DATABASE_URL='postgres://crier:crier@localhost:5437/crier?sslmode=disable' make run
# With signing disabled, POST /agents no longer needs a public_key either —
# register with just an id:
#   curl -s -X POST localhost:8767/agents -d '{"id":"agent-1"}'        # 201
#   (a duplicate id answers 409 "agent already registered" — see step 1)
# A keyless agent registered this way is unusable on a server that enforces
# signing (its agent-scoped calls answer 401 "no registered public key"), so
# re-enabling CR_REQUIRE_AGENT_SIG later means re-registering with a key.
curl -s localhost:8767/agents/agent-1/inbox
# 200 — no signature headers required

# 6. Delete the agent — DELETE /agents/{id} requires the same per-agent
#    signature (not just inbox endpoints). Sign "DELETE\n/agents/agent-1\n<ts>".
TS=$(date +%s)
curl -s -X DELETE localhost:8767/agents/agent-1 "${AUTH[@]}" \
  -H 'X-Agent-ID: agent-1' -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: $(sig DELETE /agents/agent-1 "$TS")"
# 204 — agent removed (401 without the signature headers)
```

> Prefer the runnable script: [`examples/demo.sh`](examples/demo.sh) performs the
> full register → deliver → signed retrieve → ack round-trip with an ephemeral
> ed25519 keypair (openssl 3.x). Start the server, then run `./examples/demo.sh`.
>
> The **relay subscribe leg** — which needs a WebSocket client and cannot be done
> with `curl` alone — has its own zero-install driver:
> `bash examples/ws-mesh-demo/run-demo.sh`. It starts a relay on a scratch port,
> subscribes on an exact topic, and asserts the fan-out (including that a publish
> without `X-Agent-ID` is 401). Nothing to install; see [Try the Mesh](#try-the-mesh)
> below for the mesh half of the same script.

### Remote MCP mode (crier-mcp)

`crier-mcp` bridges MCP clients (Claude Code, Cursor, …) to a running Crier
server instead of an in-process store: set `CRIER_HTTP_URL` (plus
`CRIER_AGENT_ID`) and every registry/inbox tool call becomes a signed HTTP
request against that server.

Because the server enforces per-agent signatures by default
(`CR_REQUIRE_AGENT_SIG=true`), the bridge signs every request with an ed25519
key. It registers its own identity on startup, so there is no manual
registration step: start the server and run the bridge.

```bash
# Run the MCP bridge against the remote server (all four vars are read at startup)
export CRIER_HTTP_URL=http://localhost:8767
export CRIER_AGENT_ID=mcp-agent
# Recommended for any long-lived bridge: a stable key whose public half the
# server keeps across restarts. Optional — see "Signing keys" below.
openssl genpkey -algorithm ED25519 -out ~/.config/crier/mcp-agent.key
export CRIER_AGENT_PRIVATE_KEY_FILE=$HOME/.config/crier/mcp-agent.key
# Optional shared bearer token, only when the server runs with CR_AUTH_TOKEN:
# export CRIER_AUTH_TOKEN=...
# CRIER_AUTH_TOKEN is the bridge's own name; CR_AUTH_TOKEN is accepted as an
# alias for the same shared secret (setting only CR_AUTH_TOKEN works — the
# bridge logs which variable it used, and warns if both are set to different
# values, in which case CRIER_AUTH_TOKEN wins).
make build-mcp && ./bin/crier-mcp
```

The launcher's stdout carries only JSON-RPC frames — the build's own diagnostics go
to stderr — so a strict stdio client may launch that line as-is; `make mcp` does the
same in a single command.

**Automatic registration (no operator step).** On startup in remote mode
crier-mcp registers `CRIER_AGENT_ID` on the server with its public key and the
`mcp`/`bridge` capability tags, then logs the outcome. Registration is
idempotent — an existing identity is left untouched, so re-running is a no-op.
This matters because the bridge polls *its own* inbox for replies
(`get_messages`, `ask_agent`): if the server never heard of that agent id, the
reply leg fails with `agent not found` (a 404). A registration failure (server
unreachable, …) is logged at error level and does **not** abort startup, so a
harness already running is never killed by a bootstrap step.

**Signing keys.** Each request carries a fresh `X-Agent-Ts` / `X-Agent-Sig`
signing `METHOD\n<path>\n<unix-seconds>` — the query string is excluded.

- `CRIER_AGENT_PRIVATE_KEY_FILE` must contain a PKCS#8 PEM ed25519 private key
  (`openssl genpkey -algorithm ED25519`); anything else (unreadable file, wrong
  format, RSA/EC key) is an explicit startup error. Key material is never
  logged or echoed.
- **Unset, the bridge generates an ephemeral ed25519 key for that run** and
  signs with it — enough to work against a signed server on a fresh in-memory
  (demo-only, non-durable) server, and enough for `CR_REQUIRE_AGENT_SIG=false`
  servers (which ignore the signature entirely).
- The ephemeral key is regenerated on every run, while a persistent server
  keeps the public key registered by the **first** run. Later runs therefore
  find the id already registered with a *different* key and log an error:
  every signed request would be rejected with 401. Remedies — set
  `CRIER_AGENT_PRIVATE_KEY_FILE` to the private key whose public half is
  registered for `CRIER_AGENT_ID`, or delete the stale agent
  (`curl -X DELETE localhost:8767/agents/mcp-agent`) and restart, or point the
  bridge at a fresh in-memory server (the demo-only, non-durable backend). Set
  the variable for any long-lived bridge.

#### MCP tools

The MCP server exposes **13 tools** (measured live via a `tools/list` stdio
exchange). The argument names below are the properties of each tool's
`InputSchema` in `internal/mcp/server.go`; `*` marks a required argument.

| Tool | Arguments |
|------|-----------|
| `register_agent` | `id`*, `public_key` (hex ed25519, 64 chars; required whenever signature enforcement is on — the default. Only an MCP bridge on a server run with `CR_REQUIRE_AGENT_SIG=false` accepts registration without it), `capabilities` (string array, default `[]`) |
| `list_agents` | _none_ |
| `get_agent` | `id`* |
| `unregister_agent` | `id`* |
| `deliver_message` | `agent_id`*, `payload`* (JSON object) |
| `retrieve_inbox` | `agent_id`*, `max_messages` (int 1-100, default `10`), `lease_seconds` (int 1-3600, default `30`) |
| `ack_messages` | `agent_id`*, `lease_id`*, `message_ids`* (string array, min 1) |
| `inbox_stats` | `agent_id`* |
| `send_message` | `agent_id`*, `payload`*, `reply_to` (optional correlation id) |
| `get_messages` | `max` (int 1-100, default `10`) |
| `ask_agent` | `agent_id`*, `payload`*, `timeout_s` (default `30`, max `300`) |
| `mesh_peers` | _none_ |
| `mesh_request` | `target`*, `method`*, `path`*, `body` (opaque JSON), `timeout_ms` (default `15000`) |

Unknown arguments are rejected: a member of `arguments` that is not one of the
tool's declared properties is a normal tool error naming it — `invalid
arguments: unknown argument "max_messges"` — instead of being dropped and
silently replaced by a default. The contract covers the top-level `arguments`
object only: the value of an opaque `payload` (`deliver_message`,
`send_message`, `ask_agent`) or `body` (`mesh_request`) is passed through
untouched, so what lives inside it is the caller's business (DF-CRIER-190).

##### What each tool needs

`tools/list` is the only metadata an MCP client sees before it calls a tool, so
every tool whose handler needs environment says so in its own description, and
`crier-mcp` logs **one line at startup** naming the tools that cannot work in the
mode it started in (`mode`, `tools`, `available_tools`, `unavailable_tools`,
`missing_env`). "Needs" is the environment the handler checks before it does
anything; "bridge scope" is the server-side rule for a remote bridge — the
bridge presents its own identity (`CRIER_AGENT_ID`) on every agent-scoped
request, and a server running with signature enforcement on
(`CR_REQUIRE_AGENT_SIG=true`, the default) answers 403
`agent "<bridge>" may only access its own resources` for any other agent.

| Tool | Needs | Notes |
|------|-------|-------|
| `register_agent`, `list_agents`, `get_agent`, `deliver_message`, `send_message` | _nothing_ | work in every mode; delivery and reads of the registry are not restricted to the bridge's own agent |
| `retrieve_inbox`, `ack_messages`, `inbox_stats` | _nothing_ | bridge scope: on a remote bridge `agent_id` must be the bridge's own identity (403 otherwise) |
| `unregister_agent` | _nothing_ | bridge scope: same rule — `id` must be the bridge's own identity on a remote bridge |
| `get_messages` | `CRIER_AGENT_ID` | reads the bridge's own inbox and acks what it returns; fails immediately without it |
| `ask_agent` | `CRIER_AGENT_ID` | sends to another agent, but the reply is read from the bridge's own inbox |
| `mesh_peers` | `CRIER_HTTP_URL` | queries `GET /mesh/peers` on that server |
| `mesh_request` | `CRIER_MESH_URL`, `CRIER_AGENT_ID` | the bridge opens its own WebSocket connection only when both are set |

With no environment at all — the default in-process stdio mode — four tools
cannot work, and startup says exactly which:

```text
INFO MCP tool surface: some advertised tools cannot work in this mode — set the missing environment variables to enable them mode=in-process tools=13 available_tools=9 unavailable_tools="[get_messages ask_agent mesh_peers mesh_request]" missing_env="[CRIER_AGENT_ID CRIER_HTTP_URL CRIER_MESH_URL]"
```

##### A worked stdio session

Every tool is a JSON-RPC frame on stdin and one response line on stdout — no MCP
client, no server, no environment variables, because an unconfigured `crier-mcp`
serves the same 13 tools from an in-process store. The two tools below are from
the _nothing_ row above (`register_agent`, `list_agents`), so this whole session
is copy-pasteable as-is; a tool whose row names environment needs it first.

```bash
make build-mcp && ./bin/crier-mcp <<'EOF'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"hand-rolled","version":"0.1.0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"register_agent","arguments":{"id":"demo-agent","public_key":"db4b1d3b0e4a7f9c2d5e8a1b4c7f0e3d6a9b2c5e8f1a4b7c0d3e6f9a2b5c8e1f","capabilities":["demo"]}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_agents","arguments":{}}}
EOF
```

Observed stdout — one line per request, each carrying the `id` it answers
(`notifications/initialized` is a notification with no `id`, so five frames
produce four lines):

```text
{"jsonrpc":"2.0","result":{"protocolVersion":"2024-11-05","serverInfo":{"name":"crier-mcp","version":"d25fdf9"},"capabilities":{"tools":{}}},"id":1}
{"jsonrpc":"2.0","result":{"tools":[{"name":"register_agent","description":"Register a new agent with an ed25519 public key and optional capabilities. …","inputSchema":{"type":"object","properties":{"id":{…},"public_key":{…},"capabilities":{…}},"required":["id"]}}, … 12 more …]},"id":2}
{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"id\":\"demo-agent\",\"public_key\":\"db4b1d3b0e4a7f9c2d5e8a1b4c7f0e3d6a9b2c5e8f1a4b7c0d3e6f9a2b5c8e1f\",\"capabilities\":[\"demo\"],\"status\":\"online\",\"registered_at\":\"2026-09-19T14:48:01.146463081-05:00\",\"last_seen\":\"2026-09-19T14:48:01.146463081-05:00\"}"}]},"id":3}
{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"agents\":[{\"id\":\"demo-agent\",\"public_key\":\"db4b1d3b0e4a7f9c2d5e8a1b4c7f0e3d6a9b2c5e8f1a4b7c0d3e6f9a2b5c8e1f\",\"capabilities\":[\"demo\"],\"status\":\"online\",\"registered_at\":\"2026-09-19T14:48:01.146463081-05:00\",\"last_seen\":\"2026-09-19T14:48:01.146463081-05:00\"}]}"}]},"id":4}
```

The `tools/list` line is elided — the real frame is ~6 KB, every one of the 13
entries carrying its full `description` and `inputSchema` — but the tool names
and their order are verbatim. Two sample values are per-run: the `public_key`
above is demo data (substitute your own when this bridge talks to a signed
server), and `serverInfo.version` is the version half of the build identity of
the tree that built the binary — `./bin/crier-mcp --version` prints `v` + that
same version, followed by the commit it was built from
(`vd25fdf9-<commit>`, and `-dirty` while the tree has uncommitted changes).
Two shapes are worth noting before writing a client against this wire:

- a tool result is `result.content[0].text`, and that text is **a JSON string**,
  not a nested object — a client that reads `result.agents` finds nothing and
  has to parse the string a second time;
- a rejected call is still a `result`, flagged rather than raised: e.g.
  `register_agent` without `public_key`, on a server with signature enforcement
  on, answers `{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"public_key is required"}],"isError":true},"id":3}`
  instead of a JSON-RPC `error` object.

#### Which MCP surface to use

An MCP client can reach the server through more than one surface, and they
answer different needs.

**The curated bridge (`crier-mcp`)** exposes agent-messaging verbs — registry,
inbox and mesh — and adds the composites a conversational agent would otherwise
build itself: `ask_agent` delivers, polls and acks the reply in a single call,
and `mesh_request` owns the WebSocket round trip. It also keeps the lease/ack
bookkeeping and identity registration out of the harness. Use it for
conversational agents and mesh interactions.

**Muster's `openapi-mcp`, pointed at `docs/openapi.yaml`,** generates a generic
client over the whole REST surface — every operation the spec defines, whether
or not it is agent-shaped. Use it when raw coverage of every operation matters
more than an agent-optimised surface.

Both derive from `docs/openapi.yaml`, and the curated bridge's half of that
relationship is enforced by the coverage guard in `internal/mcp`
(`spec_drift_test.go`): every advertised tool must name the spec operations it
exercises, and every spec operation must be either covered by a tool or recorded
as a deliberate exclusion with its reason. Adding, renaming or removing either
side fails the build until the table is updated.

### Try the Mesh

The mesh is the second primitive: direct agent-to-agent WebSocket connections
over `GET /mesh/connect/{agentID}`. By default it needs no signing setup — the
agent id in the path is the peer identity, and that is enough on a single host —
but it does have a frame contract, and a client that gets the correlation wrong
hangs instead of erroring. On a shared or networked deployment set
`CR_REQUIRE_MESH_AUTH=true`: the server then challenges every connection and the
peer must sign the challenge with the ed25519 key the registry holds for that id
before it is admitted (see [Authentication](#authentication-opt-in)).

**The zero-install path is this repo's own demo.** It builds this server and a Go
WebSocket client (gorilla/websocket, already in `go.mod`), starts its own relay on
a scratch port and runs the whole exchange below live — REGISTER, REQUEST,
RESPONSE, and the KEEPALIVE frames a client must ignore — asserting the result:

```bash
bash examples/ws-mesh-demo/run-demo.sh                          # port chosen by the guard (first candidate 18961)
DEMO_KEEPALIVE_WAIT=0 bash examples/ws-mesh-demo/run-demo.sh    # skip the ~30s keepalive wait
```

It exits 0 only when the RESPONSE's `request_id` equals the REQUEST's
`message_id`, its `status_code` is what the responder sent, and a KEEPALIVE frame
that arrived on the same socket was ignored instead of being taken for the reply
— and, on the relay side of the same run, that a publish *without* `X-Agent-ID`
is `401`, that a publish to another topic reaches no subscriber, and that the
subscriber received exactly one frame naming the exact topic it subscribed to.
No external WebSocket client is needed for any of it: the peers and the
subscriber are this repo's own Go client.
Flags and the step-by-step transcript: `examples/ws-mesh-demo/README.md`.

**Manual alternative — any WebSocket client works.** Neither `websocat` nor
`wscat` ships with crier, so install one first (`cargo install websocat`, or Node
≥ 16 for `npx wscat -c <url>`). The agent ID in the path *is* the peer identity:
`ws://localhost:8767/mesh/connect/agent-1`. Two terminals, one frame each:

```bash
# Terminal A — connect as agent-1 and paste the REGISTER frame below. It is
# fire-and-forget: any RFC3339 timestamp works and the server never replies.
websocat ws://localhost:8767/mesh/connect/agent-1

# Terminal B — the peer is now visible (no token needed unless CR_AUTH_TOKEN is set):
curl -s localhost:8767/mesh/peers
# {"peers":[{"agent_id":"agent-1"}],"count":1}
```

A peer shows up as soon as the socket connects and stays listed while the
connection is open; close Terminal A and it disappears. Driving the *exchange* by
hand needs a loop that filters frames by `type`, so use
`bash examples/ws-mesh-demo/run-demo.sh` (or the Python worked example in
[`docs/mesh-protocol.md`](docs/mesh-protocol.md)) rather than pasting frames into
two `websocat` sessions.

#### The frames

Every frame is one JSON object in one WebSocket **text** frame with a trailing
newline (`json.Marshal` + `\n`, `internal/mesh/message.go:107`). These eight are
the whole wire contract — the five below every client sees, plus the three that
appear **only** on a server started with `CR_REQUIRE_MESH_AUTH=true` (see
[Authentication](#authentication-opt-in) before you build a client); every field
name exists in `internal/mesh/message.go` and only the marked values are yours
to generate:

<!-- mesh-frames:start -->
```json
{"type":"REGISTER","version":1,"message_id":"f0e1d2c3b4a5968778695a4b","timestamp":"2026-09-18T09:15:00.123456789-05:00","agent_id":"agent-1","lease_id":"","lease_ttl_ms":3600000,"capabilities":{"version":"0.1.0","topics":[],"max_concurrent_sessions":10}}
{"type":"REQUEST","version":1,"message_id":"9d8f0a1b2c3d4e5f6a7b8c9d","timestamp":"2026-09-18T09:15:01.123456789-05:00","source":{"agent_id":"agent-1"},"target":{"agent_id":"agent-2"},"method":"GET","path":"/ping","body":{"hello":"world"},"trace_id":"f1e2d3c4b5a69788796a5b4c","timeout_ms":5000}
{"type":"KEEPALIVE","version":1,"message_id":"7c6b5a493827160514233241","timestamp":"2026-09-18T09:15:31.123456789-05:00","lease_id":"","agent_id":"agent-1"}
{"type":"RESPONSE","version":1,"message_id":"3f2a1b0c9d8e7f6a5b4c3d2e","timestamp":"2026-09-18T09:15:01.234567890-05:00","request_id":"9d8f0a1b2c3d4e5f6a7b8c9d","source":{"agent_id":"agent-2"},"status_code":200,"body":{"pong":true},"trace_id":"f1e2d3c4b5a69788796a5b4c"}
{"type":"ERROR","version":1,"message_id":"e57206b16d39e6e28a01e286","timestamp":"2026-09-18T09:15:01.345678901-05:00","request_id":"9d8f0a1b2c3d4e5f6a7b8c9d","error":{"code":"CONTROLLER_OFFLINE","message":"peer agent-2 not connected"},"trace_id":"f1e2d3c4b5a69788796a5b4c"}
{"type":"AUTH_CHALLENGE","version":1,"message_id":"5b4c3d2e1f0a9b8c7d6e5f4a","timestamp":"2026-09-25T11:02:00.123456789-05:00","agent_id":"agent-1","nonce":"9f1c0a7e4b2d6835a0c1e9f7b4d2638a","expires_at":"2026-09-25T11:02:10.123456789-05:00"}
{"type":"AUTH_RESPONSE","version":1,"message_id":"2e1f0a9b8c7d6e5f4a3b2c1d","timestamp":"2026-09-25T11:02:00.234567890-05:00","agent_id":"agent-1","nonce":"9f1c0a7e4b2d6835a0c1e9f7b4d2638a","signature":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
{"type":"AUTH_OK","version":1,"message_id":"1d0e9f8a7b6c5d4e3f2a1b0c","timestamp":"2026-09-25T11:02:00.345678901-05:00","agent_id":"agent-1"}
```
<!-- mesh-frames:end -->

The ids above are literals so the correlation between the frames is visible; a
real client generates a fresh 24-hex `message_id` per frame (`openssl rand -hex
12`) and a `trace_id` for the REQUEST it can echo. What each frame's fields mean
and who fills them:

| Frame | Field | Filled by / meaning |
|-------|-------|---------------------|
| all | `type` | The message type: `REGISTER`, `REGISTER_ACK`, `KEEPALIVE`, `REQUEST`, `RESPONSE`, `ERROR`, plus `AUTH_CHALLENGE`, `AUTH_RESPONSE`, `AUTH_OK` on an authenticated mesh. Dispatch on this. |
| all | `version` | Protocol version, `1`. |
| all | `message_id` | Per-frame unique id (24 hex chars). Yours. **Not** the correlation field. |
| all | `timestamp` | RFC3339 with nanoseconds (Go `time.Time`). Yours; any parseable value works. |
| `REGISTER` | `agent_id` | The connecting agent — should match the id in the connect URL. |
| `REGISTER` | `lease_id` / `lease_ttl_ms` | Registry lease; empty / requested TTL on connect. |
| `REGISTER` | `capabilities` | `{version, topics[], max_concurrent_sessions}` — informational. |
| `KEEPALIVE` | `lease_id` / `agent_id` | Sender's lease (empty) and identity. No `request_id`. |
| `REQUEST` | `source` / `target` | `{"agent_id": "<id>"}` — you and the peer you address. `target.agent_id` is what the server routes on; a REQUEST without one is answered `INVALID_MESSAGE`. |
| `REQUEST` | `method` / `path` | Your application-level route (e.g. `GET` / `/ping`). The server does not interpret them. |
| `REQUEST` | `body` | Opaque JSON payload, omitted when empty. |
| `REQUEST` | `trace_id` | Yours; the responder echoes it in the reply. |
| `REQUEST` | `timeout_ms` | Yours; the server does not enforce it — your client does. |
| `RESPONSE` | `request_id` | **The `message_id` of the REQUEST being answered.** This is the correlation field. |
| `RESPONSE` | `source` | The answering peer. |
| `RESPONSE` | `status_code` | HTTP-style code from the responder. |
| `RESPONSE` | `body` | Whatever the responder wrote, relayed verbatim (see below). |
| `ERROR` | `request_id` | Same field, same rule: the failed REQUEST's `message_id`. |
| `ERROR` | `error` | `{code, message, retry_after_ms?}`; `CONTROLLER_OFFLINE` means the target peer is not connected, `INTERNAL` means the route table is full. |
| `AUTH_CHALLENGE` | `agent_id` / `nonce` / `expires_at` | Server → peer, the FIRST frame on a mesh with `CR_REQUIRE_MESH_AUTH=true`: the identity the connect URL claims, the single-use nonce to sign, and when it stops being accepted. |
| `AUTH_RESPONSE` | `agent_id` / `nonce` / `signature` | Peer → server: the hex ed25519 signature over `mesh-auth-v1\n<agent_id>\n<nonce>` made with the private key whose public half the registry holds for `agent_id`. The ONLY frame accepted before admission. |
| `AUTH_OK` | `agent_id` | Server → peer: the identity is verified and frames are admitted. The connection is not a peer — and is not listed by `GET /mesh/peers` — before this frame arrives. |

#### Authentication (opt-in)

On the default configuration (`CR_REQUIRE_MESH_AUTH` unset) nothing on this page
changes: the agent id in the connect URL *is* the peer identity, no challenge is
sent, and the frames above are all that crosses the wire. That is fine for a
single-host or otherwise trusted network, and it is what the shipped clients do
today.

On a shared, networked or multi-tenant deployment, set
`CR_REQUIRE_MESH_AUTH=true`. The server then proves the identity before it
trusts it, using the same ed25519 identity the registry already enforces on the
inbox lane:

1. the socket is upgraded but is **not** a peer yet — it is not in the peer
   table and `GET /mesh/peers` does not list it;
2. the server sends `AUTH_CHALLENGE` naming the path identity and a single-use
   nonce (32 hex chars, valid for `CR_MESH_AUTH_TIMEOUT_S`, default 10s);
3. the client answers `AUTH_RESPONSE` with a hex ed25519 signature over exactly
   `mesh-auth-v1\n<agent_id>\n<nonce>` — with `openssl`, that is
   `printf 'mesh-auth-v1\n%s\n%s' <agent_id> <nonce> > payload.txt` followed by
   `openssl pkeyutl -sign -rawin -inkey agent.key -in payload.txt | xxd -p -c 128`
   (OpenSSL ≥ 3; the payload must be a `-in <file>`, not a pipe);
4. the signature is verified against the public key the registry holds for that
   agent id. On success the server sends `AUTH_OK` and admits the connection; on
   any failure it answers `ERROR` with code `AUTH_FAILED` naming the reason and
   closes the socket. A client that sends any other frame first (its `REGISTER`,
   say — what a pre-fix client does) is refused the same way, and may still
   answer correctly on the same socket within the window.

Because the socket's identity is proven, two frame rules apply **only** on an
authenticated mesh, and they are what stop a verified peer from speaking as
someone else one layer up:

- a `REQUEST` whose `source.agent_id` is not the authenticated peer is refused
  `FORBIDDEN` and never forwarded — the target does not see it;
- a `RESPONSE`/`ERROR` may only be sent by the peer the request was addressed
  to; anything else is refused `FORBIDDEN` and is not passed to the requester.

**Migration note.** Turning this on is a client-side change, not a transparent
one: every agent that connects must hold the private half of the key registered
for it (`POST /agents` with `public_key`), and a keyless registration — legal
while `CR_REQUIRE_AGENT_SIG=false` — can never authenticate. Clients that do not
implement the handshake are refused with `AUTH_FAILED` naming the missing
`AUTH_RESPONSE`; the server logs the same refusal, and `GET /status` reports
`"mesh_auth_required": true` so the posture is visible before the first
connection. The Go client in this repo does the handshake when
`mesh.MeshAuthConfig.SigningKey` is set; the Python worked example at the end of
[`docs/mesh-protocol.md`](docs/mesh-protocol.md) signs with `openssl`.

The **origin policy** is separate and is also explicit: `CR_WS_ALLOWED_ORIGINS`
covers the relay, and the mesh has its own `CR_MESH_ALLOWED_ORIGINS` (unset =
every origin allowed, as before). When it is set, an upgrade that *carries* an
`Origin` header must name a listed origin (otherwise `403` before any frame),
while a client that sends no `Origin` at all — every agent client, including
this repo's — still connects. `/status` reports the mode as
`"mesh_origin_policy": "allow-all"` or `"allowlist"`.

#### Two rules a client must implement

**Correlate the reply on `request_id`, never on the frame's own `message_id` and
never on arrival order.** A responder MUST set `request_id` to the exact
`message_id` of the REQUEST it is answering (`internal/mesh/message.go:76`). The
server records `message_id → requester` when it forwards the REQUEST and looks
the reply up by `request_id` (`internal/mesh/peer.go` `forwardResponse`, `:402`);
a reply that does not echo it is dropped with no error and no log, and the caller
hangs until its own `timeout_ms`. `ERROR` frames correlate identically — they take
the same forwarding path, so the server's own `CONTROLLER_OFFLINE` answer also
carries your `message_id` in `request_id`. The reply's own `message_id` is a
fresh id of its own, as in the example above, so comparing *that* to your
REQUEST's id matches nothing: `request_id` is the only field that links a reply
to its request.

**`body` is relayed verbatim, and its JSON type is the responder's choice.** The
RESPONSE's `body` is held as raw bytes (`json.RawMessage`,
`internal/mesh/message.go:79`) and the frame is handed back unchanged, so an
object stays an object and a string stays a string — a responder that puts a JSON
*string* on the wire (Python's `json.dumps({...})` is the classic) sends a string,
and nothing on the server side rewrites it. `TestResponseBodyRelayedVerbatim` in
`internal/mesh` pins this over the relayed path.

**Read in a loop and dispatch on `type` — ignore `KEEPALIVE` frames while a reply
is outstanding.** The server sends a KEEPALIVE to every connected peer every
30 seconds (`KeepaliveInterval = 30 * time.Second` in
`internal/mesh/peer.go`; the value `cmd/server/main.go` runs the mesh with is
`mesh.DefaultMeshConfig("crier")`, and the accepted-connection loop is
`Mesh.AcceptPeer` → `keepaliveLoop`), and a client built on this repo's own mesh
package sends one on the same cadence and on the same socket its reply arrives on
(`Mesh.ConnectPeer` → `keepaliveLoop`). A raw WebSocket client does not have to
send any — nothing on the server requires a heartbeat, and a client that sends
none keeps the row its CONNECT earned and then goes `stale` in the registry once
the presence window passes (§3) — but every client has to read past them. So the
frame after your REQUEST is not necessarily the answer: read frames one at a
time, dispatch on `type`, and ignore everything that is not the `RESPONSE` you
are waiting for (correlated as above) or an `ERROR`. A KEEPALIVE carries no
`request_id` at all, which is what makes the filter safe — but a client that
treats "the next frame" as the answer reads a KEEPALIVE as a RESPONSE and sees
`status_code: 0`. The demo prints exactly this: the KEEPALIVE frame the server
sent to the requester (`"agent_id":"crier"` — the server's own mesh identity) and
the RESPONSE it accepted instead.

For the deeper reference — every message type, the error codes, the silent-drop
rules, a verified Python round-trip — see
[`docs/mesh-protocol.md`](docs/mesh-protocol.md).

### Test

```bash
# Full test suite
make test

# Short (no integration tests)
make test-short

# Vet
make lint
```

### The demo harnesses never measure a server they did not start

The three runnable harnesses (`examples/federation-demo`,
`examples/hermes-gateway-demo`, `examples/ws-mesh-demo`) each start their own
crier server on a scratch port and then poll it, so a stale or foreign process
squatting that port would make a run report success for a binary it never built.
They are guarded against exactly that: `federation-demo` and
`hermes-gateway-demo` refuse to start while anything already listens on their
ports — naming the holder's pid, its command line and the `ss -tlnp | grep :<port>`
audit command — and, once a server answers `/health`, they assert that the process
holding the port is the pid they started; `ws-mesh-demo` refuses the same way and
additionally requires an empty peer list at startup. A started process that dies
before answering aborts the run with the tail of its log instead of being papered
over. Which build is answering can be confirmed at any time — `GET /version`
returns the running server's identity (version, commit, build time, dirty). The
three guards live in `scripts/lib/port-guard.sh`; `make port-guard-selftest`
exercises all of them on a port the selftest picks as free itself, and CI runs
that selftest on every push.

The two llm-mesh lanes (`examples/llm-mesh/run-demo.sh` and its `mesh/` sibling)
add the second remedy for that class: their scratch port is CHOSEN from a
bounded candidate list instead of being hard-coded, so an unrelated listener
already holding the first candidate makes the run rotate on to the next one
instead of skipping it — every skipped candidate is reported with its holder's
pid, its command line and the audit command. An explicit `CRIER_PORT` is still
honored literally and fails closed when that port is occupied, and an exhausted
candidate list is a named failure that starts nothing (QA-CRIER-10).

## Message guard (LLM)

Every inbound delivery is classified by an LLM message guard before it reaches the receiver (CR-FEAT-010..014). The guard sits at ONE choke point in `POST /agents/{id}/inbox` — after the deliver request is decoded, before BOTH downstream branches (webhook POST and inbox store) — so webhook (blocking/async/batch) and inbox deliveries get identical treatment. The verdict is computed exactly once per message; redelivery and batch flush never re-run the guard.

- **Prompt-injection screening** — the guard LLM inspects the payload for four attack classes: instruction injection, jailbreak, masquerade (obfuscated/hidden instructions), and structured-object attacks (payloads that could be interpreted as control data). A deterministic pattern pre-scan runs first over the RAW payload; its matches enrich the LLM prompt and are merged into the final verdict. JSON payloads are shown to the LLM as a schema-aware text projection, non-JSON as a text envelope (both bounded by `CR_GUARD_RENDER_MAX_BYTES`).
- **Structured verdicts** — the LLM answers with exactly one JSON object: `{decision, risk_level, reason, matched_patterns}`, where `decision` is `allow` | `block` | `sanitize` and `risk_level` is `low` | `medium` | `high`. A deterministic escalation table guarantees the LLM can never under-block below the policy's `block_risk` threshold (default `high`).
- **allow** — delivery proceeds as-is; the verdict still rides on the envelope / inbox entry and the outbound POST headers.
- **block** — uniform `403 GUARD_BLOCKED` with the full verdict; the message is never queued, never stored, never POSTed.
- **sanitize** — the guard LLM REWRITES the payload with a fixed neutralization prompt and the **rewritten payload is delivered in place of the original** (validated before delivery: valid JSON, pattern-clean, size-capped). The original rides in `crier.guard.quarantined_payload` (base64) for provenance — the original is never delivered on a sanitize verdict. If the rewrite is unavailable: fail-closed policies block, fail-open policies deliver a deterministic quarantine notice.
- **Fail-open by default** — an LLM error (provider down, timeout, missing API key) resolves to `allow` with `errored: true`; per-policy `fail_closed: true` flips this to the policy's error action (default `block`). The deterministic pre-scan is the exception and is never disabled by an LLM outage: if it matched a **high-confidence** pattern (explicit injection/jailbreak text, control keys, structural escapes), the fail-open outcome escalates to `block`/`high` with `reason: "guard_error: …; deterministic prematch block: <names>"` — the same evidence that blocks an over-cap payload without any LLM call. Its matches (empty, or low-confidence shape hits only) otherwise ride along in `matched_patterns` instead of being discarded.
- **Outcome on the wire** — the outbound webhook POST carries `X-Crier-Guard-*` headers: `X-Crier-Guard-Decision`, `X-Crier-Guard-Risk`, `X-Crier-Guard-Reason` (percent-encoded), `X-Crier-Guard-Patterns` (comma-joined), `X-Crier-Guard-Policy`, `X-Crier-Guard-Provider`, `X-Crier-Guard-Model`, and `X-Crier-Guard-Error: true` on the error path. A header whose verdict value is empty is omitted rather than sent blank (`Patterns`/`Policy`/`Provider`/`Model`), and `-Error` appears only when the guard errored. Blocked messages never POST. Inbox entries and deliver responses carry the same verdict as `crier.guard` metadata — on a deliver response the `guard` object is omitted only for a **clean** allow (no error, risk `low`, no `matched_patterns`); an allow that carries any risk marker is surfaced so it can't be mistaken for a clean pass.
- **Per-agent policy** — guard policy is configured at agent registration: `"guard":{"policies":[{"id":"default"}]}` (at least one policy required; invalid config → 400). A bare id resolves to the built-in named policy; inline policies (`{model, base_url, api_key_ref, fail_closed, action, providers, ...}`) are accepted, and `channel_match` globs (`session:*`, `thread:*`) scope a policy to specific channels. Agents without a guard config use the server-wide default (`CR_GUARD_DEFAULT_POLICY`, built-in `default` when unset).
- **Providers** — each policy declares a failover chain (`providers`, implicit `[deepseek]` when omitted). Presets: `deepseek` (default, model `deepseek-v4-flash`, thinking disabled — the preset hard-rejects `thinking_enabled`), `groq` (default `openai/gpt-oss-120b`), `nvidia` (default `google/gemma-4-31b-it`), or `custom` (requires `base_url` + `api_key_ref`). Model ids are the providers' own qualified ids, vendor prefix included — both free-limit lanes reject the bare id with 404 `model_not_found`. API keys are referenced as `env:VAR` and never stored inline (deepseek preset → `DEEPSEEK_API_KEY`). The router takes the first healthy provider: one retry (250ms backoff) on 429/5xx/network errors, a per-endpoint circuit breaker (`CR_GUARD_CIRCUIT_*`), a concurrency cap (`CR_GUARD_MAX_CONCURRENT`), and one per-message time budget across the whole chain (`CR_GUARD_TIMEOUT_MS`, default 10s). A degraded lane is visible in the server log: every provider the router skips (no key, open circuit, unknown preset, permanent rejection) or that exhausts its attempts — including a provider whose attempt the per-message budget cut off (`reason: per-message budget exhausted`) and one the chain never reached because the budget was already spent (`reason: per-message budget exhausted before attempt`) — emits one `guard router: provider skipped` / `guard router: provider failed` line naming provider, model and reason — plus the provider's HTTP status and error code on a failure — and a chain that lands on a fallback adds one `guard router: failover landed on a later provider` line (`from` → `to`). Keys and payloads are never logged.
- **Kanban output (opt-in)** — a policy can enable fire-and-forget kanban cards (`"kanban":{"enabled":true,"on":"block"|"all","assignee":...,"board_url":...}`): each scoped verdict posts a card (`[crier-guard] <agent> <decision>: <reason>`, full verdict metadata, sender, truncated payload excerpt) through the `hermes kanban create` CLI or an HTTP sink (`CR_GUARD_KANBAN_URL`). Writes are bounded (queue `CR_GUARD_KANBAN_QUEUE`, default 100; 10s per card) and never fail the delivery — full queue drops + counts, write failures log + count.
- Payloads above `CR_GUARD_MAX_PAYLOAD_BYTES` (default 65536) skip the LLM entirely — the deterministic pre-scan is the only verdict source. Only **high-confidence** prematch hits (explicit injection text, control keys, structural escapes) block; a hit from a low-confidence shape pattern (currently `b64_blob`, an 80+ char alphanumeric/base64-like run that ordinary machine-generated bodies trip) is reported as evidence but not blocked: delivery is allowed with `risk_level: medium` and `reason: payload_exceeds_guard_cap: low-confidence prematch only`. No hit at all → allow, risk medium, `reason: payload_exceeds_guard_cap`, `patterns: ["oversize"]`.

Full spec: [`specs/LLM-MESSAGE-GUARD.md`](specs/LLM-MESSAGE-GUARD.md) (CR-SPEC-002).

## Agent Ecosystem

The [`examples/agent-ecosystem/`](examples/agent-ecosystem/) directory is a runnable
reference stack that demonstrates Crier as the message bus between **popular agent systems**:
Pi Agent, OpenCode, Claude Code, Codex, Aider, Goose, Hermes, plus a plain webhook echo sink
and a full battery of tests. Every agent self-registers with Crier at boot and answers
through the bus (blocking webhook round-trips, async fire-and-forget, the LLM message guard
with real DeepSeek verdicts when `DEEPSEEK_API_KEY` is set).

```bash
cd examples/agent-ecosystem

# 1. the long-lived stack only — crier + the agent consumers. The battery is
#    profile-gated, so `up` never runs it (DF-CRIER-88/90).
docker compose up -d --build

# 2. the battery is a ONE-SHOT, explicit command: it runs exactly once, builds its
#    own image if it is missing, and never rebuilds or recreates the stack — so
#    re-running it leaves crier's container (and every agent registration) intact.
docker compose --profile battery run --rm battery
```

The two steps are independent: step 1 starts the stack and never runs the battery,
step 2 is the only thing that runs it. After editing `battery/battery.sh`, rebuild
just that image with `docker compose --profile battery build battery` — never add
`--build` to the `run` command, which rebuilds the whole dependency graph and
recreates the crier container (`docs/AGENT-ECOSYSTEM.md` §3, §6.2).

Setup, per-harness walkthroughs, battery guide, bunker deployment, CI ops and
troubleshooting: [`docs/AGENT-ECOSYSTEM.md`](docs/AGENT-ECOSYSTEM.md) (CR-FEAT-022). The
normative design authority is [`specs/AGENT-ECOSYSTEM.md`](specs/AGENT-ECOSYSTEM.md)
(CR-SPEC-003).

## Detection & containment (CR-FEAT-030)

Crier could always tell you **who** sent what — after the fact. Per-agent
ed25519 identity, one delivery choke point, durable failure receipts: that is
attribution. This is the other half: seeing an intrusion **while it is
happening**, and ending it in one call. The normative design is
[`specs/DETECTION.md`](specs/DETECTION.md).

It is opt-in (`CR_DETECT_ENABLED`, default `false`). With the flag unset, none
of the routes below is registered, no file is written and the delivery path is
byte-identical to a build without this feature.

**1. An append-only, signed delivery log.** Every delivery outcome — who sent
what to whom, when, and the verdict the bus reached — is appended to
`CR_DETECT_LOG` and signed with the server's ed25519 key (`CR_DETECT_KEY`,
created `0600` on first boot, so the log stays verifiable across restarts).
Each record carries the hash of the one before it, so editing, deleting or
reordering a record breaks verification from that point on, and the server
**refuses to start** on a log it cannot verify rather than appending to a
history that no longer means anything. `GET /delivery-log` answers a bounded
page of the in-memory window plus the honest total; `GET /delivery-log/verify`
re-reads the file and reports the first record that does not verify.

**2. Behaviour baselines, with documented alerts.** Three signals, each one
alert per agent per window (a signal that fires once per message is a signal
nobody reads). `GET /alerts` returns them, with the thresholds that are in
force:

| Signal | Severity | Trips when | Default |
|--------|----------|------------|---------|
| `fanout_spike` | critical | one sender reaches N **distinct** targets inside the window | 5 targets in 60s (`CR_DETECT_FANOUT_MIN_TARGETS`, `CR_DETECT_FANOUT_WINDOW_S`) |
| `new_peer_burst` | critical | one sender opens N first-ever conversations inside the window | 3 in 60s (`CR_DETECT_NEWPEER_MIN_TARGETS`, `CR_DETECT_NEWPEER_WINDOW_S`) |
| `odd_hour_volume` | warning | one sender delivers N messages inside the quiet window | 3 in 01:00-05:00 UTC (`CR_DETECT_QUIET_MIN_MESSAGES`, `CR_DETECT_QUIET_HOURS`; `0-0` disables it) |
| `canary_trip` | critical | a delivery is addressed to a canary id, or carries a canary token in its payload | any single occurrence |

**3. The kill-switch — one call, four actions.** `POST /agents/{id}/kill-switch`
pauses the agent's outbound webhook lane (dropping what it had queued), releases
the leases it held so its unacked messages go back on the queue, quarantines it
(the delivery path then refuses both sends **from** it and deliveries **to** it
with `403 AGENT_QUARANTINED`) and removes its registry row. The response reports
every action separately, so a partial containment cannot read as a clean one.

**4. Canary tokens.** At boot the server plants canary tokens (`CR_CANARY_TOKENS`,
or two generated ones, logged and readable at `GET /canaries`) — strings an
operator puts where an exfiltrating agent would pick them up. Any delivery whose
payload carries one, or that is addressed to a canary's id, trips
`canary_trip`.

### Catching and containing a compromised agent

```bash
BASE=http://127.0.0.1:8767
# detection on, log into a file you keep
export CR_DETECT_ENABLED=true
export CR_DETECT_LOG=delivery.jsonl     # signed with delivery.jsonl.key
# ... the scenario, against a running server:
curl -s -X POST $BASE/agents -d '{"id":"peer-1"}'            # and peer-2 .. peer-5
curl -s -X POST $BASE/agents/peer-1/inbox \
     -d '{"sender":"compromised","payload":{"x":1}}'          # and the other peers
curl -s $BASE/alerts                                          # fanout_spike + new_peer_burst, with evidence
curl -s -X POST $BASE/agents/compromised/kill-switch \
     -d '{"reason":"fan-out to 5 new peers"}'                 # ONE call: 4 actions, each reported
curl -s $BASE/delivery-log/verify                             # the whole file still verifies
curl -s -X POST $BASE/agents/compromised/inbox \
     -d '{"sender":"ops","payload":{"x":1}}'                  # 403 AGENT_QUARANTINED (target side)
```

That run is executed as a test against the real server —
`TestDetectionCatchesAndContainsACompromisedAgent` in
`cmd/server/crfeat030_test.go` — which delivers to five new peers, captures the
alert it trips, contains the agent in one call and then proves the enforcement,
the log and the restart survival. The capture is in the test's own output.

## Priority lanes & real backpressure (CR-FEAT-035)

The external review (`DISPATCH · CRI-001`, Carter, via Bane, 2026-09-25) named
three gaps in passing: **no priority lanes**, the per-agent 100/min publish cap
as **the only backpressure**, and **no visibility into how deep a queue was** —
so a steady stream of low-value messages could starve an urgent one, and a
runaway producer either got 429ed per agent id or pushed every inbox deeper with
nothing to stop it and nothing to read. All three are addressed below, and an
existing deployment is unaffected: **a delivery that names no priority is the
FIFO message it always was, and an unconfigured server sheds nothing at all.**

### Priority — an optional `priority` on a delivery

`POST /agents/{id}/inbox` accepts an optional `priority`, integer **0..9**
(default `0`). `GET /agents/{id}/inbox` hands back the **highest** priority
claimable messages first; messages of equal priority keep arrival order, and the
default queue is plain FIFO — so a client that never sends the field sees exactly
the ordering it saw before the field existed (`"priority":0` is not even on the
wire: the key is omitted at the default).

```bash
BASE=http://127.0.0.1:8767
# A low-value backlog, then one urgent message BEHIND it.
curl -s -X POST $BASE/agents/agent-1/inbox -d '{"payload":{"batch":1}}'                 # 201 (priority 0)
curl -s -X POST $BASE/agents/agent-1/inbox -d '{"payload":{"batch":2}}'                 # 201 (priority 0)
curl -s -X POST $BASE/agents/agent-1/inbox -d '{"payload":{"alert":"disk"},"priority":9}'  # 201
# The urgent message comes back FIRST, wherever it arrived.
curl -s "$BASE/agents/agent-1/inbox?max=1"      # -> the {"alert":"disk"} message, "priority":9
# Out of range is refused, never clamped, and stores nothing:
curl -s -X POST $BASE/agents/agent-1/inbox -d '{"payload":{},"priority":10}'
# 400 {"error":"priority must be 0..9"}
```

- **It is a preference within ONE inbox, not a priority queue across the bus.**
  A higher-priority message is chosen among the messages *claimable at that
  moment* (unacked, unexpired, unleased), and each retrieve returns at most
  `limit` of them — it cannot pre-empt a lease already handed out, and it never
  reorders across agents.
- **The ordering survives a restart on the durable backend**: the priority is
  stored on the message (PostgreSQL migration 007 adds
  `inbox_entries.priority`, `NOT NULL DEFAULT 0`, range-checked), so every
  message a pre-existing database already holds reads back at priority 0 — the
  order it has always had.
- **The MCP bridge does not expose it yet**: `deliver_message` sends no
  priority, so bridge-delivered messages are the FIFO default. Named here rather
  than left to be discovered.

### Backpressure — a global ingest budget, and a `Retry-After` on every 429

`CR_RATE_LIMIT_GLOBAL_PER_MINUTE` (default **0 = no budget**) is a shed on the
delivery path itself, across every agent and every sender. Set it and a delivery
over budget is refused before any transport, guard call or store write:

```bash
export CR_RATE_LIMIT_GLOBAL_PER_MINUTE=3
curl -si -X POST $BASE/agents/agent-1/inbox -d '{"payload":{"n":1}}'   # 201, 201, 201 …
# …the fourth delivery inside the minute:
# HTTP/1.1 429 Too Many Requests
# Retry-After: 60
# {"error":"RATE_LIMITED_GLOBAL","scope":"global","limit_per_minute":3,"retry_after_s":60}
```

- The error is **named** (`RATE_LIMITED_GLOBAL`) so a client branches on a code,
  not on prose; `Retry-After` (whole seconds, never below 1) and the body's
  `retry_after_s` carry the same wait, so a client that logs only one of them
  still learns it.
- The wait is honest: it is the time until enough of the current window has aged
  out that a retry is admitted, if no one else has spent the budget meanwhile.
- **Both 429s carry `Retry-After` now** — the pre-existing per-agent publish cap
  (`CR_RATE_LIMIT_PER_MINUTE`, `POST /relay/publish`) gained the header too,
  with its body and its per-agent scope unchanged.
- The budget is checked **after** the request is decoded, validated and the
  containment decision made, and **before** idempotency bookkeeping, federation
  forwarding, the guard call and the store write — so a malformed delivery still
  answers 400 and a contained agent still answers 403 `AGENT_QUARANTINED`; a 429
  never masks either.
- **Per-namespace budgets are not here yet** — that is CR-FEAT-029's half, and
  today every delivery counts against the one global key. The counter is already
  labelled by scope, so that change adds series rather than renaming them.

### Depth — where operators already look

`GET /status` carries the live store-wide queue beside the budget that protects
it, and `GET /metrics` exposes the same three numbers as gauges:

```json
{
  "global_rate_limit_per_minute": 3,
  "queue_depth": { "pending": 41, "leased": 3, "oldest_age_s": 12 }
}
```

```
# TYPE inbox_queue_depth gauge
inbox_queue_depth 41
# TYPE inbox_queue_leased gauge
inbox_queue_leased 3
# TYPE inbox_queue_oldest_age_seconds gauge
inbox_queue_oldest_age_seconds 12
# TYPE inbox_shed_total counter
inbox_shed_total{scope="global"} 7
```

- `pending` counts un-acknowledged, un-expired messages across **all** inboxes
  (a leased message is still queued); it is the sum of the per-agent counters
  `GET /agents/{id}/inbox/stats` already reports.
- A store that cannot report a depth renders `"queue_depth": null` in `/status`
  and `NaN` on the gauges — "empty" and "unavailable" are never the same
  reading.
- `inbox_shed_total` counts what the budget refused, by scope.

All of the above is executed as a test against the real server —
`TestCRFEAT035_*` in `cmd/server/crfeat035_test.go` (priority beats arrival
order, absent priority is FIFO, the budget sheds with 429 + `Retry-After` + the
named error, the depth is readable in `/status` and `/metrics`, and an
unconfigured server refuses nothing).

## Configuration

All configuration is via environment variables (defaults shown):

| Variable | Default | Description |
|----------|---------|-------------|
| `CRIER_PORT` | `8767` | Server listen port |
| `CR_PIDFILE` | _(unset — no pidfile)_ | Pidfile path. When set (or `-pidfile <path>` is passed, or `make run`'s default `.crier.pid` is used), the server writes `{pid, port, binary}` to this file **after** the port is bound and removes it on graceful shutdown; `-stop`/`make stop` read it to stop that exact process safely (ownership-checked against `/proc/<pid>/exe`; a mismatch is refused without signalling). See [Stop / restart](#stop--restart). |
| `CR_DATABASE_URL` | _(unset — the demo-only in-memory backend; [Run](#run) sets it)_ | PostgreSQL connection — **the backend the documented path uses**. When set, the registry and inboxes use the durable PostgreSQL backend (migrations applied automatically on start). Unset, the in-memory backend serves instead: process-lifetime only, so agents and undelivered messages are lost on restart — demo-only, never the configuration to hand a tester. Precedence: `CR_DATABASE_URL` → `DATABASE_URL` → `CRIER_DATABASE_URL`. Example: `postgres://crier:crier@localhost:5437/crier?sslmode=disable`. See [Durable backend (PostgreSQL)](#durable-backend-postgresql) for the runnable compose path. |
| `CR_AUTH_TOKEN` | _(unset — auth disabled)_ | Bearer token for API authentication. When set, all requests **except the five exempt paths** (`/health`, `/version`, `/openapi.json`, `/openapi.yaml`, `/docs` — see `internal/middleware/auth.go`) require `Authorization: Bearer <token>`; unset = no auth (local dev). |
| `CR_REQUIRE_AGENT_SIG` | `true` | Enforce per-agent ed25519 request signing on agent-scoped endpoints (inbox retrieve/ack/stats, DELETE /agents/{id}, and PATCH /agents/{id}). Set `false` only for trusted single-user dev setups. |
| `CR_REQUIRE_MESH_AUTH` | `false` | Require the ed25519 challenge/response handshake on `GET /mesh/connect/{agentID}` (DF-CRIER-287): a connecting peer must sign a single-use nonce with the private key whose public half the registry holds for that agent id, and is not admitted (and not listed by `GET /mesh/peers`) until the signature verifies. **Default off, deliberately** — existing single-host clients do no handshake, and turning it on requires every connecting agent to hold its key. With it on, a `REQUEST` whose `source.agent_id` is not the authenticated peer is refused `FORBIDDEN` and a reply may only come from the peer the request was addressed to. See [Authentication (opt-in)](#authentication-opt-in) for the migration note. |
| `CR_MESH_AUTH_TIMEOUT_S` | `10` | How long a mesh connect challenge stays valid — the client must answer with `AUTH_RESPONSE` inside this window, after which the server answers `AUTH_FAILED` and closes the socket. Only read when `CR_REQUIRE_MESH_AUTH=true`. |
| `CR_MESH_ALLOWED_ORIGINS` | _(unset — all origins allowed)_ | The **mesh's own** WebSocket `Origin` allowlist (`scheme://host:port`, comma-separated; `*` allows all), separate from `CR_WS_ALLOWED_ORIGINS` — which keeps covering the relay. When set, a mesh upgrade that *carries* an `Origin` header must name a listed origin (otherwise `403` before any frame), while a client that sends no `Origin` at all — every agent client, including this repo's — still connects. `GET /status` reports the mode as `mesh_origin_policy`. |
| `CR_PRESENCE_STALE_AFTER_S` | `90` | How long a registry row may go without **liveness evidence** — a mesh connect accepted for it, a `KEEPALIVE` heartbeat on that socket, or a signed `PATCH /agents/{id}` — before the `status` reported for it becomes `stale` (CR-FEAT-024; see [§3](#3-agent-registry)). The default is three missed mesh heartbeats (3 × 30s), so one dropped tick can never flip a live agent. It is a READ-TIME window: nothing is stored, no sweeper runs, and the stored `status` column keeps its `online`/`offline` values. Setting it below the 30s keepalive interval makes a healthy agent flap to `stale` between beats — the server warns at startup, and `GET /status` reports the effective window as `presence_stale_after_s`. A non-positive or non-integer value is a startup error, never a silently ignored setting. |
| `CR_LOG_LEVEL` | `info` | Log level. One of `debug`, `info`, `warn`, `error`. |
| `CR_LOG_FORMAT` | `text` | Log format. One of `text`, `json`. |
| `CR_RATE_LIMIT_PER_MINUTE` | `100` | Per-agent publish rate limit (events/minute), keyed on the `X-Agent-ID` header. `0` disables rate limiting and the identity requirement. The 429 carries a `Retry-After` header in whole seconds (CR-FEAT-035) and the budget is PER AGENT — for the delivery path's global budget see `CR_RATE_LIMIT_GLOBAL_PER_MINUTE` below. |
| `CR_RATE_LIMIT_GLOBAL_PER_MINUTE` | `0` (disabled) | Global inbox-ingest budget, in deliveries/minute across **every** agent and sender (CR-FEAT-035). `0` — the default — means no global budget exists: no delivery is ever refused by it and the delivery path is byte-identical to a build without the feature. When set, a delivery over budget is refused `429 {"error":"RATE_LIMITED_GLOBAL","scope":"global","limit_per_minute":N,"retry_after_s":N}` with a matching `Retry-After` header, before any transport, guard call or store write — so a runaway producer cannot push every inbox deeper without limit, which the per-agent publish cap alone could not do (a flood from many ids spends every agent's own budget). Checked after decode/validation/containment and before idempotency, federation, the guard and the store: a malformed delivery still answers 400 and a contained agent still answers 403 `AGENT_QUARANTINED`. Per-namespace budgets are CR-FEAT-029's half. |
| `CR_WS_ALLOWED_ORIGINS` | _(unset — all origins allowed)_ | Comma-separated list of allowed WebSocket `Origin` headers (`scheme://host:port`). `*` allows all origins. |
| `CR_FED_LINKS` | _(unset — federation disabled)_ | Comma-separated base URLs of linked relays (relay-to-relay federation, CR-FEAT-006). When set, deliveries to agents unknown on this relay are forwarded to each linked relay in order (first non-404, non-retryable answer wins and is relayed back verbatim — a blocking webhook reply returns to the original sender), and `GET /fed/peers` lists the linked relays with their agents. A transient link outage (unreachable link, or a retryable 5xx/408/429) is held and retried instead of answering `404` (`202 Accepted {"status":"held",…}`) — but only when the request names a `sender`; a sender-less request is never held and answers `502 {"error":"FEDERATION_FAILED",…}` immediately, because the terminal notification is addressed to the sender and could never reach an inbox (DF-CRIER-129). A definitive all-links-404 still answers `404` immediately. Example: `http://localhost:18772`. |
| `CR_FED_NAME` | _(unset — `localhost:<port>`)_ | Optional display name for this relay in the `GET /fed/peers` listing. |
| `CR_FED_TOKEN` | _(unset — no link auth)_ | Shared secret for federation link authentication. When set, every request this relay sends to a linked relay (deliver forwards and `/agents` discovery) carries `Authorization: Bearer <token>`, so an auth-enabled destination relay accepts the forward instead of returning 401. The linked relay must share the value: set the source relay's `CR_FED_TOKEN` equal to the destination relay's `CR_AUTH_TOKEN`. The secret is never logged, echoed, or included in `GET /fed/peers` output. |
| `CR_FED_MAX_HOLD_S` | `300` | How long a federated delivery is held at this relay and retried when every link fails transiently, before the sender gets an explicit terminal outcome (DF-CRIER-7, `specs/WEBHOOK-DELIVERY.md` §8.1). Only the transient case is held, and only when the request names a `sender` — an unreportable delivery is answered synchronously with `502 FEDERATION_FAILED` instead of being held (DF-CRIER-129). A hold **recovered** by a later successful retry logs the linked relay's reply but routes it nowhere — the sender's request already answered `202` — so a sender that needs that reply polls the peer's inbox or supplies its own correlation (a `request_id` echoed by the peer's webhook reply path; DF-CRIER-282, `specs/WEBHOOK-DELIVERY.md` §8.1). |
| `CR_FED_QUEUE_FILE` | _(unset — memory queue)_ | Path of the durable hold-queue document. When set, held deliveries survive a source-relay restart (one atomically rewritten JSON document, `0600`, recovered from `<path>.bak` after a crash). Unset keeps the queue process-lifetime only — held deliveries are lost on restart, the same demo-only, process-lifetime contract the in-memory registry backend documents for inboxes. A delivery this queue recovers (a retry that later succeeds) logs the linked relay's reply but drops it — the sender already got its `202` — so a sender that needs that reply polls the peer's inbox or supplies its own correlation (DF-CRIER-282, `specs/WEBHOOK-DELIVERY.md` §8.1). |
| `CR_DATABASE_MAX_CONNS` | `4` | Maximum PostgreSQL pool connections. |
| `CR_DATABASE_MIN_CONNS` | `0` | Minimum PostgreSQL pool connections kept open (must be ≤ `CR_DATABASE_MAX_CONNS`). |
| `CR_DATABASE_MAX_CONN_LIFETIME` | `30m` | Maximum lifetime of a pooled connection (Go duration, e.g. `30m`, `1h`). |
| `CR_DATABASE_MAX_CONN_IDLE_TIME` | `5m` | Maximum idle time of a pooled connection (Go duration). |
| `CR_DATABASE_CONNECT_TIMEOUT` | `10s` | PostgreSQL connect timeout (Go duration). |
| `CR_GUARD_ENABLED` | `true` | LLM message-guard master switch. When on, every inbound delivery is classified before webhook POST / inbox store. |
| `CR_GUARD_TIMEOUT_MS` | `10000` | Per-message guard budget in milliseconds — covers the whole provider chain, retries included. |
| `CR_GUARD_MAX_CONCURRENT` | `8` | Maximum concurrent guard LLM calls. |
| `CR_GUARD_CIRCUIT_THRESHOLD` | `10` | Consecutive failures (per base URL + model) that open the provider circuit breaker. |
| `CR_GUARD_CIRCUIT_COOLDOWN_S` | `300` | How long a tripped circuit stays open (seconds); the first call after expiry is the probe. |
| `CR_GUARD_MAX_PAYLOAD_BYTES` | `65536` | Payloads larger than this skip the LLM entirely (risk medium). Only high-confidence prematch hits block; low-confidence shape hits (`b64_blob`) allow with `reason: payload_exceeds_guard_cap: low-confidence prematch only`. |
| `CR_GUARD_RENDER_MAX_BYTES` | `32768` | Byte cap on the payload projection fed to the LLM. |
| `CR_GUARD_DEEPSEEK_BASE_URL` | `https://api.deepseek.com/v1` | Base URL override for the deepseek provider preset. |
| `CR_GUARD_MODEL` | `deepseek-v4-flash` | Default model override for the deepseek provider preset. |
| `CR_GUARD_PATTERNS_EXTRA` | _(unset)_ | JSON array of extra prematch patterns (`[{"name","pattern","class"}]`) — appended, or replacing built-ins with the same name. Invalid JSON/regex fails fast at startup. |
| `CR_GUARD_DEFAULT_POLICY` | _(unset — built-in `default`)_ | JSON `Policy` used as the server-wide default when the target agent registers no guard config. Must parse + validate at startup (fail-fast). |
| `CR_GUARD_KANBAN_QUEUE` | `100` | Kanban worker queue capacity (fire-and-forget cards, opt-in per policy `kanban`). |
| `CR_GUARD_KANBAN_URL` | _(unset — Hermes kanban CLI)_ | HTTP kanban sink base URL (http/https, CR-FEAT-009). When set, guard cards are POSTed here as JSON (fire-and-forget); unset = cards go through the `hermes kanban create` CLI writer. |
| `CR_WEBHOOK_SECRET` | _(unset)_ | HMAC outbound signing. |
| `CR_WEBHOOK_TIMEOUT_S` | `30` | Outbound webhook timeout, seconds. |
| `CR_WEBHOOK_MAX_RETRIES` | `5` | Outbound retry count for queued async/batch webhook deliveries. When a delivery exhausts them, the sender gets exactly one durable `WEBHOOK_FAILED` in its own inbox — see [Webhook delivery](#5-webhook-delivery-bypasses-the-inbox). |
| `CR_IDEMPOTENCY_WINDOW_S` | `86400` | How long a sender-supplied `idempotency_key` deduplicates a delivery (CR-FEAT-025). Within the window a repeated key for the same agent is answered with the original accept (`"idempotent_replay":true`, same message id) and stores nothing, so a sender that retried after losing the response does not duplicate work on the target; past it the key delivers normally. It is a per-relay, in-memory retry window, not a durable ledger — a restart closes it early, and the message the retry would have duplicated is still in the target's inbox. A non-positive or non-integer value is a startup error: a window of `0` would mean "deduplicate nothing" while the deliver schema still documents the feature. |
| `CR_WEBHOOK_REDELIVER_S` | `30` | Redelivery interval, seconds — one queued delivery attempt per tick. |
| `CR_WEBHOOK_PROBE_S` | `60` | Dead-target probe interval, seconds. |
| `CR_WEBHOOK_CIRCUIT_THRESHOLD` | `10` | Consecutive failures that open the circuit. |
| `CR_WEBHOOK_BATCH_MAX` | `10` | Batch flush size. |
| `CR_WEBHOOK_BATCH_FLUSH_S` | `5` | Batch flush interval, seconds. |
| `DEEPSEEK_API_KEY` | _(unset)_ | API key for the deepseek provider preset (referenced as `env:DEEPSEEK_API_KEY`). Without it, guard LLM calls fail and the guard fails open. |
| `CR_ENABLE_PPROF` | `false` | Opt-in: register `GET /debug/pprof/` (plus `cmdline`, `profile`, `symbol`, `trace`, `heap`, `goroutine`, `block`, `mutex`, `threadcreate`) for live Go profiling. Default off — unset means the path is not registered and answers `404`. Not auth-exempt: with `CR_AUTH_TOKEN` set it requires the Bearer header like any other authenticated route. See [Observability](#observability-metrics--profiling). |
| `CR_ENABLE_METRICS` | `false` | Opt-in: register `GET /metrics` serving the Prometheus text exposition format (v0.0.4) — deliveries, webhook outcomes, guard decisions, federation hold depth, relay events, WS subscribers, HTTP requests, and the live inbox queue (`inbox_queue_depth`, `inbox_queue_leased`, `inbox_queue_oldest_age_seconds`, `inbox_shed_total`; CR-FEAT-035). Default off — unset means the path is not registered and answers `404`. Not auth-exempt: with `CR_AUTH_TOKEN` set it requires the Bearer header like any other authenticated route. See [Observability](#observability-metrics--profiling). |
| `CR_A2A_ENABLED` | `false` | Opt-in: A2A (agent-to-agent protocol) interoperability — INT-A2A-001/002, [`specs/A2A-OPTION.md`](specs/A2A-OPTION.md). **Default off, and A2A is an extra rather than first-class support**: with the flag unset nothing A2A-related is registered, and every existing route, response body, auth requirement and storage path behaves exactly as it did before the option existed. The flag is also only HALF the gate — an agent takes part in A2A only if it opted in as well, via the optional `a2a` object on `POST /agents` / `PATCH /agents/{id}` (`{"a2a":{"enabled":true}}`, strictly decoded, absent by default). With the flag set crier publishes exactly ONE A2A surface: `GET /.well-known/agent-card.json?agent_id=<id>` serves the [A2A](https://a2a-protocol.org) Agent Card projected from that agent's registry row (INT-A2A-002) — `404` for an id that is not an opted-in row, `400` when the request names no agent, `Cache-Control: private` + `ETag` for conditional GETs — and nothing else: the JSON-RPC binding, streaming and the push-notification configs land with INT-A2A-003..006. |
| `CR_DETECT_ENABLED` | `false` | Opt-in: the detection layer (CR-FEAT-030) — signed delivery log, behaviour alerts, canary tokens and the kill-switch, plus the five detection routes. Default off: unset means no route is registered (all five answer `404`), no log file is written and the delivery path is unchanged. Not auth-exempt — the kill-switch requires the Bearer header like every other authenticated route. See [Detection & containment](#detection--containment-cr-feat-030). |
| `CR_DETECT_LOG` | _(unset — no log written)_ | Path of the append-only signed delivery log. Every delivery outcome is appended and fsynced; alerts and containment are recorded in it too. Unset, alerts and containment still work in memory — nothing is persisted. |
| `CR_DETECT_KEY` | _(unset — `<CR_DETECT_LOG>.key`)_ | ed25519 signing-key file for the log, created `0600` on first boot and reused after that, so a restarted server still verifies what the previous one wrote. A key file that is unreadable or the wrong length is a startup error, never a silent regeneration. |
| `CR_DETECT_FANOUT_WINDOW_S` | `60` | Fan-out signal window, seconds. |
| `CR_DETECT_FANOUT_MIN_TARGETS` | `5` | Distinct targets one sender must reach inside the fan-out window to trip `fanout_spike`. |
| `CR_DETECT_NEWPEER_WINDOW_S` | `60` | New-peer signal window, seconds. |
| `CR_DETECT_NEWPEER_MIN_TARGETS` | `3` | First-ever conversations one sender must open inside the window to trip `new_peer_burst`. |
| `CR_DETECT_QUIET_HOURS` | `1-5` | UTC quiet window `S-E` for the odd-hour signal — start inclusive, end exclusive, wrapping past midnight (`22-6` works). `0-0` disables that signal. |
| `CR_DETECT_QUIET_MIN_MESSAGES` | `3` | Messages one sender may deliver inside the quiet window before `odd_hour_volume` fires. |
| `CR_CANARY_TOKENS` | _(unset — two generated per boot, and logged)_ | Comma-separated canary tokens to plant. A delivery addressed to a canary id, or carrying a canary token in its payload, trips `canary_trip`. Pinned tokens keep stable ids across restarts; generated ones rotate per boot. |

### Durable backend (PostgreSQL)

This is the backend [Run](#run) documents: its two commands are the
`docker compose up -d postgres` below plus the server started with
`CR_DATABASE_URL` pointing at it. Nothing here is optional polish — it is the
configuration in which crier's promise (an inbox that keeps a delivered message
until it is read or expires) actually holds across a restart. Everything after
the first block is tuning for hosts where the default host port or the compose
project name is already taken.

The `postgres` service in `docker-compose.yml` (image `postgres:16-alpine`, credentials `crier`/`crier`, database `crier`) publishes the container's in-container port 5432 on host port **5437** by default:

```bash
docker compose up -d postgres
CR_DATABASE_URL='postgres://crier:crier@localhost:5437/crier?sslmode=disable' ./bin/crier
```

Migrations apply automatically on startup; agents and undelivered messages then survive restarts. Both the host port and the project (and therefore the container name) are env-overridable — `CRIER_PG_HOST_PORT` picks the host port, `COMPOSE_PROJECT_NAME` scopes the container away from a name collision on a shared host.

When host 5437 is already taken (check with `ss -tlnp | grep :5437`), override it and use the same port in the URL:

```bash
CRIER_PG_HOST_PORT=5493 docker compose up -d postgres
CR_DATABASE_URL='postgres://crier:crier@localhost:5493/crier?sslmode=disable' ./bin/crier
```

When a stale container from an earlier project squats the expected name, rescope the compose project so its container is named after it instead:

```bash
COMPOSE_PROJECT_NAME=crier-lab docker compose up -d postgres
# container <project>-postgres-1; the URL still uses localhost:5437 (or your CRIER_PG_HOST_PORT override)
```

Stop and remove with `docker compose down`; add `-v` to drop the `pgdata` volume as well.

## API

The full API is documented in [`docs/openapi.yaml`](docs/openapi.yaml) — an OpenAPI 3.1 spec with **16 paths** and **20 operations** (a path carries one entry per HTTP method, so the two counts differ) across 8 operation groups. Every count in this README names its unit; measure them yourself:

```bash
grep -c '^  /' docs/openapi.yaml                                    # 16 paths
grep -cE '^    (get|post|put|patch|delete):' docs/openapi.yaml      # 20 operations
grep -oE 'HandleFunc\("[^"]+"' cmd/server/main.go | sort -u | wc -l # 24 router paths
```

The router registers **24 paths**: those 16 plus the three spec-hosting routes (`/openapi.json`, `/openapi.yaml`, `/docs`) that are not part of the API document, plus the five optional detection routes (CR-FEAT-030) that exist only when `CR_DETECT_ENABLED` is on — see [Detection & containment](#detection--containment-cr-feat-030).

| Group | Endpoints | Description |
|-------|-----------|-------------|
| **Health** | `GET /health` | Service health check |
| **Version** | `GET /version` | Build identity of the running server (version, commit, build time, dirty) — public like `/health` |
| **Status** | `GET /status` | Effective runtime posture (auth, signature requirement, guard switch, registry backend, build identity) — authenticated, not auth-exempt |
| **Relay** | `POST /relay/publish`, `GET /relay/subscribe/{topic}`, `GET /relay/topics` | Pub/sub |
| **Mesh** | `GET /mesh/connect/{agentID}`, `GET /mesh/peers` | P2P connections |
| **Federation** | `GET /fed/peers` | Relay-to-relay federation peer listing (CR-FEAT-006) |
| **Registry** | `POST /agents`, `GET /agents` (capability filter), `GET /agents/{id}`, `PATCH /agents/{id}`, `DELETE /agents/{id}` | Agent identity + self-configuration |
| **Inbox** | `POST /agents/{id}/inbox`, `GET /agents/{id}/inbox`, `POST /agents/{id}/inbox/ack`, `GET /agents/{id}/inbox/stats` | Message delivery |
| **Detection** (opt-in) | `GET /delivery-log`, `GET /delivery-log/verify`, `GET /alerts`, `GET /canaries`, `POST /agents/{id}/kill-switch` | Signed delivery log, behaviour alerts, canary tokens and the single-call kill-switch (CR-FEAT-030, `CR_DETECT_ENABLED`) |
| **Ownership** | `POST /agents/{id}/inbox/transfer`, `GET /agents/{id}/inbox/dead-letters` | Rebalance a stuck lease; read the messages that expired unacknowledged (CR-FEAT-025) |

### Runtime posture — `GET /status`

`GET /status` answers the question `/health` ("is it up?") and `/version` ("which build?") leave open: **what is actually in force on this server?** A process that answers `/health` `ok` while running with auth disabled, per-agent signatures optional and the message guard off is indistinguishable from a locked-down one until a request is rejected — this endpoint makes that posture readable:

```bash
curl -s -H "Authorization: Bearer $CR_AUTH_TOKEN" localhost:8767/status | python3 -m json.tool
```

```json
{
  "auth_enabled": true,
  "require_agent_signature": true,
  "guard_enabled": true,
  "registry_backend": "memory",
  "rate_limit_per_minute": 100,
  "global_rate_limit_per_minute": 0,
  "queue_depth": {
    "pending": 0,
    "leased": 0,
    "oldest_age_s": 0
  },
  "log_level": "info",
  "log_format": "text",
  "webhook_signing": false,
  "federation_enabled": false,
  "federation_hold_queue": "none",
  "metrics_enabled": false,
  "pprof_enabled": false,
  "build": {
    "version": "1.2.3",
    "commit": "1a2b3c4d",
    "build_time": "2026-09-14T06:05:59Z",
    "modified": false
  }
}
```

- `auth_enabled`, `require_agent_signature` and `guard_enabled` are the effective switches at the delivery choke point — the same values the startup log lines report, readable at any time instead of only at boot.
- `registry_backend` is the backend **actually serving**: `postgres` when `CR_DATABASE_URL` (or its fallbacks) is set, `memory` otherwise. `run()` selects the store and derives this field from the same value, so it cannot advertise a backend other than the one answering.
- `webhook_signing` and `federation_enabled` report whether the corresponding secret/link configuration is in effect — as booleans, never as values.
- `federation_hold_queue` is the **durability mode** of the federation hold path: `none` (no `CR_FED_LINKS`, so no hold path exists), `memory` (held deliveries are process-lifetime and lost on restart) or `file` (`CR_FED_QUEUE_FILE` is set, so they survive a restart). The path itself is config, not posture, and is never in the body.
- `metrics_enabled` / `pprof_enabled` say whether the opt-in inspection surfaces are registered at all (`false` = those paths answer `404`).
- `global_rate_limit_per_minute` is the effective **global inbox-ingest budget** (CR-FEAT-035, `CR_RATE_LIMIT_GLOBAL_PER_MINUTE`); `0` — the default — means no global budget exists and no delivery is ever shed by one. It sits beside `rate_limit_per_minute` (the per-agent **publish** cap) because they are different lanes, and an operator deploying one must not read the other as it.
- `queue_depth` is the one field here that is a **measurement rather than posture** (CR-FEAT-035): the live store-wide inbox queue — `pending` unacknowledged messages, how many of them are `leased`, and the age of the oldest. It changes between two reads of the same server, it carries counts and an age and nothing else (no payload, no message id, no agent id), and it is `null` — not `0` — when the serving store cannot report a depth, so "empty" and "unavailable" are never the same reading. `GET /metrics` reports the same three numbers as the `inbox_queue_*` gauges.
- `build` is byte-for-byte the object `GET /version` serves — the same `internal/buildinfo` source, nested rather than flattened.
- **No secret or connection value is ever serialized**: not `CR_AUTH_TOKEN`, not `CR_DATABASE_URL` (only the backend name), not `CR_WEBHOOK_SECRET`, not `CR_FED_TOKEN`, and no guard provider credential or base URL.
- `GET /status` is **not** auth-exempt: with `CR_AUTH_TOKEN` set it requires the Bearer header and answers `401` without it. The exempt-path list in `internal/middleware/auth.go` is unchanged at five paths — `/health` and `/version` stay public because "up?" and "which build?" must be answerable without a token; the enforced-posture answer is not.

## Observability (metrics & profiling)

Two opt-in live-inspection surfaces (`DF-CRIER-142`); both are **off by default** (set the env var to enable, unset = the path answers `404`):

- `GET /metrics` (`CR_ENABLE_METRICS=true`) — the Prometheus text exposition format (v0.0.4): `deliveries_total`, `webhook_deliveries_total{outcome}`, `guard_decisions_total{decision}`, `federation_held_current`, `relay_events_total`, `ws_subscribers` (relay topic subscribers + connected mesh peers, summed), `expired_messages_total`, `dead_lettered_messages_total`, `expiry_receipts_total`, `idempotent_replays_total`, `transfers_total`, `http_requests_total{code}`, and the live inbox queue (CR-FEAT-035): `inbox_queue_depth`, `inbox_queue_leased`, `inbox_queue_oldest_age_seconds` (gauges, `NaN` when the serving store cannot report a depth) and `inbox_shed_total{scope}` (what the global ingest budget refused).
- `GET /debug/pprof/` (`CR_ENABLE_PPROF=true`) — the standard Go profiling index plus the named profiles (`heap`, `goroutine`, `block`, `mutex`, `threadcreate`, `profile`, `symbol`, `trace`, `cmdline`).

**Neither path is auth-exempt**: they are served like any other authenticated route — with `CR_AUTH_TOKEN` set they require `Authorization: Bearer <token>`; with auth disabled they are open. The exempt-path list in `internal/middleware/auth.go` is unchanged. Exposure note: the pprof surface reveals runtime internals (stacks, heap) — enable it only on trusted networks.

## Documentation

| File | Description |
|------|-------------|
| [`docs/architecture.md`](docs/architecture.md) | Architecture overview and design decisions |
| [`docs/specs.md`](docs/specs.md) | Component specifications and acceptance criteria |
| [`docs/mesh-protocol.md`](docs/mesh-protocol.md) | Mesh wire protocol — framing, message types, correlation contract, worked example |
| [`docs/openapi.yaml`](docs/openapi.yaml) | OpenAPI 3.1 API specification |
| [`docs/integration-guide.md`](docs/integration-guide.md) | End-to-end integration guide — auth modes, signing, inbox lifecycle, mesh, Postgres |
| [`docs/AGENT-ECOSYSTEM.md`](docs/AGENT-ECOSYSTEM.md) | Agent-ecosystem reference stack — setup, per-harness walkthroughs, battery guide, bunker deployment, CI ops, troubleshooting (CR-FEAT-022) |
| [`specs/LLM-MESSAGE-GUARD.md`](specs/LLM-MESSAGE-GUARD.md) | Message guard spec (CR-SPEC-002) — verdict contract, policies, providers, kanban output |
| [`specs/A2A-OPTION.md`](specs/A2A-OPTION.md) | A2A interoperability as an opt-in extra (INT-A2A-001) — binding decision (JSON-RPC 2.0 + SSE), A2A ⇄ crier object mapping, the two-half gate, the route surface, the non-regression contract |
| [`examples/demo.sh`](examples/demo.sh) | Runnable end-to-end demo (register → deliver → signed retrieve → ack) |

## Project Status

All core primitives are implemented and tested:

- **Relay** — Thread-safe in-memory pub/sub, 87.5% coverage, 7/7 GitReins PASS
- **Mesh** — P2P WebSocket connections ported from Hivemind, 8/8 GitReins PASS
- **Registry + Inboxes** — Net-new, 78.3% coverage, 8/8 GitReins PASS
- **Persistence** — PostgreSQL backend for registry + inboxes via `CR_DATABASE_URL`; verified live that agents (webhook + guard config included), and undelivered messages survive a server restart
- **Message guard** — LLM prompt-injection guard at the delivery choke point (CR-FEAT-010..014): structured verdicts, fail-open with per-policy fail-closed, X-Crier-Guard-* headers, provider failover, opt-in kanban cards
- **Detection & containment** — an opt-in detection layer (CR-FEAT-030, `CR_DETECT_ENABLED`): an append-only ed25519-signed delivery log that survives restarts and refuses to start on a rewritten history, per-sender behaviour alerts (`fanout_spike`, `new_peer_burst`, `odd_hour_volume`, `canary_trip`), a single-call kill-switch (pause webhooks + revoke leases + quarantine + unregister, each reported) and canary tokens. Verified live by `TestDetectionCatchesAndContainsACompromisedAgent`
- **API** — 24 router paths registered in `cmd/server/main.go` (`HandleFunc`) — 19 always-on plus the 5 opt-in detection routes — documented as 16 paths / 20 operations in `docs/openapi.yaml`, wired with middleware and graceful shutdown
- **CI** — GitHub Actions, matrix build Go 1.26.6

Coverage numbers above are measured fresh per change (`go test -short -count=1 -cover ./internal/<pkg>`); the ≥70% gate lives in `make coverage-check`.

### Roadmap

- **CI-003b** ✅ — PostgreSQL persistence for registry and inboxes (implemented, `CR_DATABASE_URL`)
- **CI-007** ✅ — MCP server exposing registry and inbox tools (implemented, `cmd/crier-mcp`)
- **Capability-based routing** — route messages by agent capability cards

## License

MIT — see [LICENSE](LICENSE).
