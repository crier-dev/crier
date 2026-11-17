Crier README health sweep — READ producer transcript
Tick: crier-readme-2026-11-17-16-40-16
Date: 2026-11-17T11:52:21-05:00
Canonical: /home/kara/crier
Probe worktree: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip

=== IDENTITY ===
### CMD: git -C /home/kara/crier rev-parse HEAD
### CWD: /home/kara/crier
0aee643863fcdbeda8c224d9ae90cd18fe81325c
### RC: 0

### CMD: git -C /home/kara/crier rev-parse origin/main
### CWD: /home/kara/crier
0aee643863fcdbeda8c224d9ae90cd18fe81325c
### RC: 0

### CMD: git -C /home/kara/crier rev-list --left-right --count HEAD...origin/main
### CWD: /home/kara/crier
0	0
### RC: 0

### CMD: git -C /home/kara/crier remote -v
### CWD: /home/kara/crier
gitlab	git@gitlab.readydedis.com:totalwindup/crier.git (fetch)
gitlab	git@gitlab.readydedis.com:totalwindup/crier.git (push)
origin	git@github.com:crier-dev/crier.git (fetch)
origin	git@github.com:crier-dev/crier.git (push)
### RC: 0

### CMD: git -C /home/kara/crier status --porcelain
### CWD: /home/kara/crier

### RC: 0

### CMD: git -C /home/kara/crier rev-parse HEAD:README.md
### CWD: /home/kara/crier
bb2814a308d65c144ea6bf0dbf24f5c9367e3b17
### RC: 0

### CMD: git -C /home/kara/crier rev-parse 0aee643863fcdbeda8c224d9ae90cd18fe81325c:README.md
### CWD: /home/kara/crier
bb2814a308d65c144ea6bf0dbf24f5c9367e3b17
### RC: 0

### CMD: sha256sum README.md
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
8f3ab3d2e648a64b558f5cd20d11f31f8b83864d7fd2fc85b3815e6ec6283090  README.md
### RC: 0


=== FRESHNESS: release boundary ===
### CMD: git -C /home/kara/crier for-each-ref --sort=-creatordate --format=%(refname:short) %(creatordate:iso8601) refs/tags
### CWD: /home/kara/crier
v0.6.1 2026-11-11 16:33:38 -0500
v0.6.0 2026-11-09 13:48:01 -0500
v0.5.0 2026-11-08 12:32:53 -0500
v0.4.1 2026-11-07 21:17:04 -0500
v0.4.0 2026-11-07 17:47:14 -0500
v0.3.0 2026-11-06 10:39:14 -0500
v0.2.0 2026-10-20 22:41:29 -0500
v0.1.0-rc2 2026-09-21 08:53:59 -0500
v0.1.0-rc1 2026-09-20 05:27:10 -0500
### RC: 0

### CMD: git -C /home/kara/crier tag -l v0.7.0
### CWD: /home/kara/crier

### RC: 0

### CMD: git -C /home/kara/crier ls-remote --tags origin refs/tags/v0.7*
### CWD: /home/kara/crier

### RC: 0

### CMD: gh release list --repo crier-dev/crier --limit 3
### CWD: /home/kara/crier
v0.6.1	Latest	v0.6.1	2026-11-11T21:38:41Z
v0.6.0		v0.6.0	2026-11-09T18:51:28Z
v0.5.0		v0.5.0	2026-11-08T17:35:29Z
### RC: 0

### CMD: gh release view v0.7.0 --repo crier-dev/crier
### CWD: /home/kara/crier
release not found
### RC: 1

### CMD: gh release view v0.6.1 --repo crier-dev/crier --json tagName,isDraft,isPrerelease,publishedAt
### CWD: /home/kara/crier
{"isDraft":false,"isPrerelease":false,"publishedAt":"2026-11-11T21:38:41Z","tagName":"v0.6.1"}
### RC: 0

--- README version-pin lines ---
### CMD: grep -n v0\.7\.0\|v0\.6\.1\|v0\.6\.0\|release assets README.md
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
334:### Install prebuilt binaries (v0.7.0 release assets)
336:Release `v0.7.0` publishes 8 assets, the complete set: cross-compiled `crier`
340:been verified against these release assets, including checksum verification.
360:curl -fsSL https://raw.githubusercontent.com/crier-dev/crier/main/scripts/install.sh | sh -s -- --version v0.7.0 --dir "$HOME/bin"
380:and installs both binaries without relying on GitHub release assets:
407:builds carry the same identity as release assets: `scripts/release-artifacts.sh`
580:that produce the release assets, `Dockerfile`, `Dockerfile.mcp`) stamp it, and a
### RC: 0

--- CHANGELOG head ---
### CMD: sed -n 14,22p CHANGELOG.md
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
## [Unreleased]

## [0.7.0] - 2026-11-17

Range: `v0.6.1`..`v0.7.0` — 3 `feat`, 16 `fix`, 4 `perf`; the rest are
`chore`, `docs`, `board`, `foreman`, `test`, `dogfood`, `qa`, `audit`, `ops`,
`ci` and release bookkeeping. No total commit count is stated on purpose: it
changes with every commit pushed after this section is written, so re-derive
the census at the tag commit with
### RC: 0

--- README commits since v0.6.1 ---
### CMD: git -C /home/kara/crier diff --stat v0.6.1..origin/main -- README.md docs/ Makefile CHANGELOG.md go.mod scripts/install.sh
### CWD: /home/kara/crier
 CHANGELOG.md                                       |  67 ++
 Makefile                                           |  46 +-
 README.md                                          | 195 ++++-
 docs/architecture.md                               |   2 +-
 docs/claims.yaml                                   |  61 +-
 .../2026-11-12-mcp-restart-identity-real-use.md    | 136 +++
 docs/dogfood/2026-11-15-consumer-inbox-real-use.md | 154 ++++
 docs/dogfood/2026-11-15-provenance-repair.md       |  51 ++
 .../crier-dogfood-2026-11-12-qa-evidence.jsonl     |  26 +
 ...r-dogfood-2026-11-15-15-54-49-fire3-real-use.md | 110 +++
 .../crier-dogfood-2026-11-15-15-54-49-qa-ci.log    | 112 +++
 ...ood-2026-11-15-15-54-49-qa-evidence-fire3.jsonl |  24 +
 .../crier-dogfood-2026-11-15-15-54-49-qa-tests.log |  50 ++
 .../diagnostics-2026-11-12-mcp-restart-identity.md | 143 ++++
 docs/dogfood/diagnostics-2026-11-15-ci-act.md      | 101 +++
 docs/evidence/int010-gap-wave-20261117T0742.md     | 113 +++
 docs/evidence/int010-iteration-20261117T065225Z.md | 654 +++++++++++++++
 docs/evidence/int010-wave3-20261117T1152.md        |  62 ++
 docs/evidence/int025-a2a-lifecycle-20261117.md     |  59 ++
 docs/gate-posture.md                               |  37 +
 docs/harness-consumer-posture.md                   |  37 +
 docs/integration-guide.md                          | 114 ++-
 docs/mcp-capabilities.md                           |  29 +
 docs/openapi.yaml                                  | 929 ++++++++++++++++++++-
 docs/perf-002-benchmark.txt                        |  32 +
 docs/perf-lane-log.md                              |   4 +
 docs/performance.md                                |  79 ++
 docs/releases.md                                   |  54 +-
 docs/router-dispatch.md                            |  51 +-
 docs/service-delivery-auth.md                      |  27 +
 go.mod                                             |  39 +-
 31 files changed, 3461 insertions(+), 137 deletions(-)
### RC: 0


=== README size/structure ===
### CMD: wc -l README.md
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
2906 README.md
### RC: 0

### CMD: grep -n ^#\{1,3\}  README.md
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
1:# Crier — Agent-to-Agent Message Bus
27:## Architecture
31:### 1. Relay (Pub/Sub)
45:### 2. WebSocket Mesh (Peer-to-Peer)
55:### 3. Agent Registry
108:### 4. Inboxes
137:### 5. Webhook delivery (bypasses the inbox)
206:### 6. Hermes-session delivery (also bypasses the inbox)
255:### 7. Named groups (`@team:<name>`)
332:## Quick Start
334:### Install prebuilt binaries (v0.7.0 release assets)
376:### Install both binaries from source (available now)
411:### Prerequisites
551:### Build
602:### Make a keypair — `crier keygen`
626:### Address a foreign A2A agent — `crier a2a-client`
655:### Run
694:# Free port for the server instead of the default 8767:
698:# Host port 5437 taken as well? Move the container, and follow it with the URL:
707:### Stop / restart
796:### Try it
807:# …prints the agent id, the public key, the exact POST /agents body, and what to run next.
849:# 0. One-time setup: generate an ed25519 keypair for agent-1. EITHER source writes the
850:#    PKCS#8 PEM the server reads — `crier keygen` needs no openssl and no xxd and prints
851:#    the registration config:
852:#      ./bin/crier keygen -out /tmp/crier-agent.key -id agent-1 -json   # -> use its "public_key"
853:#    or, with openssl 3.x + xxd:
856:# sig helper: sig <key-file> <METHOD> <PATH> <TS> -> hex(ed25519_sign("METHOD\nPATH\nTS", key)) —
857:# same wire format as examples/demo.sh. The KEY FILE IS THE FIRST ARGUMENT, so the key is never
858:# baked in here: a `crier keygen -out <file>` key and an `openssl genpkey` key are the same
859:# PKCS#8 PEM and either works (this is TESTERS.md's helper, lifted verbatim).
860:# Requires OpenSSL >= 3 for pkeyutl -sign -rawin; on older OpenSSL it errors loudly
861:# instead of producing an empty (silently-401-rejected) signature. That flag is also a
862:# ONE-SHOT operation: the payload must be a seekable file passed with -in, so the helper
863:# refuses a zero-byte signature rather than sending it (a piped payload fails with
864:# "unable to determine file size for oneshot operation" and would 401 blaming the headers).
867:# 1. Register an agent (public_key = hex-encoded ed25519 public key; required
868:#    whenever signature enforcement is on — the default. Only a server run
869:#    with CR_REQUIRE_AGENT_SIG=false accepts registration without it.)
872:# 201 — a SECOND registration never overwrites its existing row:
873:#   * the SAME public_key answers the historical 409 {"error":"agent already
874:#     registered: \"agent-1\""}.
875:#   * a DIFFERENT public_key answers 409 {"error":"public_key_mismatch","message":"..."}.
876:# The stored registration and key remain unchanged in either outcome. For a key
877:# mismatch, DELETE /agents/agent-1 first, then register the fresh key, or register
878:# that identity under a new id. Re-running the quickstart with the same key is safe:
879:# skip the register step, or delete the agent first to start over (DF-CRIER-302).
881:# 2. Deliver a message to its inbox. An inbox delivery is agent-owned exactly
882:#    like a read, so under the default posture it proves the SENDER's key: sign
883:#    "POST\n/agents/agent-1/inbox\n<unix seconds>" with agent-1's own key and
884:#    send the trio. Unsigned it answers 401 before anything is stored — the
885:#    body's `sender` claim is not proof.
886:#    `ttl_seconds` is optional: absent keeps the 24h default, 0 means the message
887:#    never expires and is reported as
888:#    "expires_at":null (the key is present, the value is null — never the zero
889:#    time). `transport` in the body is
890:#    always present: "inbox" here, "webhook" when the target has a webhook
891:#    configured (then the inbox is bypassed — see §5 of the architecture notes).
897:# 201 {"id":"...","transport":"inbox","expires_at":"..."}  (expires_at = created_at + ttl_seconds)
898:# Drop the trio and it is 401 missing agent signature headers: reaching the
899:# inbox as a named sender requires that identity's key (INT-018).
901:# 3. Publish to a relay topic (pub/sub). `X-Agent-ID` is the header the relay
902:#    requires — the per-agent rate limiter keys on it and a publish without it
903:#    is 401. Signing is optional on ordinary topics, but offered proof is checked:
904:#    stale or bogus agent signature proof is refused with 401.
905:#    A2A task topics require proof when CR_REQUIRE_AGENT_SIG=true; retained
906:#    answers distinguish publisher-identity-verified from publisher-identity-claimed.
907:#    Without subscribers, ordinary events still answer 202 and are dropped (see §1).
913:# 202 — drop the trio and it is still 202; drop `X-Agent-ID` and it is 401.
915:# 4. Retrieve — agent-scoped endpoints require per-agent request signatures by
916:#    default (CR_REQUIRE_AGENT_SIG=true). This covers inbox delivery (step 2),
917:#    inbox retrieve/ack/stats AND DELETE /agents/{id}. Headers:
918:#      X-Agent-ID  agent id
919:#      X-Agent-Ts  unix seconds (must be within ±30s of the server clock)
920:#      X-Agent-Sig hex ed25519 signature over "METHOD\nPATH\nTS"
921:#    Timestamps are generated fresh below — never hardcode them (stale timestamps
922:#    are rejected with 401).
923:#    Those four checks run in this order, so the FIRST one to fail names the status:
924:#      1. signature trio present?  no -> 401 "missing agent signature headers (X-Agent-ID, X-Agent-Ts, X-Agent-Sig)"
925:#      2. agent exists?            no -> 404 "agent not found"
926:#      3. timestamp within ±30s?   no -> 401 "request timestamp outside allowed window (±30s)"
927:#      4. signature valid?         no -> 401 "signature verification failed"
928:#    The agent EXISTENCE check (2) sits between the trio check and the timestamp /
929:#    signature checks, so an UNREGISTERED agent id answers 404 "agent not found"
930:#    — never 401 — as soon as a signature trio is present. The stale-timestamp 401
931:#    in (3) is measured on a REGISTERED agent (agent-1): an unknown id never gets
932:#    that far.
933:#    Reproduce both sides — same call, same id in X-Agent-ID and in the path, a
934:#    fresh X-Agent-Ts and a well-formed 128-hex X-Agent-Sig that cannot verify:
935:#      GET /agents/agent-1/inbox         -> 401 "signature verification failed"
936:#      GET /agents/ghost-agent-xyz/inbox -> 404 "agent not found"
940:# 200 {"messages":[{"id":"...","payload":"eyJoZWxsbyI6IndvcmxkIn0=","lease_id":"..."}],"lease_id":"...","queue_depth":1,"leased_count":1}
941:# queue_depth/leased_count disambiguate an empty batch: messages=[] with
942:# leased_count>0 = everything queued is HELD under a lease (DF-CRIER-177)
943:# Note: message payloads are base64-encoded on the wire ([],byte form)
945:# 5. Ack the message — message_ids is REQUIRED (an ack without it is rejected
946:#    with 400: it would otherwise be a silent no-op and the message would be
947:#    redelivered after lease expiry). Sign "POST\n/agents/agent-1/inbox/ack\n<ts>".
952:# 204 — message permanently removed (never redelivered after lease expiry)
954:# Dev shortcut: disable signing for trusted single-user setups. The durable
955:# backend is still the documented one; drop the CR_DATABASE_URL assignment and
956:# the server runs the demo-only in-memory backend instead.
958:# With signing disabled, POST /agents no longer needs a public_key either —
959:# register with just an id:
960:#   curl -s -X POST localhost:8767/agents -d '{"id":"agent-1"}'        # 201
961:#   (a duplicate of this keyless call gets the historical 409 "agent already
962:#    registered" response. For registrations that supply a public_key, step 1
963:#    distinguishes SAME public_key from DIFFERENT public_key / 409 public_key_mismatch,
964:#    including the delete/re-register remedy.)
965:# The same keyed outcomes are also served with the id in the path —
966:# POST /agents/{id} with the key in the body answers the same bodies.
967:# A keyless agent registered this way is unusable on a server that enforces
968:# signing (its agent-scoped calls answer 401 "no registered public key"), so
969:# re-enabling CR_REQUIRE_AGENT_SIG later means re-registering with a key.
971:# 200 — no signature headers required
973:# 6. Delete the agent — DELETE /agents/{id} requires the same per-agent
974:#    signature (not just inbox endpoints). Sign "DELETE\n/agents/agent-1\n<ts>".
978:# 204 — agent removed (401 without the signature headers)
994:### Remote MCP mode (crier-mcp)
1012:# Run the MCP bridge against the remote server (all four vars are read at startup)
1015:# Recommended for any long-lived bridge: a stable key whose public half the
1016:# server keeps across restarts. Optional — see "Signing keys" below.
1019:# Optional shared bearer token, only when the server runs with CR_AUTH_TOKEN:
1020:# export CRIER_AUTH_TOKEN=...
1021:# CRIER_AUTH_TOKEN is the bridge's own name; CR_AUTH_TOKEN is accepted as an
1022:# alias for the same shared secret (setting only CR_AUTH_TOKEN works — the
1023:# bridge logs which variable it used, and warns if both are set to different
1024:# values, in which case CRIER_AUTH_TOKEN wins).
1025:# Required when the server uses CR_SERVICE_DELIVERY_ALLOWLIST:
1026:# export CR_SERVICE_DELIVERY_AUDIENCE=<audience-name>
1210:### Try the Mesh
1247:# Terminal A — connect as agent-1 and paste the REGISTER frame below. Any
1248:# RFC3339 timestamp works, and the server answers with exactly one
1249:# REGISTER_ACK frame (the handshake is still one-way: nothing waits on it).
1252:# Terminal B — the peer is now visible (no token needed unless CR_AUTH_TOKEN is set):
1254:# {"peers":[{"agent_id":"agent-1"}],"count":1}
1429:### Test
1432:# Full test suite. The Makefile pins a 60s per-package timeout; when the
1433:# whole suite runs in parallel on a busy host, cmd/server and
1434:# internal/session can exceed it and report "panic: test timed out after
1435:# 1m0s" even though every package passes. Give the same suite headroom:
1436:#   go test ./... -count=1 -timeout 300s
1439:# Short (no integration tests)
1442:# Vet
1446:### The demo harnesses never measure a server they did not start
1474:### A leaked server set is visible from inside the repo
1500:## Web chat client (CR-CHAT-009)
1516:### Session audiences — what a `201` does and does not mean (DF-CRIER-479)
1549:### Session webhook budgets and retries
1590:## Message guard (LLM)
1608:## Agent Ecosystem
1620:# 1. the long-lived stack only — crier + the agent consumers. The battery is
1621:#    profile-gated, so `up` never runs it (DF-CRIER-88/90).
1624:# 2. the battery is a ONE-SHOT, explicit command: it runs exactly once, builds its
1625:#    own image if it is missing, and never rebuilds or recreates the stack — so
1626:#    re-running it leaves crier's container (and every agent registration) intact.
1641:## Detection & containment (CR-FEAT-030)
1689:### Catching and containing a compromised agent
1703:# detection on, log into a file you keep
1707:# One keypair per agent in the scenario (peer-1 .. peer-5, the compromised
1708:# sender, the operator). Each keygen writes <id>.key (PKCS#8 PEM, mode 0600) and
1709:# prints that identity's registration body:
1710:#   {"id":"peer-1","public_key":"<ed25519 public key — 64 hex characters>"}
1738:# Server posture for this walkthrough: CR_REQUIRE_AGENT_SIG=false (see above).
1761:# Server posture for this walkthrough: CR_REQUIRE_AGENT_SIG=false (see above).
1780:# Server posture for this walkthrough: CR_REQUIRE_AGENT_SIG=false (see above).
1792:# Server posture for this walkthrough: CR_REQUIRE_AGENT_SIG=false (see above).
1803:# Server posture for this walkthrough: CR_REQUIRE_AGENT_SIG=false (see above).
1815:## Priority lanes & real backpressure (CR-FEAT-035)
1826:### Priority — an optional `priority` on a delivery
1844:# A low-value backlog, then one urgent message BEHIND it.
1848:# The urgent message comes back FIRST, wherever it arrived.
1850:# Out of range is refused, never clamped, and stores nothing:
1852:# 400 {"error":"priority must be 0..9"}
1869:### Backpressure — a global ingest budget, and a `Retry-After` on every 429
1883:# …the fourth delivery inside the minute:
1884:# HTTP/1.1 429 Too Many Requests
1885:# Retry-After: 60
1886:# {"error":"RATE_LIMITED_GLOBAL","scope":"global","limit_per_minute":3,"retry_after_s":60}
1907:### Depth — where operators already look
1920:# TYPE crier_inbox_queue_depth gauge
1922:# TYPE crier_inbox_queue_leased gauge
1924:# TYPE crier_inbox_queue_oldest_age_seconds gauge
1926:# TYPE crier_inbox_shed_total counter
1944:### Federation peer authentication (CR-CHAT-024)
1948:## Namespaces (realms) — per-realm policy (CR-FEAT-029)
1971:# … or CR_NAMESPACES_FILE=/etc/crier/namespaces.json (set at most one of the two)
2008:# register into a realm (a realm the server does not declare is refused)
2010:# deliver: the realm is implied by the target, and recorded with the message
2012:# a crossing attempt is refused, and nothing is stored
2014:# publish inside a realm (the realm is a HEADER — the per-realm limit is evaluated first)
2017:# what is actually enforced, with a live per-realm agent census (no secrets)
2034:## Configuration
2129:### Service delivery on the in-memory backend (dev recipe)
2149:# 1. two identities, each with its own key (no openssl, no xxd)
2153:# 2. boot the DEFAULT in-memory backend, declaring both identities
2166:# 3. one signed service delivery over the allowlist, then read + ack it as the
2167:#    recipient. Reads are self-only, NOT protected deliveries, so the receiving
2168:#    client is built without an audience; only the protected POST carries one.
2191:# a. the declaration is what boots it — omit it and the SAME command still refuses:
2195:# exit 1, level=ERROR msg="configure service delivery authorization"
2196:#   error="service identity \"agent-b\" is not pre-provisioned: agent not found: \"agent-b\""
2198:# b. a durable store keeps its own agents, so seeding it is refused:
2201:# exit 1, level=ERROR msg="apply CR_REGISTRY_SEED_IDENTITIES"
2202:#   error="CR_REGISTRY_SEED_IDENTITIES applies only to the in-memory registry
2203:#   backend ... provision new identities with POST /agents"
2206:### Delivery ACL — principals, roles and grants (CR-CHAT-003)
2299:### Task Lifecycle — create, claim, complete (CR-CHAT-030, DF-CRIER-304)
2364:# Posture: CR_PERMISSIONS_ENABLED=true with CR_PERMISSIONS_ADMIN_TOKEN set (the ACL is
2365:# armed), and CR_REQUIRE_AGENT_SIG=false so agents register keyless (the dev shortcut
2366:# above). TS is a LIVE timestamp — the recipes are re-runnable, nothing is hardcoded.
2373:# 0. Two agents and a session. The creator is REQUIRED: a session that cannot name one
2374:#    is 400 CREATED_BY_REQUIRED, never defaulted to a service identity.
2381:# 1. THE ASYMMETRY, while alice is unclassed — same sender, same session, same target:
2386:# → 403 {"error":"TASK_FORBIDDEN","reason":"NO_TASK_AUTHORITY",
2387:#        "detail":"calling agent \"alice\" has no owner principal to act as (§6.3, §8.2 Q2)"}
2389:# 2. THE PREREQUISITE (admin token): mint an owner principal → bind it to alice → class
2390:#    alice `personal` with that owner. Until all three land, step 1 is the only answer.
2399:# 3. The task is created — 201, with the task's OWN id and state lifted to the top level
2400:#    (the message view's `id` is the MESSAGE id, so read `task_id`).
2406:# 4. THE ADDRESSEE HITS THE SAME WALL: bob is not classed, so he cannot claim the work
2407:#    addressed to him — 403 TASK_FORBIDDEN / NO_TASK_AUTHORITY, exactly as in step 1.
2410:# 5. … so bob gets the same three writes (one owner principal may hold many agents),
2411:#    and THEN claims: 201, the owner is named, a new record-version in the same thread.
2417:# → 201 {"state":"claimed","owner":"bob","task_id":"…"}
2419:# 6. THE 409 LADDER: the task is no longer `open`, and the refusal says so.
2421:# → 409 {"error":"INVALID_TASK_TRANSITION",
2422:#        "detail":"task \"…\" is \"claimed\"; only an open task can be claimed"}
2424:# 7. COMPLETE — accepted from ANY actor with `send` on the session (no owner check):
2425:#    alice closes bob's task, and the completion payload lands in the thread.
2429:# → 409 {"error":"INVALID_TASK_TRANSITION",
2430:#        "detail":"task \"…\" is \"done\"; only a claimed or running task can be completed"}
2439:### Dagger control — create, observe, cancel, resume, rewind (CR-CHAT-033)
2493:# create a run for agent hermes-1 (the run id comes back from the executor)
2497:# observe it, then cancel it
2564:### Registry and inbox storage
2625:### Durable backend (PostgreSQL)
2654:# container <project>-postgres-1; the URL still uses localhost:5437 (or your CRIER_PG_HOST_PORT override)
2659:## API
2689:### Runtime posture — `GET /status`
2740:## Observability (metrics & profiling)
2749:### Namespaced metric names (MON-CRIER-005 rename; dual-emit window closed by MON-CRIER-007)
2810:## Documentation
2826:## Contributor hook health
2877:## Project Status
2893:### Roadmap
2904:## License
### RC: 0


=== DOCS GATE ===
### CMD: make docs-check
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
go test -short -count=1 -run 'TestDocsClaims' ./cmd/server
ok  	github.com/crier-dev/crier/cmd/server	6.450s
### RC: 0


=== ANCHORS ===
### CMD: python3 /home/kara/.hermes/skills/coding-hermes/coding-hermes-readme/scripts/check-md-anchors.py README.md
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
anchors_checked=32 broken=0
### RC: 0


=== LINKS (local + external) ===
### CMD: python3 /home/kara/.hermes/skills/coding-hermes/coding-hermes-readme/scripts/readme-link-check.py README.md
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
MISSING README: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/README.md/README.md
### RC: 2


=== STRUCTURE CENSUS ===
### CMD: python3 /home/kara/.hermes/skills/coding-hermes/coding-hermes-readme/scripts/readme-structure-census.py README.md --cli-name crier
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
file=README.md bytes=237224 lines=2906

== H2 vs Contents ==
h2_total=15 contents_bullets=0
h2_without_toc=15 [(27, 'Architecture'), (332, 'Quick Start'), (1500, 'Web chat client (CR-CHAT-009)'), (1590, 'Message guard (LLM)'), (1608, 'Agent Ecosystem'), (1641, 'Detection & containment (CR-FEAT-030)'), (1815, 'Priority lanes & real backpressure (CR-FEAT-035)'), (1948, 'Namespaces (realms) — per-realm policy (CR-FEAT-029)'), (2034, 'Configuration'), (2659, 'API'), (2740, 'Observability (metrics & profiling)'), (2810, 'Documentation'), (2826, 'Contributor hook health'), (2877, 'Project Status'), (2904, 'License')]
toc_entries_without_heading=[]
WARN every heading reports NO-TOC -> treat this census as a parser artifact, not a finding

== documented commands ==
table_rows=181 distinct=155
documented_more_than_once={'make build': [585, 586], 'register_agent': [1073, 1121], 'unregister_agent': [1076, 1123], 'retrieve_inbox': [1078, 1122], 'get_messages': [1082, 1124], 'ask_agent': [1083, 1125], 'mesh_peers': [1084, 1126], 'mesh_request': [1085, 1127], 'create_run': [1086, 1128], 'courier_unread_summary': [1092, 1129], 'harness_list': [1094, 1130], 'REGISTER': [1299, 1300, 1301], 'REGISTER_ACK': [1302, 1303], 'REQUEST': [1305, 1306, 1307, 1308, 1309], 'RESPONSE': [1310, 1311, 1312, 1313], 'ERROR': [1314, 1315], 'POST': [2485, 2487, 2488, 2489, 2490]}
### RC: 0


=== BOARD STATE (README/DOC rows) ===
### CMD: jq README/DOC rows from tasks.jsonl
DOC-1	complete	P3
README-001	complete	P2
README-002	complete	P2
DOC-CRIER-1	complete	P5
README-003	complete	P4
README-004	complete	P4
README-005	complete	P5
README-006	complete	P5
README-008	complete	P4
DOC-2	complete	P4
DOC-3	complete	P4
DOC-4	complete	P4
DOC-5	complete	P4
DOC-6	complete	P4
README-009	complete	P4
README-010	complete	P4
README-011	complete	P5
README-012	complete	P4
README-013	complete	P5
README-014	complete	P5
DOC-007	complete	P3
README-015	blocked	P5
README-016	complete	P4
### RC: 0

### CMD: doc-severity census by prefix
      2 P2
      2 P3
     12 P4
      7 P5
### RC: 0
=== END ===
