# Dogfood diagnostics — 2026-10-10 — CR-CHAT wave + ephemeral install leg

## How this run was built, and what each error meant

- **Wrong request shapes were the first wall.** Sessions/messages/compile all
  use strict JSON decoding: `created_by` is an object (AuthorRef), message
  bodies want `payload`+`sender`, not `from`. The 400s name the Go struct
  field — useful if you read Go, useless if you do not. The openapi.yaml at
  docs/openapi.yaml has every schema; grep `PostSessionMessageRequest` first.
- **Group name vocabulary.** POST /groups name must be bare (`dogfood`), the
  `@team:` prefix is the ADDRESS FORM in message bodies. Naming it
  `team:dogfood` is refused (INVALID_GROUP) — the error message explains the
  addressable pattern, one of the better errors in the product.
- **The task lifecycle dead end.** POST /tasks creates and returns a task but
  `state` is null; no read route exists (GET /tasks → 405, GET
  /tasks/{id} → 404 with a did_you_mean hint that only confirms the gap);
  claim/complete on the freshly created id → TASK_NOT_FOUND. Reproduced
  3x. Filed DF-CRIER-304; do not hand-test claim without first re-verifying
  the row is fixed.
- **The dagger boot trap (P1).** CR_DAGGER_URL set + default store
  /var/lib/crier unwritable → `initialize dagger control: mkdir /var/lib/crier:
  permission denied` → process EXITS. Reproduced on control host and on a
  fresh bunker Debian. Fix is config-side (user-relative default like the
  sqlite path) or fail-soft. CR_DAGGER_STORE_DIR avoids it.
- **openssl oneshot signing.** `printf … | openssl pkeyutl -sign -rawin`
  fails ("unable to determine file size for oneshot operation") — must write
  the message to a file and pass `-in`. The working recipe is in the
  integration report.
- **Lease window invisibility.** A retrieved-but-unacked message disappears
  from retrieve responses for ~60s (lease). We initially read this as data
  loss; stats (leased_count:1, oldest_age_ms growing) was the tell. Lesson:
  check /inbox/stats before believing an "empty inbox".

## The ephemeral install leg — what actually happened

- bunker-las-03 and las-04 offline (ssh timeout), las-02 spawn failed
  transiently (slice-limits containment landing — per skill, retried on a
  sibling), bunker-mvp spawn OK (agent f29aea56, ttl 2h).
- Documented gitlab https clone FAILED on the fresh box (private repo, could
  not read Username). Per the hard rule no credentials were minted and no
  visibility changed; the substitute was a tar-of-tree over scp, recorded as
  a substitute in DF-CRIER-307, never as a clean-machine clone proof.
- go build on stock go1.22.2 auto-downloaded go1.26.6 + deps: 94s cold.
- Smoke on the fresh box: -version, boot on sqlite, /health ok, POST /sessions
  201, sqlite file 106KB. The dagger-configured boot failed there exactly as
  at home → that failure is now PROVEN fresh-machine, not environmental.
- Agent destroyed (`bunker destroy` → verified 0 in list).

## Right way summary

Boot sqlite-only, read docs/openapi.yaml before the first POST of each kind,
register agents before group sends, set CR_DAGGER_STORE_DIR whenever
CR_DAGGER_URL is set, and never trust an empty inbox without checking stats.
