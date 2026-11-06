# Dogfood 2026-11-06 — harness push delivery, used the way an operator would

Lane: `crier-dogfood` (scheduler tick `crier-dogfood-2026-11-06-12-41-09`).
Verdict: **PROMISING-BUT-ROUGH** — the adapter layer is real and usable from the
CLI, but the documented push-delivery path cannot be reached by a user at all:
nothing in the product can bind a session to a harness.

## Target identity

| Field | Value |
|---|---|
| Canonical checkout | `/home/kara/crier` |
| Lane stand-in | `/home/kara/.hermes/stand-in/dogfood/crier` (board symlink only) |
| Tested HEAD | `5dc81b05bc5689ae64d1c666a34daa9cb9657a12` |
| `origin/main` | same SHA (parity, `rev-list --left-right --count` = 0/0) |
| Checkout state | clean (`status --short -uno` empty) |
| Git markers | `.git/REBASE_HEAD` = `2e1ed4d3…` with **no** `rebase-merge`/`rebase-apply` directory — a stale marker, so the live tree was not timed; every probe below ran from a fresh `git clone --no-hardlinks --single-branch --branch main` at the pinned SHA (verified HEAD + clean + `README.md` present) |
| Build | `make build` → `bin/crier`, 3.4 s warm, stamped `v0.2.0-455-g5dc81b05` / commit `5dc81b05` |

## The promise that was walked

`docs/harness-adapters.md`:

> Push delivery: hand a stored Crier message to a live agent harness session —
> continuing an existing one or creating a new one — and record a receipt that
> says what actually happened. The inbox stays; this is the push half.

`specs/CRIER-HARNESS.md` §4.3 names the way a user is supposed to select the
harness:

> A `new_session` ingress with `target.harness:"pi"` creates the Crier session
> **and** records the harness id on first successful delivery.

## What actually happened

Server: scratch state directory, `-port 18911`, `CR_HARNESS_ENABLED=true`,
`CR_HARNESS_DEFAULT=pi`, `CR_HARNESS_PI_BINARY=/home/kara/.local/bin/pi`,
`CR_GUARD_ENABLED=false`. Client: the shipped `clients/python/crier_client.py`
with a real ed25519 key (`register` 4.4 ms, signed requests throughout).

| Step | Command shape | Result |
|---|---|---|
| 1 | `POST /ingress/messages` `mode=new_session`, `target.template{…}`, `target.harness:"pi"` (§4.3 verbatim) | **400 `INGRESS_INVALID_JSON`** — `target: unknown field "harness" (accepted: session_id, template, agent_id, capability)` |
| 2 | same, `target.template.harness:"pi"` (the other plausible spelling) | **400** — `target: template: unknown field "harness" (accepted: title, kind, namespace, members)` |
| 3 | same without any harness key (control) | **200**, `delivery: {"mode":"store","state":"stored"}`, `created_session: true` |
| 4 | `POST /ingress/messages` `mode=session` + `target.harness:"pi"` | **400**, same unknown-field refusal |
| 5 | `GET /sessions` vs `GET /sessions/{id}` | list 200, single-session 200-only-by-list; there is **no** `GET /sessions/{id}` route registered — my first probe assumed one, that 404 is correct behaviour and not a defect |

So with the harness registry **enabled**, an ingress push to a real, freshly
created session still reports `store`/`stored`: the dispatch hook is reached and
immediately declines, because the session names no harness.

### Why it cannot be bound

Read from the tested tree:

- `internal/ingress/request.go:49-58` — `rawTarget` has exactly four fields
  (`session_id`, `template`, `agent_id`, `capability`); `rawTemplate`
  (`:61-67`) has `title`, `kind`, `namespace`, `members`. The body is decoded
  with strict JSON, so `target.harness` is refused by construction — the
  refusal above is the designed message, not a coincidence.
- `internal/session/http.go:445-458` — `createSessionRequest` has no harness
  field either, so `POST /sessions` cannot bind one.
- `internal/registry` (non-test code) has no `Harness` symbol at all, so an
  agent cannot carry a binding at registration.
- `cmd/server/harnesswiring.go:76-80` is the only consumer: `name == ""` →
  `{"mode":"store","state":"stored"}`.
- `internal/session/store.go:551-566` (`HarnessBindRecord`) is written **only**
  by `cmd/server/harnesswiring.go:104`, i.e. only after a delivery to a session
  that *already* names a harness. The writer needs the value it is supposed to
  produce.
- `docs/openapi.yaml` documents `harness` only inside the ingress **response**
  delivery object; no request schema accepts it.

Consequence: the feature is dark in a real deployment. The only reachable push
today is the in-process adapters used by the shipped Go tests. Rows
`CRIER-HARNESS-6` and `CRIER-HARNESS-7` ("prove live delivery into a real
Hermes / opencode session, end to end") cannot be attempted by a user until a
binding surface exists.

## What *does* work (real user, same build)

`crier harness list` (1.30 s wall, five adapters probed through their
`--version`):

```
NAME         AVAILABLE  VERSION               FEATURES
claude-code  false                            continue,create,json,print,resume,stdin   (binary not found: claude)
codex        true       0.166.0               continue,create,json,resume,sandbox,stdin
hermes       true       0.22.5+3030.g770a952  cli,continue
opencode     true       1.18.29               continue,create,json
pi           true       1.5.0                 continue,create,json,print,session_id
```

`crier harness probe pi` and `probe pi-agent` both return the pi capability
JSON (rc 0) — the alias from CRIER-HARNESS-9 holds; `probe nope` exits 1 naming
the known adapters. This half of the documented surface is honest and usable.

## Documentation drift found while walking the promise

`docs/harness-adapters.md` still describes the pre-CRIER-HARNESS-2/3/11 world:

- line 8 and lines 52-54 call the `pi` and `codex` blocks "adapter
  CRIER-HARNESS-2/3" as if unbuilt; line 112 states outright that "`pi` and
  `codex` adapters (config blocks are accepted and report unavailable)" — the
  same build reports both **available** with full feature sets (above).
- lines 113-114 say the `POST /ingress/messages` route "is a later row;
  `Registry.Deliver` is the exported hook it will call" — the route is
  registered in `cmd/server/main.go` (README line 2745 counts it among the 54
  always-on paths) and answered 200 in this run; `cmd/server/harnesswiring.go`
  is the landed joint.
- the capability table (lines 19-24) lists hermes, opencode, opencode-server and
  claude-code but has no row for `pi` or `codex`, both of which ship.
- nothing in the document tells a user **how to bind** a harness to a session;
  the only statement of that is `specs/CRIER-HARNESS.md` §4.3, which the API
  refuses.

## Performance (Step 2b)

Operation that matters: an ingress push. Nothing here is slow enough for a user
to notice, so no `PERF-<n>` row was filed.

| Arm | Measurement |
|---|---|
| Build (`make build`, warm module cache) | 3.40 s |
| Cold start → `/version` 200 (fresh state dir, fresh port) | 81.3 ms |
| Cold first push (register + new_session ingress) | 27.3 ms |
| Warm push, store-only `mode=session`, n=20 | median **5.42 ms**, mean 7.34, min 5.06, max 13.35 |
| `crier harness list` (5 adapter probes) | 1.30 s wall |

## Install leg

Ephemeral bunker battery, launched through `bunker-qa.sh launch` on `bunker-mvp`
and collected to completion (`launch` OK → 24 evidence rows → agent destroyed,
`bunker info` returns `not_found`).

| Cell | Result |
|---|---|
| toolchain-bootstrap | OK — Go 1.27.3 installed over an ambient 1.22.2 (floor 1.26.10), gcc/g++, make, jq, compose+buildx, node 22.15.0 |
| **fresh-install** | **OK** — the pinned tree built from a clean sync into `~/qa-src` |
| lint | OK (`make lint`, rc=0) |
| tests | OK (`go test …`, rc=0) |
| docker_build | OK |
| compose_config | OK |
| upgrade | OK — previous release `v0.1.0-rc2` tree reinstalled clean |
| ui-probe | OK — go-native server answered |
| chaos-disconnect | OK — fails fast on a cut network |
| chaos-shutdown | OK — SIGTERM + SIGKILL recovery clean |
| chaos-errorpath | OK — clean documented failure on a broken config |
| chaos-corruption | N/A — no db/state files in the repo |
| chaos-resource | INFO — OOM under the 3 GiB cap (expected for the heavy suite) |
| ci-pass / ci-act | FAIL / UNVERIFIED — `act` cannot clone `actions/setup-go`; the known harness class already owned by QA-CRIER-48 / QA-CRIER-7 |
| docker-deploy | INFO — `compose up` OK but the probe got `000`; this is the open QA-CRIER-REVIEW-7 (the battery's port extractor cannot parse the compose `${VAR:-default}:port` mapping) — re-proved, not re-filed |

So the install half is real: a fresh machine builds, lints, tests, docker-builds
and survives the chaos ladder. The only non-green cells are the two known
harness/detector classes already on the board.

## Verdict

**PROMISING-BUT-ROUGH.** The adapter registry, probes and receipts are real
work and the CLI half is usable; the push path the document headlines is
unreachable from every public surface, and the document that explains it is one
generation stale. That combination is the worst shape for a new user: the doc
says "hand a stored message to a live harness", the API says "unknown field
harness", and the harness registry says "store / stored".
