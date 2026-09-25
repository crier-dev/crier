# Crier diagnostics — 2026-09-25 addendum: the identity you rate-limit is the identity you cannot verify

Context: runs 1-9 documented the relay, mesh, inboxes, federation and the
lifecycle pitfalls. This run exercised the rate limiter and the auth-exempt
reference surfaces for the first time, and the interesting lesson is about
**which identity a check can trust** — not about any crier defect.

## How the identity system actually splits (and why)

Crier has two identity tiers on one ed25519 registry:

- **Verified identity** — inbox retrieve, inbox ack, agent PATCH. The
  signature trio is checked against the key registered for `X-Agent-ID`, with
  a ±30 s timestamp window. Spoofing here gets a precise 401
  (`signature verification failed` / `request timestamp outside allowed
  window (±30s)`), and the wrong-key case is indistinguishable from the
  broken-key case to the caller — correct, because both mean "you are not
  this agent".
- **Claimed identity** — relay publish. The trio is presence-only BY DESIGN
  (README quickstart says so; `docs/claims.yaml`
  `STATUS-README-RELAY-PUBLISH-BOGUS-SIG-202` gates the 202 live). Publish is
  fire-and-forget and the event is dropped with zero subscribers, so there is
  no durable payload to protect.

The consequence a user should understand: **the rate limiter's per-agent
budget is only as strong as the claim.** Any client can publish with
`X-Agent-ID: <someone else>` (202) and spend that agent's 100/min, or rotate
never-registered ids to avoid limiting entirely. If a deployment ever cares,
the design lever is an authenticated-publish mode — not a bug fix, a posture
choice. Meanwhile `GET /status` tells you which posture you are running
(`require_agent_signature`, `rate_limit_per_minute`).

## Errors hit this run, and the right way past each

1. **`:8767` already held by a leftover server.** TESTERS.md's own §1 advice
   (check `ss -tlnp` first, start on a scratch port) is the right way; the
   leftover was 4 hours old, from a previous lane. The `-pidfile` pairing is
   what makes `make stop` safe — use it even for throwaway runs.
2. **`bunker-qa.sh launch` died in upgrade-prep** (empty `docker pull` arg →
   `DETECT_UP_PREV_DIR` unbound under `set -u`) before any cell ran. This is
   fleet tooling, not crier (DF-CRIER-285 has the details). The right way
   when the battery dies at launch: the repo sync already succeeded, so run
   the documented install path by hand on the same agent and destroy it —
   a SKIPPED row is only honest when the machine itself is unreachable.
3. **`curl -sL https://go.dev/dl/...` truncated its write on retry**
   (curl error 23, 66 MB partial file). `dl.google.com` (the canonical
   download host) worked clean. On a fresh box, verify the tarball extracts
   and `go version` answers before blaming the repo.
4. **`xxd` missing on bare Debian 13** — the known DF-CRIER-280 class, hit
   again independently. Coreutils `od` is the portable substitute for the
   hex helpers: `... | tail -c 32 | od -An -tx1 | tr -d " \n"` produces the
   identical pubkey hex, and the same trick works for the signature
   (`od -An -tx1` in place of `xxd -p -c 128`).
5. **409 on re-register, 403 on cross-agent retrieve.** Both are the system
   working: re-registering an existing id is a conflict, and the retrieve
   that carried `X-Agent-ID: rt` against `/agents/rt2/inbox` was refused with
   an error naming BOTH ids — the clearest authz message in the codebase.
   When scripting a round-trip, derive the resource path and the header id
   from ONE variable, or you will re-create this exact mismatch.

## What "working" sounded like this run

- Rate limiter: exactly 100×202 then 429, per-agent isolation (a second agent
  publishes through the first's 429), reset on the next window.
- `GET /docs` renders the full 14-path table from the spec at startup, and
  `/openapi.json` is real OpenAPI 3.1 — a fresh user can read the contract
  from the running server alone.
- PATCH liveness contract: `status:"online"` never lies about liveness
  because it never claims it — mesh/peers is 0 for a never-connected agent
  while the registry says online, and only a signed PATCH advances
  `last_seen`. The README's long explanation of this is accurate; trust it.
