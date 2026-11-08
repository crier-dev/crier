# CRIER-GITHOOK-DISPATCH — boardctl git hooks → crier ingest → registered agent (the always-working pipeline)

Status: DRAFT v1 · 2026-11-08 · Owner ruling 2026-11-08 ("row lands from anywhere → hook →
crier queue → registered agent") · Row: **INT-015**

Series: `specs/CRIER-WEBHOOK-RSS-INPUT.md` §3 (the ingest endpoint these hooks POST to —
Part A, verbatim), `specs/CRIER-BOARDCTL-BRIDGE.md` (the inbound, converse half),
`scripts/doctor-hooks.sh` + `make install-hooks` (the managed-marker hook discipline this
extends), gitreins guards (the commit-time hook precedent).

## 1. Purpose

A board row can land from anywhere — a merged PR, a script, a human on a laptop. Today
nothing notices until a foreman tick polls the board. This spec closes the loop: **a
boardctl git hook detects rows ADDED locally or SEEN during a pull, POSTs each to crier's
webhook ingest, crier queues the task and delivers it to the registered agent.** The agent is
always working; the board is the single source of truth and the hook is just its doorbell.

**Premise verification (measured at HEAD `95fd40b0`, 2026-11-08).**

| Claim | Measured fact |
|---|---|
| No dispatch hook exists | `grep -rni 'post-receive\|post-commit'` matches only crier's OWN pre-commit board-gate machinery (`scripts/doctor-hooks.sh`, hilo warm hooks in `.git/hooks`) — nothing POSTs board rows anywhere |
| Hook discipline precedent | `scripts/doctor-hooks.sh` DF-CRIER-430/433: installed hooks are verified by MANAGED MARKER lines, not byte comparison (a disarmed hook can be byte-identical to its source); DF-CRIER-438/439/441: probes drive real `git commit`/`git merge`, `timeout(1)` required, SIGTERM-trapping hooks killed by the bound |
| Managed install precedent | `make install-hooks` copies tracked `scripts/hooks/*` into `.git/hooks` idempotently (AGENTS.md line 23) |
| Ingest receiver | `specs/CRIER-WEBHOOK-RSS-INPUT.md` §3 — `POST /ingress/webhooks/{source_id}` with per-source HMAC secret, persistent dedup ledger (R9), 202-on-accept. This spec's hook is its FIRST caller |
| boardctl owns hooks | boardctl (`/home/kara/coding-hermes-tools`, master @ `e7a17ab`) already ships `boardctl install` (fleet lint hook installer with `--hook-path --dry-run`) — the new dispatch hook extends this family |

## 2. Ownership split (binding)

- **R1** **boardctl owns the hook code**: `boardctl hook install / uninstall / doctor /
  dispatch` (new subcommand family in `coding-hermes-tools`) — detection, signing, POSTing,
  spooling. Crier NEVER reads a repo, a worktree, or a diff; it only receives signed POSTs.
- **R2** **Crier owns the queue**: once a POST is accepted (202), ordering, dedup (its
  persistent ledger, WEBHOOK-RSS-INPUT R9), delivery, retries and dead-lettering are crier's
  existing delivery path — no hook-side queue logic beyond the spool of R8.
- **R3** The two sides share exactly ONE contract: the §4 request. Neither reads the other's
  state files.

## 3. Hook surfaces

### 3.1 Install / uninstall

```
boardctl hook install --hook post-commit  [--crier-url URL --source-id boardctl --secret-file P] [--dry-run]
boardctl hook install --hook post-merge   […same…]
boardctl hook uninstall --hook post-merge
boardctl hook doctor
```

- **R4** Install appends a MANAGED BLOCK to the chosen hook script in `.git/hooks/`
  (creating a wrapper if the hook does not exist; never replacing an existing non-managed
  hook body). The block is delimited by
  `# >>> boardctl-dispatch BEGIN (hook=<name>)` / `# >>> boardctl-dispatch END` and carries
  the marker line `boardctl hook dispatch --hook <name> "$@"` — the marker the doctor
  checks (DF-CRIER-430 pattern).
- **R5** `--dry-run` prints the exact block and the target path, writes nothing.
- **R6** Uninstall removes ONLY the managed block; a hook left with an empty body is removed
  entirely; a hook with other content survives untouched. Uninstalling a hook that was never
  installed exits 1 naming it (no silent success).
- **R7** `boardctl hook doctor` reports, per hook: installed / marker present / marker
  REACHABLE (DF-CRIER-433: a marker that cannot be reached is refused) / crier URL
  reachable / secret file mode 0600. Exit 1 names every failing condition. A repo may adopt
  this into its own `scripts/doctor-hooks.sh` family later; boardctl's doctor is self-contained.

### 3.2 Detection

- **R8** `post-commit`: rows ADDED by the commit — `boardctl hook dispatch --hook post-commit`
  diffs `HEAD^..HEAD` on `.coding-hermes/board/tasks.jsonl` (via `git show`) and emits every
  row id present at HEAD and absent at `HEAD^`.
  `post-merge`: rows SEEN during the pull — diffs `ORIG_HEAD..HEAD` (absent when no merge
  happened ⇒ hook exits 0 doing nothing). Eligible row = `status == "pending"` and not a
  fixture row (`fixtures` header). Payload per row: `{id, title, priority, acceptance_criteria,
  board_dir, commit_sha, event: "row_added"}`.
- **R9** A rebase/squash that rewrites rows already dispatched is absorbed by dedup (§4 R11):
  the same row id + event re-POSTs as a duplicate and queues nothing.

### 3.3 Dispatch

- **R10** Each batch is POSTed to `<crier-url>/ingress/webhooks/<source-id>` (default source
  id `boardctl`) signed per WEBHOOK-RSS-INPUT R7: `X-Crier-Timestamp` +
  `X-Crier-Signature: sha256=HMAC(secret, timestamp + "." + body)`. Secret lives in a 0600
  file (path baked into the managed block at install time), read per dispatch, never in the
  hook body, never logged (WEBHOOK-RSS-INPUT R12 discipline).
- **R11** Idempotency: `Idempotency-Key: boardctl:<row-id>:<event>` — crier's persistent
  ingest dedup ledger (WEBHOOK-RSS-INPUT R9) makes re-fires (hook runs twice on one commit,
  post-commit AND post-merge after a pull race, cherry-pick replays) answer
  `200 {duplicate: true}` and queue NOTHING twice. The hook treats 200-duplicate and 202 as
  success.

## 4. Failure posture (decision recorded)

- **R12** **The hook must not block the commit.** These are post-commit / post-merge hooks:
  the commit already exists when they run. Every failure path below exits 0 toward git —
  a crier outage can never make a commit fail.
- **R13** Queue-and-retry, bounded, then drop-with-log. Undelivered batches spool to one
  file per attempt batch under `<board_dir>/.dispatch-spool/<timestamp>-<n>.json`
  (created 0700). Retry sources, in order: the next hook fire of EITHER hook (drains the
  spool first), or an explicit `boardctl hook flush`. After 3 failed attempts a batch is
  DELETED with one line to `<board_dir>/.dispatch-spool/dropped.log` (row id, reason,
  last status code).
- **R14** Why drop-and-log is safe: the board file itself is the durable record of the row;
  crier's queue is a convenience, not the store. `boardctl hook flush --dry-run` and the
  dropped log make any loss visible and re-drivable. The alternative (unbounded spool)
  risks a silent 4 GB dir on a long outage — refused.
- **R15** Crier's own refusal codes pass through to the spool decision: 401/409 (secret
  rotated, clock skew) are retried; 413 (batch too large — R16) is dropped immediately with
  the log line (retrying cannot fix size).
- **R16** Batch cap: at most 50 rows or 512 KiB per POST (`--batch-cap`, default 50);
  overflow spills to additional POSTs, then to the spool. Protects the ingest 413 boundary
  (WEBHOOK-RSS-INPUT R13) and keeps hook wall-time bounded.
- **R17** Total hook wall-time budget 10s (`timeout(1)` bound per DF-CRIER-439); anything
  still pending on timeout spools and exits 0.

## 5. End-to-end flow (the acceptance story)

```
1. human/script/PR lands a row:  boardctl create --id INT-020 …  (commit + push per board flow)
2. post-commit hook fires        → detects INT-020 ADDED
3. signs + POSTs                 → POST /ingress/webhooks/boardctl (HMAC, Idempotency-Key boardctl:INT-020:row_added)
4. crier verifies + dedups       → 202, message authored webhook/boardctl, tags [webhook, src:boardctl]
5. crier queues + delivers       → guard → inbox → registered agent (webhook/harness transport, existing path)
6. agent works the row           → replies/updates the board via CRIER-BOARDCTL-BRIDGE.md or its own flow
```

Re-fire safety at every step: commit the board file twice (R9), pull what a lane already
pushed (R9), crier down during 3-5 (R13 spool), crier up but the agent's transport down
(existing dead-letter path, untouched here).

## 6. Config summary

| Side | Key | Default |
|---|---|---|
| crier | `CR_INGEST_WEBHOOK_ENABLED` | false (WEBHOOK-RSS-INPUT R1) |
| crier | source `boardctl` registered via `POST /ingress/sources` | no source ⇒ every hook POST 404s (visible, fail-closed) |
| boardctl | `--crier-url`, `--source-id`, `--secret-file` at install time | none — nothing installed unless asked |
| boardctl | `--batch-cap` (R16), spool dir (R13) | 50; `<board_dir>/.dispatch-spool/` |

## 7. Testing

| Test | Expected |
|---|---|
| install post-commit in a scratch repo with a fake pre-existing hook → managed block appended, original body preserved; `--dry-run` wrote nothing | R4, R5 |
| real `git commit` adding a row → exactly ONE POST captured by a fixture crier; signed body verifies with the registered secret | R8, R10 |
| same commit's hook re-run manually → 200 duplicate, fixture crier saw ONE event total | R11 |
| commit + pull of a lane-pushed row (post-merge) → row POSTs once; pull with no new rows (ORIG_HEAD absent) → zero POSTs | R8 |
| crier fixture returns 503 ×3 → spool file exists, git commit unaffected (exit 0), 4th attempt drains spool | R12, R13 |
| 3 failed attempts → spool emptied, dropped.log carries row id + reason | R13, R14 |
| 60-row batch → 2 POSTs (50 + 10) | R16 |
| `boardctl hook doctor` after hand-deleting the marker line → exit 1 naming post-commit marker | R7 |
| uninstall → block removed, sibling hook content byte-identical; second uninstall exits 1 | R6 |

## 8. Non-goals

- No hook on pre-commit (boards are not gated by crier — the existing board gate is).
- No bidirectional sync (crier → board row status updates flow through the bridge or the
  agent's own tooling, not through this hook).
- No per-row routing rules in the hook (which agent gets which row is crier-side registration
  and delivery policy; the hook is a dumb, signed doorbell).
- No replacement of the foreman's own tick loop — this is push notification of new rows, not
  a scheduler.

## 9. Open questions for review quorum

1. **Q-1** `post-merge` only vs. also `post-receive` on bare remotes (a shared bare board
   repo would need post-receive; no bare board repo exists in the fleet today). Default:
   post-commit + post-merge now, post-receive as a named follow-up if a bare board appears.
2. **Q-2** Should the hook also fire on row STATUS changes (e.g. `pending → complete` events
   to the agent's inbox as receipts)? Default: no — `row_added` only; status events are
   foreman-tick territory and would double-notify.
3. **Q-3** Spool location inside the board dir (R13) vs. outside it (a `.dispatch-spool`
   entry could show up in board diffs). Default: inside, gitignored by boardctl at install —
   reopen if the quorum prefers XDG state dirs.
4. **Q-4** Should crier auto-register the `boardctl` source at startup when
   `CR_INGEST_WEBHOOK_ENABLED` (saves one manual step, but creates a secret nobody recorded)?
   Default: manual registration — the once-shown secret discipline (WEBHOOK-RSS-INPUT R4)
   requires a human to store it in the 0600 file anyway.

## 10. Quorum amendments (2026-11-08)

A chimera quorum adversarial review returned **RECONSIDER** for this spec, with a
fatal flaw: hooks as change-data-capture miss fast-forward pulls and rebases (no
post-commit fires), and the bounded drop-after-3 spool silently loses rows —
visibility restricted to hook stderr (swallowed by git) or local logs, silent to
operators. Seats: redteam `gpt-5.6-sol`, architect `grok-4.5`, challenger
`kimi-k2.7-code`, merge (aggregator) `qwen3.7-max` — total cost $0.33. Full
verdict: `/home/kara/.hermes/cache/spillover/chatcmpl-tool-9efc3d70c8fcf76d.txt`.
The amendments below REPLACE the v1 failure posture (§4 R13/R14) and the
idempotency rule (R11); the hook's role is demoted, crier's is promoted.

1. **Hooks are BEST-EFFORT intake only (amends R8/R12).** Hook discovery is an
   optimization, never the delivery guarantee. The DURABLE path is a crier-side
   RECONCILIATION SWEEP: on a cadence (default 10m) crier diffs its delivery
   queue against the board's row set and enqueues any row present on the board
   but absent from its queue. This catch-all makes every hook miss — fast-forward
   pulls, rebases, spool loss, crashed hooks — non-fatal: every row is
   eventually seen even if hooks never fire.
2. **Dead-letter instead of drop-with-log (replaces R13/R14).** After the bounded
   retry budget the batch is written to a PERSISTENT dead-letter store with
   operator-visible telemetry (count, last failure reason, row ids) — never
   silently dropped. **"Drop-with-log" is REMOVED from the contract.**
3. **One logical event key per row (replaces R11).** SEEN and ADDED for the same
   row MUST map to ONE logical event key: `boardctl:<row-id>:<content-hash>` —
   the event verb is no longer part of the key, so the double-queue
   (SEEN-during-pull then ADDED-by-commit) collapses. Rows whose row-id changes
   through rebase/cherry-pick dedupe on the CONTENT hash: a re-queued clone is
   absorbed, and an edit keeping the same row-id with new content is a NEW
   logical event (delivered) instead of being silently suppressed.
