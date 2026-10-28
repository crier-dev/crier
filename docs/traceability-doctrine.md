# Repository traceability doctrine

Every claim this repository makes about its own work — a board row closed, a route
shipped, a gate run, a deployment done elsewhere — is expected to name four things:
the row it serves, the artifact that shows it, the surface a reader can re-run to
see it, and the commit that carries it. This file is the contract for that record.

It is the authoritative, repository-local definition of:

- the `ch:trace` marker and every field it carries ([§2](#2-the-chtrace-marker));
- the evidence and witness rules, including what `none:<reason>` means and what it
  does not excuse ([§3](#3-evidence-and-witness-rules));
- how a board row binds to its commit hash **and** to a GitReins verdict artifact
  ([§4](#4-row--commit-binding), [§5](#5-verdict-binding));
- the `STATUS: / TIER: / EVIDENCE:` close-out block and the tier ladder it names
  ([§6](#6-the-close-out-contract)).

It is not a style guide and not a template gallery. A claim that cannot satisfy
§2–§6 is not closed — it stays pending with the missing link named in the row.

Companion documents, each owning a different slice:

| Document | Owns |
| --- | --- |
| [`docs/ops-evidence.md`](ops-evidence.md) | off-repo evidence artifacts, and the board-only close that skips Tier 2 (DF-CRIER-278) |
| [`docs/acceptance-matrix.md`](acceptance-matrix.md) | the built-artifact acceptance cells a release ships |
| [`docs/claims.yaml`](claims.yaml) | the prose claims `make docs-check` executes against a live server (CR-GAP-055) |
| [`AGENTS.md`](../AGENTS.md) | the commit gate's scope, and what a green does not cover |

## 1. Scope

The contract binds **every** record this repository keeps about its own work:

| Record | Path | What it holds |
| --- | --- | --- |
| board rows | `.coding-hermes/board/tasks.jsonl` | the claim: what work exists, its state, its commit, its evidence run |
| board events | `.coding-hermes/board/events.jsonl` | the history: what a tick dispatched, landed, judged |
| wave manifests | `.coding-hermes/waves/<tick-id>.json` | which dispatch produced which worktree, sha, judge and guard line |
| committed evidence | `.coding-hermes/evidence/<TASK-ID>.md` | the artifact a close commits, when the close is otherwise board-only |
| verdicts | `.gitreins/history/<date>/<shortid>/verdict.json` | what an independent judge ruled, per stage |
| commit messages and docs | `reports/*.md`, `docs/*.md` | the marker and the close-out block, where a reader will look first |

Out of scope: `.coding-hermes/board/board.db` and `.coding-hermes/board/*.parquet`
are rebuildable local caches, never the record; `bin/`, `dist/` and `.vfs/` are
build output. A traceability claim is never satisfied by a cache.

## 2. The `ch:trace` marker

One line, one claim. The marker is a single-line, greppable, space-separated
record. Every field is `key=value`, values are single tokens (no spaces, no
quoting), and absences are written out — never omitted, never left blank.

```text
ch:trace row=<ID[,ID…]> spec=<path#anchor|none:reason> wave=<tick-id[#step]|none:reason> \
         test=<path::test|make:target|command|none:reason> doc=<path[,path…]|none:reason> \
         prompt=<path|none:reason> evidence=<path|pointer|none:reason> \
         witness=<kind:locator|none:reason> verdict=<id|none:reason> \
         commit=<sha|none:reason> memory=<key|none:reason>
```

### Fields

| Field | One-line meaning | Example value |
| --- | --- | --- |
| `row=` | the board row this claim serves — **required, never `none`** | `row=DOC-5` |
| `spec=` | the spec or doc anchor that promised this behaviour | `spec=docs/openapi.yaml#/relay/publish` |
| `wave=` | the dispatch that produced the work, as `<tick-id>#<step>` | `wave=crier-foreman-2026-10-28-02-05-39#DOC-5` |
| `test=` | the test, make target or command that exercises the claim | `test=make:docs-check` |
| `doc=` | the documentation surface that now states the behaviour | `doc=docs/traceability-doctrine.md` |
| `prompt=` | the brief or instruction file the work was dispatched from | `prompt=/tmp/crier-foreman-2026-10-28-02-05-39-DOC-5-brief.txt` |
| `evidence=` | the artifact that shows the claim, or a typed pointer to it | `evidence=reports/README-004-verification.md` |
| `witness=` | the surface **outside** our own output that a reader can re-run | `witness=path@origin:go.mod` |
| `verdict=` | the independent judge's verdict id, bound to `commit=` | `verdict=52692af8` |
| `commit=` | the commit whose tree carries the change | `commit=d011bcf1cb43369245579340020bae9d0c08bd63` |
| `memory=` | a DuckBrain key holding the decision or lesson, as an **addition** to a repo reference | `memory=/project/crier/status/tick-2026-10-27-22-40-54` |

`prompt=` is present in worker and foreman traces (a dispatched brief is part of
the record); it is not required for a hand-authored change. Extra keys appear in
older traces (`block=`, `count=`, `tags=`); they are tolerated and are not part of
this contract — the eleven fields above are.

### Value syntax

| Rule | Why |
| --- | --- |
| one token per field | the marker must survive a grep, a log line and a JSON string without escaping |
| multiple ids/values are comma-joined (`row=CR-1,CR-2`, `doc=a.md,b.md`) | keeps the field token-shaped while still naming every member |
| an absent value is `none:<reason>` — always with a reason | `witness=none:playwright-unavailable` is auditable; `witness=none` and `witness=` are not |
| `row=` may never be `none:*` | an unowned claim has no reader and no owner to chase it |
| `commit=none:<reason>` is allowed only with the reason stated | a row that closes without landing code must say why (owner block, verification-only tick, a row whose deliverable is a record) — and a fabricated sha is never the alternative |
| `commit=self` in a marker that rides the commit it names | a commit cannot contain its own sha; the absolute sha is bound in the row's `commit_hash` once the commit exists |

### Worked marker line — a real one in this repository

`reports/README-004-verification.md` carries the full eleven-field form:

```text
ch:trace row=README-004 spec=none:no-spec-owns-this wave=crier-foreman-2026-10-19-23-27-03#README-004 test=Makefile::docs-check doc=README.md,INSTALL.md,docs/architecture.md,CONTRIBUTING.md,examples/ws-mesh-demo/README.md prompt=/tmp/crier-foreman-2026-10-19-23-27-03-README-004-rework-brief.txt evidence=reports/README-004-verification.md witness=path@origin:go.mod verdict=none:not-judged commit=d011bcf1cb43369245579340020bae9d0c08bd63 memory=none:worker-no-memory-write
```

Read it back yourself:

```bash
grep -RIn 'ch:trace' docs .coding-hermes reports
```

## 3. Evidence and witness rules

Six rules. Each one exists because a weaker form was measured to close work that
was not done.

1. **`row=` is mandatory.** A claim with no row cannot be closed, chased, or
   re-opened by anyone but its author.
2. **Nothing closes by reference.** `evidence=` names an artifact or a witness,
   never another row, another verdict, or another report. A chain of records that
   each point at the next is not evidence.
3. **Every claim carries a witness, or an explicit `none:<reason>`.** Silence is
   never the honest answer; a stated reason is.
4. **`memory=` accompanies a repository reference; it never replaces one.** Cite
   only keys that resolve. Memory is an addition to the record, never the record.
5. **`verdict=` binds to `commit=`, and that commit is reachable from the
   published tip (`origin/main`).** A verdict on a sha that was never merged is
   not evidence about what shipped.
6. **The auditor is not the author.** An out-of-band reading — the committed tree,
   the live endpoint, the pushed tag — outranks anything the author wrote about
   their own work. Re-read; do not remember.

### Evidence pointers

`evidence=` accepts exactly these shapes:

| Shape | Meaning | Example |
| --- | --- | --- |
| repository-relative path | a tracked artifact | `evidence=reports/README-004-verification.md` |
| `verdict:<id>` | a verdict directory under `.gitreins/history/` | `evidence=verdict:52692af8` |
| a commit sha | the change itself, when that is the artifact | `evidence=commit:7111f9c` |
| `run:<id>` | an external run/battery record, resolved by id | `evidence=run:e2e-194` |
| `live:<locator>` | a live surface's recorded output | `evidence=live:localhost:8767/health` |
| `github:<locator>` | a CI run, review or release on the forge | `evidence=github:run/40596183360` |
| `none:<reason>` | nothing exists, and this is why | `evidence=none:owner-declined-the-share` |

A bare external label (`evidence=e2e run`) is ambiguous and does not satisfy the
field: name the artifact or type the pointer.

### Witness kinds

A witness is a surface **we cannot write** — the point of naming it is that
somebody else can re-run it and get the same answer.

| Kind | Shape | Proves |
| --- | --- | --- |
| `tag:` | `tag:v0.2.0@origin` | the published tag exists on the remote |
| `release:` | `release:v0.2.0` | a release object names this artifact |
| `http:` | `http:200+sha256:<digest>` | a served response, at a named digest |
| `path@origin:` | `path@origin:go.mod` | the file resolves in the published tree |
| `ledger:` | `ledger:<predicate>` | an append-only run log satisfies a predicate |
| `provider:` | `provider:<usage-page>` | a provider console shows the usage claimed |
| `live:` | `live:localhost:8767/health` | a running process answers |
| `none:` | `none:<reason>` | nothing outside our output can be cited |

Prefer a witness over prose whenever the claim is about the world rather than
about our own output. `docs/ops-evidence.md` is the repository's extended rule for
the case where the deliverable never lands in this repository at all.

## 4. Row → commit binding

A board row is the claim; the commit is what makes it checkable. Both halves must
name each other.

### In the row

`.coding-hermes/board/tasks.jsonl` carries the binding fields:

| Field | Holds |
| --- | --- |
| `id` | the row id that `ch:trace row=` names |
| `status` | `pending` until the close, then the canonical `complete` (a handful of legacy rows read `done`; `boardctl validate` reports off-vocabulary values) |
| `commit_hash` | the sha whose tree carries the change |
| `guard_result` | `PASS` / `FAIL` / `SKIP` from `gitreins guard` on that commit |
| `worker_summary` | the worker's `ch:trace` marker and `STATUS: / TIER: / EVIDENCE:` block, where a row records the close |
| `foreman_note` | the foreman's marker, or the reason the row could not close |
| `acceptance_criteria` / `acceptance` | what a reader re-runs to reproduce the gap and its fix |
| `detail.evidence[]` | the verdict run id(s) the completion is bound to — one `{run_id, ts}` per binding, each resolving to `.gitreins/history/<date>/<run_id>/verdict.json` |

A completion is written through `boardctl`, not by hand:

```bash
boardctl -C . update DOC-5 --status complete \
  --commit-hash <sha> --guard PASS --ci GREEN \
  --evidence-run-id <verdict-id> --summary "ch:trace row=DOC-5 … STATUS: PASS TIER: T3 EVIDENCE: …"
```

`--status complete` **requires** `--evidence-run-id` (TR-251): a completion with
no bound run is refused, which is the row-level form of "nothing closes by
reference, and nothing closes on a claim alone". Measured, exit 1:

```text
boardctl: evidence run required: completion REFUSED for "<ID>": --evidence-run-id is REQUIRED when setting status "complete" (TR-251); re-run with --evidence-run-id <run-id> — the run id is recorded on the row's detail.evidence[] and must resolve to a stored run (e.g. .gitreins/history/<date>/<run-id>/verdict.json)
```

(one line, printed to stderr; `<ID>` names the row being completed)

The wording is the rule: the id must **resolve to a stored run**, so a completion
cannot be manufactured by typing a UUID. Validate the board after writing:

```bash
boardctl -C . validate            # the board gate
bash scripts/check-board.sh       # the commit-path wrapper; a nonzero validate stops the commit
```

### In the commit

- Subject: `<type>: <what> — <why>`; the row id appears as `Addresses <ID>` in the
  subject or body, so `git log --grep '<ID>'` finds it.
- Body: the `ch:trace` marker, and the close-out block when the commit closes the
  row.
- The commit message is **not** the binding. The binding is the committed tree.

### Verify the binding, not the message

A commit whose message claims a close, over a tree that still reads
`"status": "pending"`, is a false close — and it has happened. Read the row out of
the commit:

```bash
sha=<sha>
git show "$sha":.coding-hermes/board/tasks.jsonl | grep -h '"DOC-5"' | jq -r '.status, .commit_hash'
git show "$sha":docs/traceability-doctrine.md >/dev/null && echo "file in tree"
git merge-base --is-ancestor "$sha" origin/main && echo "reachable from published tip"
```

Three checks, three different facts: the row state **at** that commit, the artifact
**in** that commit's tree, and the commit's reachability from what shipped. A
`git status`-clean working tree proves none of them.

## 5. Verdict binding

The independent judgement of a row is a GitReins verdict artifact:

```text
.gitreins/history/<date>/<shortid>/verdict.json
```

`<date>` is the run date (`YYYY-MM-DD`); `<shortid>` is the verdict's **directory
handle**, which is derived from the run and is not necessarily the id the CLI
printed as `Verdict saved: <id>`. Read the directory's own `task_id` back before
binding it to a row — do not transcribe a printed id into `detail.evidence[].run_id`.

```bash
# the one verdict committed under .gitreins/history/ in this repository
jq -r '"task=\(.task_id) passed=\(.passed) stages=\(.stages|keys|join(",")) at=\(.evaluated_at)"' \
  .gitreins/history/2026-08-17/52692af8/verdict.json
# task=CR-GAP-034 passed=true stages=tier1,tier2 at=2026-08-17T22:55:49.376298
```

The artifact's shape is `task_id`, `task_title`, `task_criteria`, `passed`,
`stages` (each stage with `passed`, `any_failed`, `summary`, `steps`), `summary`,
`evaluated_at`; a sibling `summary.md` carries the human-readable form.

**`.gitreins/history/` is gitignored in this repository** (`.gitignore` line 58), so a
verdict is *local* evidence: the two files under
`.gitreins/history/2026-08-17/52692af8/` are tracked because they were force-added,
and every later verdict is written but not committed. That is why the durable
binding is threefold and all three are required:

1. `verdict=` in the marker and `detail.evidence[].run_id` in the row name the id;
2. `commit=` in the marker names the sha the verdict graded, and that sha is
   reachable from `origin/main`;
3. a committed artifact — the row's `worker_summary`, or
   `.coding-hermes/evidence/<TASK-ID>.md` for a board-only close — carries the
   verdict path and the gate output, so a fresh clone can still read the record.

A verdict graded on a pre-merge sha proves something about a branch, not about
what shipped. Re-judge on the merged sha, or state plainly that the verdict
predates the merge.

`verdict=none:<reason>` is legitimate — no independent judge ran, and the marker
says so (`verdict=none:pending-tier2`, `verdict=none:owner-blocked`). A fabricated
verdict id is not: every id is greppable back to a directory, so an invented one
fails the first read-back a reviewer performs.

```bash
ls -d .gitreins/history/*/<verdict-id>/verdict.json   # resolves, or the claim is false
```

### Board-only closes

When the diff contains zero source files, `gitreins task complete --skip-tier2` is
allowed **only** after

```bash
git diff --name-only <merge-base>...HEAD | bash scripts/lib/judge-diff-class.sh
```

prints `verdict=board-only`, and only with a `.coding-hermes/evidence/<TASK-ID>.md`
artifact riding the same commit. The full rule, the template
(`.coding-hermes/evidence/SKIP-TEMPLATE.md`) and its limits are in
[`docs/ops-evidence.md`](ops-evidence.md) §"Board-only closes and the tier-2 skip".

## 6. The close-out contract

A close states three lines, in this order, in the same record as the marker:

```text
STATUS: PASS | PARTIAL | FAIL | PENDING
TIER: T<n> — what was actually invoked and observed
EVIDENCE: <the measured result, with its command or artifact and its exit code>
```

- `STATUS` is the outcome: `PASS` (the claim holds), `PARTIAL` (a named surface
  stayed unverified), `FAIL` (the work does not hold), `PENDING` (blocked, with the
  blocker named in `EVIDENCE`).
- `TIER` is the strongest operation actually completed — see the ladder below.
- `EVIDENCE` names the measurement, not the intention: the command and its output
  or exit code, or the artifact path that holds them.

### The tier ladder

| Tier | Name | What it means | Example |
| --- | --- | --- | --- |
| **T0** | Filed | the claim exists; nothing was read or run | a row was created from a report |
| **T1** | Read | a static reading of source, docs or board that names exact paths and lines | `grep -n 'ch:trace' docs/*.md` |
| **T2** | Executed | a build, vet, unit test, script or gate actually ran, and its exit code and output were read | `go build ./... && go vet ./...` → exit 0 |
| **T3** | Invoked and observed | the real artifact was invoked — binary, route, process, or published file — and the observation was read back | server booted, `GET /health` answered 200 with the expected body |

**T3 is the minimum for "done". T0–T2 never close a row.** A passing build is T2: it
proves the code compiles, not that the behaviour exists. A doc claim is T3 when the
documented command was run and its output matched what the document prints.

Some lanes record a further rung, **T4** — a live artifact touched and read back
(a running process, a database, a mount, a published release asset). T4 is an
upper rung where a lane uses it; T3 remains the floor for every close.

### Where the block goes

The block must be in the **committed** record, not only in a chat report:

- the row's `worker_summary` (written via `boardctl update --summary`), or
- a committed artifact under `.coding-hermes/evidence/<TASK-ID>.md`, or
- the commit message body of the closing commit,

and a reader must be able to `grep` it out of the tree:

```bash
git grep -n 'ch:trace row=DOC-5' HEAD
git grep -n 'TIER: T' HEAD -- .coding-hermes/board
```

A worker's final chat message is not repository evidence. The judge grades the
checkout, so a marker that exists only in the transcript is a failed criterion —
commit it before the row is judged, not after.

## 7. Worked example — a docs close, end to end

`DOC-5` adds this file. The same claim has two stages, and the difference between
them is the point of the contract.

**At authoring time** — the worker's report, before the commit exists and before
any judge has run:

```text
ch:trace row=DOC-5 spec=none:no-spec-owns-this wave=crier-foreman-2026-10-28-02-05-39#DOC-5 test=make:docs-check doc=docs/traceability-doctrine.md,README.md,CONTRIBUTING.md prompt=/tmp/crier-foreman-2026-10-28-02-05-39-DOC-5-brief.txt evidence=docs/traceability-doctrine.md witness=none:file-not-yet-at-origin verdict=none:pending-tier2 commit=<sha-of-this-commit> memory=none:no-decision-recorded
STATUS: PASS
TIER: T3 — the acceptance commands were run in the worktree and their output read back
EVIDENCE: test -s docs/traceability-doctrine.md → exit 0; make docs-check → exit 0; go build ./... → exit 0; go vet ./... → exit 0 (the commit body carries the verbatim output)
```

**At close** — the row's `worker_summary`, once the commit reached `origin/main`
and the judge returned:

```text
ch:trace row=DOC-5 spec=none:no-spec-owns-this wave=crier-foreman-2026-10-28-02-05-39#DOC-5 test=make:docs-check doc=docs/traceability-doctrine.md,README.md,CONTRIBUTING.md prompt=/tmp/crier-foreman-2026-10-28-02-05-39-DOC-5-brief.txt evidence=docs/traceability-doctrine.md witness=path@origin:docs/traceability-doctrine.md verdict=<judge-verdict-shortid> commit=<merged-sha> memory=none:no-decision-recorded
```

Two of those fields only became writable later: the witness resolved when the file
appeared in the published tree, and the verdict existed only after the judge ran.
Writing either one early would have been invention, which is the one thing the
marker may never do. Where a marker rides the very commit it reports on, it says
`commit=self` (a commit cannot name its own sha); otherwise the placeholders
`<sha-of-this-commit>` and `<judge-verdict-shortid>` are the only ones this contract
allows, and both are replaced by a real id in the same repository, never by a
plausible-looking one.

Read the closed line field by field: the target is a single row (`DOC-5`); no spec
owns the behavior (`none:` with a reason, not an empty field); the tick and step
that dispatched it are named; the gate that fails if the doc drifts is
`make docs-check`; three documentation surfaces moved; the evidence is the artifact
itself; the witness is the file's presence in the published tree; the verdict is an
independent reader's ruling bound to the same sha; the commit is the binding — the
row reads that sha in `commit_hash`; nothing was written to long-term memory.

## 8. What this doctrine does not cover

- **Prose drift in `README.md` / `TESTERS.md` and the claim docs.** That is
  `make docs-check`, which executes `docs/claims.yaml` against a live server. A
  trace marker records that a doc changed; it cannot prove the doc is true.
- **CI status.** A green CI run is a witness about the pipeline, never about the
  acceptance criteria of a row. `witness=github:run/<id>` may be *part* of a
  record; it never closes one.
- **The tooling's internals.** How `gitreins` derives a verdict directory name,
  what `boardctl` stores in its DuckDB cache, and which paths `.vfs/` holds are
  outside this contract; the contract is what a reader can re-run from the tree.
- **Other repositories.** The marker's field names are shared across the
  organisation's repositories; the paths, tiers-in-use and gates here are this
  repository's.
- **The gate's own reach.** Markdown and prose edits are not read by Tier 1
  (secrets / build / vet / tests) — see `AGENTS.md` for the measured scope of the
  commit gate. This doctrine is a contract a reviewer and a judge enforce, not a
  check the pre-commit hook performs.

## 9. Conformance

A change that touches this contract, or closes a row under it, should leave these
green:

```bash
test -s docs/traceability-doctrine.md            # the doctrine is present and non-empty
grep -n 'traceability-doctrine' CONTRIBUTING.md README.md   # it is discoverable
grep -c 'ch:trace' docs/traceability-doctrine.md # the marker is defined here
grep -RIn 'ch:trace' docs .coding-hermes        # the marker sweep across the record
make docs-check                                  # the prose claims still execute
go build ./... && go vet ./...                   # the tree still builds and vets
```

The marker sweep is expected to find markers in two places: this file, which
defines the format, and `.coding-hermes/board/` rows, which carry the close — the
record lives in board rows rather than in prose, which is where it belongs.
