# Crier Product Requirements Document

**Status:** Proposed — owner review required; this document does not authorize implementation.

**Updated:** 2026-10-12

**Baseline reviewed:** `d6a8e2a9fad07c4c102fc496706ea4ccb6e8205e` (`main`)

**Owner:** Crier project owner

**Repository:** `crier-dev/crier`

> **One-line product:** Crier is the self-hosted communication layer where people and agents exchange addressed messages, durable inbox work, threaded session history, and trusted cross-instance traffic—without becoming a second scheduler or workflow executor.

## 1. The decision brief

The codebase has a substantial agent-messaging substrate and a session API, but the browser client is still an MVP. Three decisions determine whether the current product meets the original “Postgres-free, human-usable Crier” intent:

1. **What must work without PostgreSQL?** *Recommendation:* SQLite or JSONL must durably cover the full core bus—registry, inboxes, and sessions—not sessions alone. Effort: **L**. Today only session storage has SQLite/JSONL adapters; registry and inbox storage are PostgreSQL-backed or in-memory.
2. **How does a human authenticate in `/chat`?** *Recommendation:* use a principal-bound browser session/cookie flow. Do not put the deployment-wide Bearer token in a URL or static client. Effort: **L**, dependent on the identity-provider decision.
3. **What is the signing guarantee on relay publishes?** *Recommendation:* keep compatibility by default, but add an explicit opt-in signed-publish policy for deployments that need cryptographic publisher attribution. The current agent-signature flag does not authenticate relay publishes. Effort: **M**.
4. **Are the original UI pictures required for visual fidelity?** The four reference PNGs are missing from their old cache paths. The written `CHAT-INTERFACE.md` and `docs/mock-ui-cr-chat-011.html` remain. *Recommendation:* re-upload the pictures if pixel-level fidelity matters; otherwise use those surviving artifacts as the interim visual authority. Effort: **S** for asset recovery, not implementation.

## 2. The problem Crier solves

Agents need more than a publish/subscribe channel. They need a stable identity, a way to find one another, a durable place to leave work for an offline peer, and a receipt that says whether the work was accepted, read, acknowledged, refused, expired, or transferred. People need to see where that work happened, who was addressed, what the agent actually received, and which permissions or trust rules allowed it.

The historical cases show why the contract must be explicit:

- **Mesh-identity review:** the review found that a mesh connection could claim the agent id in its URL path without proving possession of that agent’s key. Crier now has an Ed25519 mesh challenge, but `CR_REQUIRE_MESH_AUTH` defaults to **false** for compatibility. The control exists; deployments must arm it.
- **Scratch-port self-test portability:** the current open P1 says the self-test fails deterministically in a throwaway HOME when Go 1.26.6 cannot bootstrap, because the toolchain download targets that HOME; `act` also fails the self-test’s `ss` prerequisite gate with exit 2. This is a QA portability failure, not a Crier protocol failure, but it makes the gate unreliable in those environments.
- **This review’s live UI probe:** with `CR_AUTH_TOKEN` enabled, `GET /chat` returned 401 without a Bearer header; the HTML has no token-entry or browser-login flow. At a 390px viewport the current page overflowed horizontally by 368px.

These are not reasons to replace Crier’s existing message paths. They are reasons to finish the human-facing and deployment contracts around the same API.

## 3. Value case

Crier should let a team integrate each agent once, then reuse its identity and communication path across direct messages, named groups, capability pools, durable inboxes, sessions, and trusted peers. A shared implementation avoids writing a different queue, signature helper, and transcript store for every agent runtime.

The return is repeated use: a single improvement to identity, storage parity, and delivery receipts benefits every agent and every project connected to the bus. The claim is conditional: a PostgreSQL-free session database alone is not a PostgreSQL-free durable bus, and a `/chat` page that cannot authenticate in a protected deployment is not yet a usable human interface.

## 4. Users and user stories

1. **Human operator:** As a person responsible for an agent, I can enter an authenticated session, speak as an authorized principal or bound agent, branch deliberately, and see the resulting transcript and delivery status.
2. **Agent builder:** As an agent developer, I can use REST, WebSocket, a first-party Python/TypeScript client, or MCP tools to register, deliver, retrieve, acknowledge, and observe messages without inventing a second protocol.
3. **Crier operator:** As an operator, I can run Crier locally without PostgreSQL, inspect which backend each subsystem is using, and know whether a message survives a restart.
4. **Federation operator:** As an operator of two Crier instances, I can trust a named peer key, scope what crosses the link, revoke that peer, and verify that untrusted or revoked signatures are rejected.
5. **Workflow owner:** As a human or agent, I can ask Crier to create or observe a Dagger run from a conversation while Dagger remains the execution authority and the run result returns through the normal message/inbox path.

## 5. Acceptance criteria

Each criterion is executable. The first four map to the user stories above; the remaining criteria protect the shared contract.

- **AC-1 — Browser identity (C):** Given a deployment with authentication enabled, when a human opens `/chat`, then the page provides a secure principal-authentication path and can read sessions without a manually injected header or token in the URL. Proof: browser E2E records the login request, authenticated `GET /sessions` response, and principal identity; an invalid session receives a visible 401/403 state.
- **AC-2 — UI message effect (C):** Given an authenticated principal bound to an allowed agent and a session containing its authorized participants, when the user sends a root message or deliberate reply, then the backend transcript records the correct author, parent/root relationship, sequence, and per-target outcome. Proof: click the UI control, then read back the same session id through the API; a refused target is shown as refused, never as delivered.
- **AC-3 — Mobile fit (T/C):** Given viewport widths 320, 360, 390, 414, 640, 641, 700, 767, 768, 1024, and 1280px, when `/chat` is loaded, then `document.documentElement.scrollWidth - clientWidth` equals zero and every primary control is visible or reachable. Proof: browser DOM sweep with the measured result at each width.
- **AC-4 — Shared API and existing clients (T):** Given the same Crier server, when the Python and TypeScript first-party clients perform register, deliver, signed retrieve, ack, and relay publish/subscribe, then both produce the same documented result and the existing bus routes remain unchanged. Proof: `make client-roundtrip-check` plus the full Go suite.
- **AC-5 — Authentication and identity boundaries (T/C):** Given two registered agents with distinct Ed25519 keys, when one agent reads or acknowledges the other agent’s inbox, then the server returns 403; a missing or invalid signature on a signed read returns 401. Proof: `clients/python/two_agents.py` and the server’s negative signature controls.
- **AC-6 — Persistence across engines (T):** Given a common record fixture, when sessions are written and read through SQLite and PostgreSQL, then transcript order, thread links, idempotent replay, and permissions are equivalent. Given a process restart, the same ids and message records are read back. Proof: `scripts/acceptance-matrix.sh` cells `2a-sqlite-relaunch`, `2b-postgres`, and `3-session-threads`.
- **AC-7 — Whole-bus PostgreSQL-free mode (C):** If the intended requirement is that the *whole bus* be durable without PostgreSQL, then with PostgreSQL absent, registry records and inbox messages as well as sessions must survive restart through a supported SQLite or JSONL backend. Proof: register → deliver → restart → signed retrieve/ack, followed by registry and inbox readback. This is a proposed interpretation of the stated Postgres-free requirement; narrow it to sessions only if that was the intended boundary.
- **AC-8 — Federation trust (T/C):** Given two Crier instances, a directly trusted peer’s valid signed delivery is accepted; a wrong-key or revoked peer is refused; no third-party key becomes trusted without explicit delegation. Proof: two-instance federation battery with valid, wrong-transcript, revoked, and delegated-key cases.
- **AC-9 — Honest signing claims (T):** Given a relay publish with a valid and invalid signature, then the configured publish policy either verifies both and refuses the invalid signer, or explicitly documents that this route uses `X-Agent-ID` only. The UI and docs never imply that `CR_REQUIRE_AGENT_SIG` authenticates relay publishes unless the live route checks it. Proof: live signature matrix and route-specific documentation.
- **AC-10 — Dagger boundary and deployment (C):** Given `CR_DAGGER_URL` absent, Dagger routes are not registered; when configured, create/status/cancel/resume/rewind/skill operations use the configured external executor and preserve its run id and evidence references. Proof: built binary and Docker release cells, including a fresh-box Dagger-configured boot.
- **AC-11 — Release evidence (C):** Given a release candidate, the built server and MCP binaries and Docker image pass all six documented acceptance-matrix cells, with any skip explicitly reasoned. Proof: `scripts/acceptance-matrix.sh` returns a 6-cell summary with zero unexpected skips.

**Anti-gaming rule:** an HTTP 2xx, a green page, or a “complete” board row is not sufficient. For state-changing flows, the test must read back the exact session, agent, message, peer, or run id and verify its stored outcome after the relevant restart boundary.

## 6. Design and current implementation status

- **Message bus — shipped:** relay over HTTP/WebSocket, peer mesh, agent registry, lease/ack inbox, webhooks, and federation are present. **Requirement:** keep existing routes and semantics additive; treat a delivery as accepted only when the response and persisted outcome agree.
- **Sessions and threads — shipped:** one session Store contract has SQLite, PostgreSQL, and JSONL modes. SQLite/PostgreSQL share repository/query logic and use dialect-specific adapters. **Requirement:** keep source messages authoritative; generated summaries are indexes with provenance, not replacements.
- **Registry and inbox persistence — partial:** the current registry/inbox implementations are memory and PostgreSQL. Without `CR_DATABASE_URL`, those records are in memory and disappear on process exit. **Requirement:** if PostgreSQL is optional for the whole product, add an embedded/portable durable adapter; do not describe SQLite session storage as full-bus durability.
- **Identity and permissions — partial:** per-agent Ed25519 signatures protect selected agent-scoped HTTP operations; permission management has a separate admin token; delivery ACL is opt-in (`CR_PERMISSIONS_ENABLED=false` by default). **Requirement:** human login, browser identity, bound-agent attribution, grant enforcement, and admin-token configuration must be explicit and testable.
- **Mesh and relay signing — partial:** mesh authentication exists behind `CR_REQUIRE_MESH_AUTH`, default false. The documented relay publish path uses `X-Agent-ID` but does not verify the signature. **Requirement:** name the security guarantee per route; do not imply one flag authenticates every transport.
- **Federation and trust — implemented in code/specs; not live-tested in this review:** peer identity, signed peer auth, trust/revocation structures, and opt-in surfaces are present. **Requirement:** keep trust direct by default; require explicit delegation for transitive trust; run the two-instance battery before claiming release readiness.
- **Chat UI — MVP/partial:** `GET /chat` serves a static client of the same session API. Source implements session creation, transcript reading, permission display, send, and reply; this review live-tested transcript reading and send/reply, not the UI’s create button. **Requirement:** add secure browser auth, principal-aware sender selection, onboarding for missing agents, mobile layout, and honest guard/delivery status. The current UI says guard verdict is not exposed.
- **Interoperability — shipped surfaces:** OpenAPI 3.1 exposes 54 path entries; the stdio MCP bridge registers 19 tools. A2A is opt-in; no ACP implementation was found in the tree. **Requirement:** keep adapters additive and backed by the same Crier records; state which external protocols are not implemented.
- **Dagger — opt-in client integration:** Crier calls the configured Dagger HTTP/JSON surface; it is not a second executor or scheduler. **Requirement:** preserve that boundary, prove a fresh-box configured boot, and return run outcomes through the ordinary Crier path.
- **Operations — partial:** health/build identity/status, opt-in metrics/profiling, Docker, and release acceptance machinery exist. **Requirement:** status must expose each persistence backend; release proof must cover built artifacts, not only source tests.

**Board/source reconciliation:** the pending admin-token documentation row says `CR_PERMISSIONS_ADMIN_TOKEN` has zero README/docs mentions, but the current README has a mention in the ACL section; the remaining gap is the environment table and operator setup guidance. The pending key-rotation row’s 409 behavior is confirmed, but the current body is `agent already registered: "<id>"`, not the row’s quoted string. The key remains unchanged after the rejected replacement.

## 7. Scope and boundaries

**In scope:** the communication contract, human-to-agent chat and sessions, identity/permissions, durable agent messaging, federation trust, portable persistence, and deployment/release proof. The UI remains a client of the same REST/WebSocket API; it must not create a second message store or delivery path.

**Out of scope:** a second workflow executor, a replacement for the scheduler/board, automatic execution triggered by a bare `@tag`, or a wholesale Matrix/NATS replacement. Crier may call Dagger and interoperate with other protocols, but those systems remain their own authorities.

**Current visual authority:** `specs/CHAT-INTERFACE.md` and `docs/mock-ui-cr-chat-011.html`. The four original PNG references named by the spec are missing from the old image-cache paths. This PRD does not invent a replacement visual direction.

## 8. Success criteria as replayable proofs

1. Replay the signed inbox contract: `clients/python/round_trip.py` succeeds, unsigned retrieve returns 401, wrong-key retrieve returns 401, and the two-agent cross-inbox ack returns 403.
2. Replay SQLite durability: create a session, root message, and reply; restart the process; read the same session id and parent link back. The review already exercised this for the session store.
3. Replay the release matrix against built artifacts: all six cells pass; PostgreSQL and federation cells run against their own test services.
4. Replay the browser auth and mobile gates: authenticated `/chat` works without URL token leakage, and every viewport in AC-3 measures zero page overflow.
5. Replay the key-rotation case: document the 409 behavior or implement old-key-possession rotation; a new key is never silently substituted.
6. Replay the scratch-port self-test in a clean HOME and in the supported CI harness; it either passes or records a named, truthful skip for an unsupported environment.
7. Do not claim a capacity ceiling from this review. A separately scheduled bounded 100-agent/500-message probe is required when host load is below the project gate.

## 9. Effort and dependencies

These are rough planning classes, not measured delivery dates.

- **Browser principal auth and sender binding — L:** choose the supported identity/session mechanism; preserve the API auth boundary.
- **Responsive chat surface and honest empty/error states — M:** use the current written visual spec; re-upload original images for exact fidelity.
- **Durable registry/inbox adapter for PostgreSQL-free installs — L:** confirm the whole-bus interpretation of AC-7; reuse common repository/query contracts.
- **Route-specific signing policy and visible provenance — M:** decide whether relay signing is opt-in or required for selected namespaces.
- **Admin-token docs and key-rotation contract — S–M:** reconcile the pending documentation and rotation items with current source.
- **Fresh-box Dagger boot and QA self-test portability — M:** reconcile the pending fresh-box and self-test items in their target CI/toolchain environments.

## 10. Risks and mitigations

- **Shared Bearer token in a browser:** if the UI exposes it in source, local storage, or URLs, it becomes a deployment-wide credential. Use a principal-bound browser session and keep service credentials server-side.
- **Misleading Postgres-free claim:** SQLite sessions can coexist with volatile registry/inbox memory. Report the backend for each subsystem separately; only call a mode durable when every required record survives restart.
- **Identity drift in the UI:** a hard-coded `web-client` sender can create a transcript entry whose delivery is refused. Bind actions to the current principal/agent and render the server’s actual outcome.
- **Security flag defaults:** mesh auth and delivery ACL are opt-in for compatibility. Operator docs must say exactly which flag is off, what it protects, and what remains exposed.
- **Federation claims without a second instance:** source and unit tests do not prove live cross-instance trust/revocation. Keep this marked unverified until the two-instance battery runs.
- **Scale extrapolation:** the Carter review skipped the 100/500 probe because host load was above the project’s safe threshold. Do not convert the local latency sample into a throughput promise.

## 11. Why others would want it

Agent builders can integrate once through HTTP, WebSocket, MCP, or first-party clients. Operators get receipts, leases, identity, and a way to keep offline work. Teams that need a human-facing surface get sessions and threads that reference the same messages agents use. Crier is most valuable when the same conversation must remain inspectable across people, agents, and a federated boundary; it is not a replacement for a dedicated stream broker or durable workflow engine.

## 12. Sources and provenance

- Product source: `README.md`, `docs/architecture.md`, `TESTERS.md`, `docs/acceptance-matrix.md`.
- Normative comms specs: `specs/CHAT-INTERFACE.md`, `CHAT-SESSIONS.md`, `CHAT-PERMISSIONS.md`, `CHAT-ADDRESSING.md`, `CHAT-STORAGE.md`, `CHAT-FEDERATION.md`, `CHAT-TRUST.md`.
- Live task source: `.coding-hermes/board/tasks.jsonl`, deduplicated last-write-per-id at baseline `d6a8e2a9fad07c4c102fc496706ea4ccb6e8205e`.
- Historical design brief: the local Crier interface planning file outside this repository (its old per-goal status labels were not reused as current status).
- Evidence snapshot: the owner-facing Carter-style review and raw localhost transcripts are delivered separately from this public PRD.
