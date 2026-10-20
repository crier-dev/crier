# 2026-10-20 — crier real-use: the board that lost 690 rows, and the gate that let it through

**Verdict:** 🟡 PROMISING-BUT-ROUGH for the board gate as an *artifact-integrity*
surface — the product itself is SHIPPABLE (fresh install from source works end to
end, graded below), but in this window the repo's own queue lost 99.86% of its
rows and **every gate in the repo said PASS**. The owning lane filed and fixed
that hole while this tick was running; this run's contribution is the
**independent re-proof of the fix** on fresh artifacts, plus one correction to
the incident record itself (DF-CRIER-427).

- Build under test: `bin/crier v0.1.0-rc2-642-g193dc76-dirty` (local build),
  fresh-box clone `5e2ea5f`; incident blob `3b2d857`; fix `7ad8f9b`; history at
  close `5e2ea5f` (local == `origin/main`, 0 unpushed).
- Angle: the board/queue surface — the thing every foreman on this repo reads
  and writes — not a second sweep of the relay/chat/federation API.

---

## 1. What a real user does here, and what happened

A crier foreman tick does not read a web page: it reads
`.coding-hermes/board/tasks.jsonl`, picks a row, works it, closes the row,
commits. The board IS the product surface for that user. So this run used it the
way the foreman does: read the board, run the repo's own gate against it, and
follow a row from open to closed.

At 00:28:45Z this tick started. Between 00:32:02Z and 00:38:50Z the concurrent
`crier-foreman-2026-10-20-00-28-44` tick read a **691-row** board and committed
`3b2d857` ("chore(board): close README-005"), whose diff is:

```
 .coding-hermes/board/tasks.jsonl | 692 +--------------------------------------
 1 file changed, 1 insertion(+), 691 deletions(-)
```

691 rows → 1 row. 677 `complete`, **12 `pending`, 1 `in_progress`** (DF-CRIER-413…
425) went with it. `events.jsonl` did not receive them (byte-identical either
side of the commit), so nothing was archived — the rows were dropped.

Measured, not inferred:

```
$ git show 3b2d857:.coding-hermes/board/tasks.jsonl | grep -o '"id": "[^"]*"'
"id": "README-005"
$ git show 3b2d857:.coding-hermes/board/tasks.jsonl | grep -c 'BOARD-V2'
0
$ git show 3b2d857:.coding-hermes/board/tasks.jsonl | wc -lc
      1 3697
$ git show 0217916:.coding-hermes/board/tasks.jsonl | wc -lc     # one commit earlier
    691 1622768
```

The file that survived was a **re-serialization**: spaced separators (`": "`)
and a non-alphabetical key order (`id, status, priority, title, created_at,
reasoning, detail, worker_summary, updated_at, capability_tags, complexity,
foreman_note, worktree, branch, commit_hash, guard_result, ci_result,
worker_status`) — the shape a hand-built Python dict emits, not the canonical
shape. A stock `boardctl` board is the opposite on both axes (compact
separators, alphabetical keys, measured in a scratch board during this run), so
boardctl is not the writer that produced it.

The failing step is visible in the concurrent tick's own transcript: at
00:38:24Z it printed `Deleted: README-005`, then `README-005 complete P5`, and
committed at 00:38:50Z. That is the last write before the truncated blob exists.

## 2. The gate: 4 arms, and none of them looks at how MANY rows are left

`scripts/check-board.sh` is the board arm of the commit gate (installed by
`make install-hooks`, called by `.git/hooks/pre-commit` as
`check-board.sh -q --staged-index`, and run in CI as `bash scripts/check-board.sh`).
It validates the **index** (not the worktree) through `boardctl validate`, and it
refuses a missing/empty board. Nothing in it compared row counts.

This run reproduced the incident on a fresh scratch artifact — the exact blob
from `3b2d857`, in the exact form the hook uses — against the checker that was
on `main` when the wipe landed:

```
--- TRUNCATED (1 row) vs historical checker (05fd74d) ---
check-board.sh exit=0
   | RESULT: OK (7 warning(s))
   | board: validation PASS — 1 row(s) accepted (7 warning(s), not fatal)
```

A 1-row board is *valid*. It is also indistinguishable, to a validity checker,
from a 691-row board that was deleted. That is the whole defect.

## 3. The fix, re-proved independently (not re-run — discriminated)

The owning lane landed `7ad8f9b` ("fix(gate): reject board row loss — valid
survivors do not prove retention", addressing DF-CRIER-426) plus a `fetch-depth: 2`
CI checkout. This run did not take that on trust: it rebuilt eight fixtures from
git blobs and drove the fixed checker through each. Every leg below is my own
execution, on artifacts created in `/tmp`, at checker rev `7ad8f9b`.

| # | fixture | expected | observed |
|---|---|---|---|
| S1 | the wipe blob staged, hook form (`-q --staged-index`) | refuse | **exit 1**: `shrank: HEAD=691, index=1; 690 row(s) lost` + first 20 lost ids + the `BOARD_ALLOW_SHRINK=1` rerun hint |
| S1b | same fixture, **pre-fix** checker `05fd74d` | accept (the incident) | **exit 0**: `PASS — 1 row(s) accepted` |
| S2 | a same-count row edit (one status change, 691 rows) | pass | **exit 0**: `PASS — 691 row(s) accepted` |
| S3 | S1 + `BOARD_ALLOW_SHRINK=1` | allowed, loudly | **exit 0** with `shrink explicitly allowed …; required paths and row validation still apply` |
| S4 | truncation already committed, CI/worktree mode (`HEAD` vs `HEAD^1`) | refuse | **exit 1**, same counts and lost ids |
| S5 | root/first-board commit (no baseline) | pass | **exit 0**: `PASS — 691 row(s) accepted` |
| S6 | 691 fabricated rows replacing 691 real ones (same count) | not detected | **exit 0** — matches the checker's own documented scope ("not an identity/history audit") |
| S7 | `git clone --depth 1` of the real repo, CI form | fail loudly | **exit 2**: `shrink baseline HEAD^1 unavailable in shallow history; fetch at least two commits` |
| S8 | `git clone --depth 2`, CI form | pass | **exit 0**: `PASS — 692 row(s) accepted` (692 = 691 + the incident row) |

S1b and S1 are the same file and the same command on two checker revisions: the
before/after pair that makes the fix a measured change rather than a claim. S2,
S3, S5 and S8 are the false-positive controls — a gate that refuses a normal
close would be worse than the hole it closed. S7/S8 confirm the CI arm's shallow
checkout behaves as documented rather than going quietly green.

**The fix holds.** Restore fidelity was checked separately: the restored board
carries the same 691-row id set as the pre-wipe board, zero duplicate ids, and
README-005 re-closed — no row lost, no row invented.

## 4. Install leg — REAL, on a fresh ephemeral bunker agent

`bunker-mvp` was the only reachable fleet host with a live `bunkerd` (las-01/02/03/04
offline 7–18 days; `bunker-mvp` `systemctl is-active bunkerd` → `active`). Agent
`49128a65` spawned with a 1 h TTL, destroyed and verified gone at the end.

Documented path run verbatim (`INSTALL.md` §4, Path B — build from source; Path A
is documented as **failing closed today** because the release carries no assets):

```
git clone --depth 1 https://github.com/crier-dev/crier.git   -> rc 0, 3 s, HEAD 5e2ea5f
( cd crier && make build build-mcp )                          -> rc 0, 94 s
bin/crier 32,937,584 bytes; bin/crier-mcp 23,943,592 bytes
./bin/crier -version                                          -> crier v5e2ea5f-5e2ea5f3
./bin/crier -port 18931 -pidfile $HOME/crier.pid              -> /health {"status":"ok"}
./bin/crier -stop -pidfile $HOME/crier.pid                    -> "pid stopped", graceful
```

The agent image is not a bare box (it ships git/make/go/curl/wget/python3/tar/
sha256sum/xxd/openssl), so this is a fresh-*agent* install, not a fresh-*Debian*
one; `INSTALL.md` §0's "no fetch tool at all" premise was not exercised. That is
the same substitution previous runs recorded, and the tool census above is the
evidence. `xxd` being present also means the `DF-CRIER-280` class (hand-signing
recipes that assume xxd) is invisible on this host.

Time-to-first-success on the fresh box: **~100 s** (3 s clone + 94 s build + boot).

## 5. Performance — measured, and no PERF row is owed

| operation | measured |
|---|---|
| board gate on a healthy 691-row board, warm | **104.8 ms ± 6.9 ms**, n=10, `hyperfine -N` (`-q -C`) |
| same, cold/loose runs | 0.14–0.35 s wall (`/usr/bin/time`) |
| local incremental `make build` | 1.16 s wall (user 2.06 s) |
| fresh-box `make build build-mcp` (cold, incl. module download) | 94 s |
| fresh-box `git clone --depth 1` | 3 s |
| boot → `/health` ok (fresh box) | < 1 s after `-port …, -pidfile …` |

The shrink check adds a git blob read plus one Python pass over ≤691 records to a
command that already ran `boardctl validate`: the whole gate is ~0.1 s warm, on
every commit. Nothing here makes a user wait, so **no PERF-<n> row is filed** —
the number that would justify one does not exist. The 94 s fresh-box build is Go
compilation plus module download, i.e. the work itself, not overhead.

## 6. Findings

| id | what |
|---|---|
| DF-CRIER-426 | (filed by the owning lane mid-tick) board gate validated row *validity*, never row *count* — 691→1 passed every arm. **Fixed in `7ad8f9b`; re-proved by this run (S1/S1b above).** |
| **DF-CRIER-427** | **The incident row's evidence names the wrong surviving row.** DF-CRIER-426 states the survivor was `BOARD-V2`, "the FIRST row of the board, re-serialized with alphabetically sorted keys". Byte-measured, the survivor is `README-005`, its keys are not alphabetical, and `BOARD-V2` does not occur in the blob at all. `BOARD-V2` is the first entry of the checker's *lost ids* list, which is how the misread reads: the survivor and the first casualty were confused. The fix direction on 426 is unaffected; the diagnosis of *which writer* truncates the board is. |

Recorded as scope, deliberately **not** filed as rows:

- **Same-count replacement is not detected** (S6). The checker documents this in
  its own header ("this is not an identity/history audit"), so it is an informed
  boundary, not a defect. A wipe-and-replace that keeps 691 rows still passes.
- **The truncating writer is still unidentified.** This run pinned its *shape*
  (spaced JSON, insertion-order keys — not boardctl's compact/alphabetical
  output) and its *moment* (the tick's `Deleted: README-005` step at 00:38:24Z,
  immediately before `3b2d857`), but the code path itself is not named. With
  `7ad8f9b` armed, a repeat cannot *land* — it can still leave every commit in
  the repo blocked until someone repairs the working tree, which is what happened
  here.

## 7. Verdict

- **Does it work?** Yes — fresh clone + source build + boot + health + graceful
  stop, on a clean agent, ~100 s.
- **Is it useful?** The board is the repo's working surface, and it is now the
  one surface with a real anti-loss gate; that is worth having.
- **Is it usable?** The gate's refusal message names both counts, the first 20
  lost ids, the total, and the exact escape hatch. That is a model refusal.
- **Is it trustworthy?** This is where the window hurt: for ~4 minutes the
  authoritative queue of the project was 1 row and **four gate arms agreed it was
  fine**. The gap is closed now; what remains is that the incident record itself
  misreads its own artifact (DF-CRIER-427).
- **Is it fast enough?** Yes — numbers in §5; nothing user-noticeable.

Artifacts left behind: this report, `diagnostics-2026-10-20-board-truncation.md`,
a `skills/crier-usage/SKILL.md` section on the gate and the shrink contract, and
the `.coding-hermes/dogfood-log.md` entry. No code was changed by this lane; the
finding is a board row for the owning foreman.

---

## Run 2 addendum (same tick, second angle) — the hooks as a REAL gate

Run 1 drove the checker directly. Run 2 drove **git itself**: the documented
`make install-hooks` / `make doctor-hooks` pair (the arming contract of
DF-CRIER-421) and the `pre-merge-commit` / `pre-applypatch` entrypoints
(DF-CRIER-417) — in a scratch clone of `2ada84c` (`git clone` + `make install-hooks`;
the live tree was not touched). Neither surface had ever been driven by a dogfood
run.

| leg | what was driven | result |
|---|---|---|
| D | `make doctor-hooks` with the checkout's **local** `core.hooksPath` unset | **exit 2** — `ERROR: local core.hooksPath is unset (global hooksPath is not sufficient); repair: make install-hooks`; re-armed → exit 0 |
| A | **real `git commit`** with the wipe blob staged | **refused, exit 1, no commit created** (HEAD unmoved): `shrank: HEAD=693, index=1; 692 row(s) lost` |
| E2 | real `git commit`, same-count legitimate board edit | **committed** (exit 0); `validation PASS — 693 row(s) accepted` — no false positive |
| B | real `git merge --no-ff` of a branch dropping 692 rows | **refused, exit 1, no merge commit**; `pre-merge-commit` printed its board-only scope, then the shrink refusal |
| C | real `git am` of a patch dropping 692 rows | **refused, exit 1**; `.git/rebase-apply` was created and the probe aborted it |
| G | the documented hatch: `BOARD_ALLOW_SHRINK=1 git commit …` | **committed** with `shrink explicitly allowed by BOARD_ALLOW_SHRINK=1; required paths and row validation still apply` |

The arm is therefore not a checker that happens to be wired: it stops the three
git entrypoints that can land a board, and the arming contract fails closed when
the hooks are disarmed — DF-CRIER-421's symptom, reproduced positively.

Tester notes (this run's own mistakes, kept because they bite the next repairer):

- **`git checkout -- <path>` restores from the INDEX, not HEAD.** After a refused
  commit the truncation is still staged, so that "undo" gives a one-row worktree
  and a bogus control failure. Use `git reset --hard HEAD`.
- **A leg that changes the baseline invalidates the next leg.** Leg G (the hatch)
  deliberately landed a 1-row HEAD, so leg H's "truncation without the hatch" was
  a same-count change against that HEAD and correctly committed. The real A/B pair
  is leg A (refused) vs leg E2 (committed), both at the same 693-row HEAD.
- The board now carries **mixed serialization** — 685 spaced rows from the restore
  plus later compact rows — because the restore re-serialized the file while newer
  writers emit compact JSON. Nothing shipped matches a board row by byte shape
  (the compact literals in `scripts/` are HTTP bodies; `check-board.sh` counts with
  `grep -c '"id"'`), so this is cosmetic: recorded, not filed.

Perf (Step 2b): a real commit through the full gate costs **0.7–1.0 s wall**
(board-only diff, warm; `gitreins` startup dominates the rest). No PERF row.

Result: **no new defect** — the retention arm and the arming contract hold under
real use. Recorded as row DF-CRIER-429. Separately, DF-CRIER-427 (run 1) was
consumed by the owning lane in `596825f`: DF-CRIER-426's evidence was corrected,
427 closed, 428 filed — the fix direction landed in the record it was aimed at.
