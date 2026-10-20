# Diagnostics — the crier board, its gate, and the 2026-10-20 truncation

*Explained, not dumped. If you are the agent who lands in this repo next and sees
no rows on the board, read §4 first.*

## 1. What the board is, and who writes it

`.coding-hermes/board/` is the crier repo's work queue and it is
**JSONL-canonical** — the files are the store, there is no database behind them:

| file | role |
|---|---|
| `tasks.jsonl` | one JSON object per line, one line per row id. Task rows. |
| `events.jsonl` | append-only event log (`task_created`, `task_completed`, `audit`, …) |
| `board.jsonl` | header metadata — project, namespace, `last_tick`, `ticks_total`, `cooldown_s`, last commit |
| `fixtures.jsonl` | recurring fixtures (E2E-001, NEVER-DONE) |
| `board.db` | a retired DuckDB cache. **12 KB, zero tables** (measured: `duckdb.connect(...).execute("show tables")` → `[]`). Do not read it; do not write it. |

The writer of record is **boardctl** (`~/.local/bin/boardctl`, a fleet tool, not
part of this repo). Its output shape, measured on a scratch board during this
run, is **compact separators with alphabetically sorted keys**:

```
{"attempts":0,"blocked_reason":null,"blocked_since":null,"blocks":[],
 "capability_tags":[],"ci_result":null,"commit_hash":null,…}
```

Keep that shape in mind — it is how you tell a boardctl write from a hand write,
and this run used exactly that discrimination.

### The closed-row convention

Rows are **rewritten in place** as they move (`pending` → `in_progress` →
`complete`); they are not deleted and re-added. A `tasks.jsonl` in which an id
appears twice is normal history, and a reader must take the **last** line for
that id as its current status. A row *count* that drops is therefore an anomaly
by construction — which is what §4 is about.

## 2. The gate — four arms, and what each one actually reads

`make install-hooks` copies the tracked wrapper `scripts/hooks/pre-commit` into
`.git/hooks/` (the installed hook is generated and untracked; never symlink it).
That wrapper runs, in order:

1. `gitreins guard` — Tier 1: secrets / go_build / go_lint / go_tests. On a
   board-only diff these *skip*: `Tier 1: DEGRADED PASS (skips: go_build=No Go
   files staged, …)`. A DEGRADED PASS is not evidence about a shell, Makefile or
   board change.
2. `scripts/check-gofmt.sh` on staged `.go` files.
3. `scripts/check-shell-yaml.sh` on staged shell/workflow files.
4. `scripts/check-make-docker.sh` on staged Makefile/Dockerfile files.
5. `scripts/check-board.sh -q --staged-index` — the board arm.

Two properties of the board arm matter when you are reading a green:

- **It validates the INDEX, not the working tree.** Since DF-CRIER-399/403,
  every commit carries the whole board regardless of what is staged, so an
  invalid board cannot hide outside the diff. The blobs are materialised from
  `git show :<path>` into a temp root, then handed to `boardctl validate`.
- **It refuses a *missing, deleted or unmerged* board path and an *empty*
  `tasks.jsonl`.** What it did **not** do, until `7ad8f9b`, is compare how many
  rows there were.

CI runs the same checker without `--staged-index` (so: `HEAD` vs `HEAD^1`) in the
first job, which now checks out with `fetch-depth: 2`. `make install-hooks`
installs the hook; `gitreins install` **clobbers** it (measured previously and
re-stated in AGENTS.md) — re-run `make install-hooks` after any harness install.

## 3. The incident, step by step

| time (Z) | what |
|---|---|
| 00:32:02 | the concurrent foreman tick reads the board: `rows=691 pending=13` |
| 00:33:02 | it creates `README-005`, 00:33:05 marks it `in_progress`, dispatch dies at 00:34:42 |
| 00:37:16 / 00:37:53 | `Completed: README-005 → complete`, tier-2 judge PASS on the re-run |
| 00:37:36 | the README merge commits — and the **board arm of the gate validated a 691-row board and printed PASS** |
| **00:38:24** | **`Deleted: README-005`** — the last write before the truncated blob exists |
| 00:38:50 | `3b2d857` lands: `1 file changed, 1 insertion(+), 691 deletions(-)` |
| 00:38:54 | the gate runs again on the commit after it — and PASSes a 1-row board |
| 00:42:26 | a sibling restores from `0217916`, re-applies the README-005 close, verifies 691 rows / 0 dup ids / `boardctl validate` OK |
| 00:45–00:52 | DF-CRIER-426 filed (P0) and fixed (`7ad8f9b`), CI checkout deepened to 2 |

The truncated blob was **one row, `README-005`**, re-serialized with spaced
separators and insertion-order keys (`id, status, priority, title, created_at,
reasoning, detail, worker_summary, updated_at, capability_tags, complexity,
foreman_note, worktree, branch, commit_hash, guard_result, ci_result,
worker_status`). Byte check:

```
$ git show 3b2d857:.coding-hermes/board/tasks.jsonl | grep -o '"id": "[^"]*"'
"id": "README-005"
$ git show 3b2d857:.coding-hermes/board/tasks.jsonl | grep -c 'BOARD-V2'
0
```

That shape rule is why **boardctl is not the writer**: boardctl emits the
compact/alphabetical shape (§1). Something else wrote the board file in this
window, from a Python-side dict, holding only the row it was told about.

## 4. If you find a 1-row board: the repair recipe

1. **Do not commit anything else first.** The board arm compares against `HEAD`;
   a second write makes the baseline worse.
2. Find the last good blob: `git log --oneline -- .coding-hermes/board/tasks.jsonl`
   and take the commit **before** the shrink
   (`git show <rev>:.coding-hermes/board/tasks.jsonl | wc -l`).
3. Restore from that blob, then re-apply whatever legitimate update the bad
   commit intended (it usually has a real payload — here, `README-005`'s close).
4. Verify before committing: id-set equality against the good blob, `0` duplicate
   ids, and `boardctl validate -C .` → `RESULT: OK`.
5. Commit by explicit pathspec. If the deletion was intended, declare it:
   `BOARD_ALLOW_SHRINK=1 git commit …` (the escape hatch is read by the checker,
   and it never bypasses required paths, the empty-board refusal or row validation).

## 5. Errors hit, and the right way

- **`/stats` is not a route.** Probing `/stats` on a fresh boot returns 404; the
  documented surface is `GET /agents/{id}/inbox/stats` (README "API" table,
  `docs/openapi.yaml`). Read the spec's path list before curling a guessed path.
- **A naive `POST /agents` returns 400** — the register body is not
  `{"id": …, "name": …}`; read the `public_key`/`created_by` shape in the
  openapi spec first. Both of these were this run's own mistakes, not defects.
- **`bunker exec` is not the way to run a build.** Use direct ssh with the
  agent's key (`ssh -i ~/.config/bunker/keys/<id> bunker-<id>@<ip>`) for
  anything long; `bunker exec` has stream deadlines that kill installs mid-run.
  Stage scripts under `$HOME` on the agent (`/tmp` is not always writable) by
  piping them in: `cat leg.sh | ssh … 'cat > $HOME/leg.sh && bash $HOME/leg.sh'`.
- **The `bunker spawn` output prints an agent API key.** Redact it before
  anything reaches a repo file or a report.
- **`make build` auto-downloads the Go toolchain.** The agent's stock go1.22.2
  built the tree (`INSTALL.md` §1 asks for 1.26.9+); the build took 94 s and
  exited 0. Do not pre-install a toolchain to "help" — the auto-download is the
  behaviour a fresh user gets.
- **`-version` on a clone without tags reads `crier v<sha>-<sha>`**, not
  `v0.1.0-rc2-…`: the rc2 tag is not fetched by `--depth 1` of the default
  branch. That is expected, not a build stamp failure.

## 6. Why the shrink check exists in the shape it does

The checker now refuses a commit whose board carries fewer nonblank records than
its baseline (`HEAD` for hooks, `HEAD^1` for CI, first parent of a merge). Three
deliberate boundaries, each measured during this dogfood run:

- **A same-count replacement passes** (691 fabricated rows for 691 real ones →
  exit 0). The check is a retention check, not an identity audit; the alternative
  is a row-hash history the board does not keep.
- **A root/first-board commit passes** (no baseline to compare).
- **A shallow checkout fails closed** — `exit 2`, naming `git fetch --deepen=1` —
  rather than being treated as "no baseline". That is why CI's checkout had to
  take `fetch-depth: 2` in the same commit.
