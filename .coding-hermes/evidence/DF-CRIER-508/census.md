# DF-CRIER-508 — `created_at_source` census, validator canon decision, ci_result rewrite, red-proof

Date: 2026-11-17 · Board: crier (WORKTREE A `/home/kara/worktrees/crier-DF-CRIER-508`, branch `wt/DF-CRIER-508`, based on origin/main `269c7c6f`) · Validator: coding-hermes/boardctl (WORKTREE B `/tmp/crier-508-boardctl`, branch `wt/DF-CRIER-508-validator`, from master `2c258e0`)

## 1. What the declared canon is

The canon lives in the boardctl validator source: `internal/board/keys.go`.
A task-row key is sanctioned when it is in `RequiredTaskRowKeys`
(id/title/status/priority), in `DefaultTaskRowKeys` (the create schema,
`internal/board/write.go`), in `sanctionedExtras` (the measured extras, each
admitted by census evidence in a comment), or matches the `_note` suffix rule.
`UnknownTaskKeys()` (keys.go:252) returns every row key outside that set, and
the BT-056 check in `internal/board/validate.go:361-379` turns each into the
warning `key(s) {...} outside the declared canon`. A known alias
(`TaskKeyAliases`) still warns — it is drift with a named repair.

`created_at_source` appears NOWHERE in boardctl source; it is a per-row key
written by the crier project recording WHERE a row's `created_at` came from
when the date was derived from history rather than recorded live. On the
crier board it carries exactly one value:

    "derived: first board commit whose tasks.jsonl contained this row id (committer date)"

## 2. Fleet-wide census (rows × project families)

Method: walk `/home/kara` to depth ≤ 2 for `.coding-hermes/board/tasks.jsonl`
(1,082 boards found: 579 main-tree boards, 503 worktree/clone copies);
parse every line, count rows (lines with an `id` key; header lines and
unparseable lines excluded), and count rows carrying a `created_at_source`
key. Id family = the row id with ALL trailing `-<number>` segments stripped
(`DF-CRIER-508` → `DF-CRIER`, `RELEASE-2026-10-25-01` → `RELEASE`,
`QA-CRIER-064` → `QA-CRIER`).

### Fleet aggregates

| scope | boards | task rows | rows with key | % |
|---|---|---|---|---|
| all boards | 1,082 | 484,968 | 22,712 | 4.7% |
| main-tree boards | 579 | 251,000 | 16,316 | 6.5% |
| worktree/clone boards | 503 | 233,968 | 6,396 | 2.7% |
| **boards carrying the key** | **71** | — | — | **every one is a crier-family board (crier main tree + its worktrees/qa/releng clones)** |

Value-shape census across all carriers: **one uniform value string on
100.0%** of the 22,712 rows (the "derived: first board commit…" string
above). Zero alternate spellings, zero object/null variants.

### Crier board per id family (WORKTREE A @ 269c7c6f; identical counts in the main tree `/home/kara/crier`)

| family | rows total | rows with key | % |
|---|---|---|---|
| DF-CRIER | 506 | 261 | 51.6% |
| CR-GAP | 76 | 15 | 19.7% |
| QA-CRIER | 67 | 33 | 49.3% |
| INT-CI | 40 | 3 | 7.5% |
| CR-CHAT | 38 | 1 | 2.6% |
| RELEASE | 32 | 1 | 3.1% |
| CI | 10 | 1 | 10.0% |
| DOC | 7 | 1 | 14.3% |
| QA-CRIER-REVIEW | 7 | 2 | 28.6% |
| DEPS | 5 | 1 | 20.0% |
| CR-CHAT-028-FOLLOWUP | 1 | 1 | 100.0% |
| 62 other families | 257 | 0 | 0.0% |
| **total** | **976** | **320** | **32.8%** |

### Other project boards (required sample, main trees)

| board | rows total | rows with key | % |
|---|---|---|---|
| muster (`/home/kara/wojons/muster`) | 391 | 0 | 0.0% |
| hermes-dagger (`/home/kara/Hermes-DAGger/hermes-dagger`) | 1,549 | 0 | 0.0% |
| coding-hermes-scheduler (`/home/kara/coding-hermes-scheduler/coding-herms-scheduler`) | 942 | 0 | 0.0% |
| h3/shim (`/home/kara/get-h3/shim`) | 103 | 0 | 0.0% |
| boardctl (`/home/kara/coding-hermes-boardctl`) | 278 | 0 | 0.0% |
| warpfs (`/home/kara/warpfs`) | 480 | 0 | 0.0% |

### VERDICT: CONVENTION — sanction it (criterion-2 branch **a**)

Justification:

1. **Consistent within the one project that owns it**: 22,712 rows across 71
   boards (the crier main tree and every one of its worktree/qa/releng
   clones) spanning 11 crier id families, with a 100.0%-uniform value
   grammar. Drift looks like the canon's own example
   (`probe_2026_10_31_0144`: 4 ad-hoc rows, one project, assorted values);
   this is the opposite — machine-uniform at fleet scale.
2. **Written AND enforced by committed project tooling**: crier's
   `scripts/board-created-at-census.py` (DF-CRIER-451) REQUIRES a non-empty
   `created_at_source` on any row whose `created_at` is null ("a null must
   carry a reason") and exits 1 otherwise. The DRIFT branch (rewriting the
   rows to drop the key) would break the project's own audit tool and delete
   real provenance.
3. **A real concept the create schema lacks**: the schema records the date,
   never its provenance. No canonical key respells it, so no alias applies.
4. **Precedent inside this canon**: `repo` and `tick` were sanctioned on the
   SCHED-GAP-1572 census of the crier board alone ("a census of one project's
   board can establish a real concept"). The one-project-family limitation is
   noted honestly: the canon's general rarity rule prefers >1 family, but
   enforcement-by-tooling plus fleet-scale uniformity is the stronger
   evidence class, and it is the same class that admitted `ch:trace`.

Sanctioned in `sanctionedExtras` (provenance/lifecycle group) in WORKTREE B,
with the census cited in the code comment, per house style.

## 3. Criterion 2a — validator change (WORKTREE B, boardctl)

Branch `wt/DF-CRIER-508-validator` off master `2c258e0`. Change:
`internal/board/keys.go` adds `"created_at_source"` to `sanctionedExtras`
with a census-justified comment; `internal/board/keys_test.go` adds two tests
(`TestCreatedAtSourceKeyIsSanctioned` — sanctions the exact key, keeps
near-misses `created_at_sources` / `createdatsource` / `source_created_at` as
drift; `TestValidateCreatedAtSourceIsNotDrift` — a fixture row carrying the
key validates with zero key drift).

Unit-level red-proof (before touching keys.go, both new tests against the
pre-fix canon):

    --- FAIL: TestCreatedAtSourceKeyIsSanctioned (0.00s)
        keys_test.go:349: key "created_at_source" should be sanctioned — ...
    --- FAIL: TestValidateCreatedAtSourceIsNotDrift (0.00s)
        keys_test.go:380: expected 1 clean row, got 0 ({Rows:1 Clean:0 Keys:map[created_at_source:1] Fixable:0})

the wiring failure reproduces the production warning class verbatim
(`created_at_source:1` in the drift map).

## 4. Red-proof at the board level (criterion 4)

Board: WORKTREE A copy of the crier board (identical to the main tree: 976
rows, 320 with the key). Command, run from each validator state:

    go run ./cmd/boardctl validate -C /home/kara/worktrees/crier-DF-CRIER-508

| state | RESULT line | warnings naming created_at_source | legacy no-id events |
|---|---|---|---|
| BEFORE (pre-fix validator, master 2c258e0) | `RESULT: OK (451 warning(s))` | **320** (310 solo `{created_at_source}` + 10 composite `{body, created_at_source}` / `{acceptance, created_at_source}`) | 21 (`event has no id field (legacy shape tolerated)`) |
| AFTER (canon extended, same board) | `RESULT: OK (141 warning(s))` | **0** | **21 — unchanged, all the same lines (e.g. 1956, 2207, 2208, 2211, 2222, 2593) — the true residue** |

451 − 320 warnings disappeared; the 10 composite rows still warn, now naming
only their remaining unsanctioned keys (this is why AFTER = 141, not 131).
The 21 legacy no-id event warnings, 5 non-numeric event-id warnings and 5
duplicate event-id warnings are untouched — they are genuine legacy-shape
residue, not created_at_source soup.

## 5. Criterion 3 — free-form ci_result → structured form

The structured shape the canon expects (`internal/board/result_schema.go`,
SCHED-GAP-1572): either a plain `{GREEN,RED,SKIP}` string (case-tolerant) or
`{"status":"pass|fail|skip|pending","details":"<prose>"}` (status
case-sensitive, details a string).

The brief's count ("2") is stale: the live validator flags **12** free-form
`ci_result` values on this board (plus 9 free-form `guard_result` values and
1 structured object with an out-of-vocabulary status — residue, see §6).
Validator grep (pre-rewrite): 12 warnings of the class
`ci_result "…" is free-form — not in the ci_result vocabulary {GREEN,RED,SKIP}`.

The 12 offending rows, quoted from `git show HEAD:.coding-hermes/board/tasks.jsonl`
(before) and the working tree (after); `status` classified from the prose
(GREEN/pass→pass, RED→fail, PENDING→pending, BLOCKED→skip = never ran),
`details` = the original prose verbatim:

- `INT-CI-012`
  - before: `"ci_result": "GREEN - CI run 39338420027 on exactly d0e55ce: conclusion success, all 6 jobs success (Build & Test 1.26.9, Whole-board validation, Coverage, Integration Tests, Release acceptance matrix, OpenAPI Spec Validator); the Agent-ecosystem offline execution binding step passed with the provisioned venv (pytest==9.1.1 installed into $RUNNER_TEMP/agent-ecosystem-battery-venv, node tests 83/83)."`
  - after:  `"ci_result": {"status": "pass", "details": "GREEN - CI run 39338420027 on exactly d0e55ce: conclusion success, all 6 jobs success (Build & Test 1.26.9, Whole-board validation, Coverage, Integration Tests, Release acceptance matrix, OpenAPI Spec Validator); the Agent-ecosystem offline execution binding step passed with the provisioned venv (pytest==9.1.1 installed into $RUNNER_TEMP/agent-ecosystem-battery-venv, node tests 83/83)."}`
- `INT-CI-013`
  - before: `"ci_result": "GREEN - same run 39338420027 on d0e55ce: 'Whole-board validation (fail closed)' job success and Build & Test success; the host-hook leak that reded the local Tier-1 gate did not reach CI (the runner has no global core.hooksPath), and the local gate is now green too (gitreins guard --full Tier 1 PASS)."`
  - after:  `"ci_result": {"status": "pass", "details": "GREEN - same run 39338420027 on d0e55ce: 'Whole-board validation (fail closed)' job success and Build & Test success; the host-hook leak that reded the local Tier-1 gate did not reach CI (the runner has no global core.hooksPath), and the local gate is now green too (gitreins guard --full Tier 1 PASS)."}`
- `DF-CRIER-428`
  - before: `"ci_result": "pass"`
  - after:  `"ci_result": {"status": "pass", "details": "pass"}`
- `DF-CRIER-466`
  - before: `"ci_result": "GREEN (merged to main 856dc28; local gates green; verdict 3c593503)"`
  - after:  `"ci_result": {"status": "pass", "details": "GREEN (merged to main 856dc28; local gates green; verdict 3c593503)"}`
- `DF-CRIER-468`
  - before: `"ci_result": "GREEN (merged 6e71013; local gates green; verdict 9fa82068 PASS)"`
  - after:  `"ci_result": {"status": "pass", "details": "GREEN (merged 6e71013; local gates green; verdict 9fa82068 PASS)"}`
- `DF-CRIER-469`
  - before: `"ci_result": "GREEN (merged to main 12604ce; local gates green; verdict 574cca34 FAIL-on-artifact disclosed)"`
  - after:  `"ci_result": {"status": "pass", "details": "GREEN (merged to main 12604ce; local gates green; verdict 574cca34 FAIL-on-artifact disclosed)"}`
- `DF-CRIER-473`
  - before: `"ci_result": "RED (CI run 40045386277 on d51d4ec; cmd/server short suite failed; local focused gates pass)"`
  - after:  `"ci_result": {"status": "fail", "details": "RED (CI run 40045386277 on d51d4ec; cmd/server short suite failed; local focused gates pass)"}`
- `DF-CRIER-474`
  - before: `"ci_result": "GREEN (GitHub run 40109528478; sibling same-SHA runs pending at recovery close)"`
  - after:  `"ci_result": {"status": "pass", "details": "GREEN (GitHub run 40109528478; sibling same-SHA runs pending at recovery close)"}`
- `QA-CRIER-REVIEW-1`
  - before: `"ci_result": "BLOCKED"`
  - after:  `"ci_result": {"status": "skip", "details": "BLOCKED"}`
- `QA-CRIER-51`
  - before: `"ci_result": "PENDING"`
  - after:  `"ci_result": {"status": "pending", "details": "PENDING"}`
- `INT-CI-017`
  - before: `"ci_result": "GREEN (exact-head CI Build & Test run 40582899662)"`
  - after:  `"ci_result": {"status": "pass", "details": "GREEN (exact-head CI Build & Test run 40582899662)"}`
- `INT-CI-036`
  - before: `"ci_result": "PENDING: post-closeout exact-head CI"`
  - after:  `"ci_result": {"status": "pending", "details": "PENDING: post-closeout exact-head CI"}`

Rewrite mechanics (committed script
`.coding-hermes/evidence/DF-CRIER-508/rewrite_ci_result.py`):

1. default mode operates on a COPY of tasks.jsonl and prints a dry-run diff
   summary; the board is written only with `--apply`;
2. it rewrites ONLY the raw bytes of the `ci_result` value on the 12 named
   rows, preserving each row's own compact/spaced JSON style and key order —
   every other line stays byte-identical;
3. a row whose stored value no longer equals the expected free-form string is
   SKIPPED loudly (this fired on the first dry-run for INT-CI-012/013, whose
   expected strings had been truncated from live output — the script refused
   until the exact 388/302-char values were quoted from the board).

Verification after apply:

- `git diff --numstat`: exactly `12 12` on `.coding-hermes/board/tasks.jsonl`,
  nothing else; line-by-line diff vs HEAD = exactly the 12 ids (lines 673,
  677, 694, 739, 741, 742, 746, 747, 763, 766, 778, 943); 977 lines before,
  977 after;
- all 976 rows still parse; 12/12 rows now carry
  `{status ∈ {pass,fail,skip,pending}, details: non-empty string}`;
- post-rewrite validate: `RESULT: OK (129 warning(s))` — the 12 free-form
  ci_result warnings are gone, `guard_result` free-form (9) untouched.

## 6. Known residue (out of this task's scope, documented for the foreman)

- 9 free-form `guard_result` warnings (same migration shape available);
- 1 `ci_result` structured object with status `"partial"` (CRIER-HARNESS-13) —
  not in `{pass,fail,skip,pending}`; needs a real decision, not a rewrite;
- 45 `acceptance` alias-drift warnings + 12 `body` + 5 `lane` + assorted
  composite key sets — the named repair is `boardctl update <id> --normalize`
  for aliases; `body`/`lane` are separate canon decisions;
- 21 legacy no-id events + 5 non-numeric event ids + 5 duplicate event ids
  (events.jsonl untouched by this task, per constraint);
- 4 timestamp-exclusion warnings (report-parser grammar).

## 7. Regression gates (WORKTREE B)

    go build ./...   → exit 0
    go vet ./...     → exit 0
    go test ./... -count=1 → exit 0, all packages ok (log: scratch/df508_boardctl_test.log)

gofmt: clean on both edited files. New tests included in the full run.
