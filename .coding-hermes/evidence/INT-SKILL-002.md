# INT-SKILL-002 — board-only close: tier-2 skipped on a zero-source diff

- **Task:** `INT-SKILL-002` — [hygiene:P2] coding-hermes-worker SKILL.md is 100,722 chars — over the 100K skill limit, so no patch op applies and worker lessons can only land in references/ (same class as INT-SKILL-001)
- **Tick / date:** tick `372` — `2026-09-20` (UTC)
- **Merge base used:** `e03a836ba2be53ba01056d58aed9aaa11eae436b` (pre-tick HEAD; parent of the close commit)
- **Close command run:** `gitreins task complete int-skill-002-skill-split --skip-tier2`
- **Classification:** `verdict=board-only` — zero source-bearing paths

## 0. The deliverable (why this close is off-repo)

The task's deliverable lives in the Hermes skill tree: the skill
`coding-hermes-worker`. Split executed by worker session `20260920_161225_8ea10d`
(glm-5.3-flash @ zai-glm, lane verified genuine — no fallback line):

- `SKILL.md`: 104,848 → 4,869 chars (frontmatter version 1.6.0 → 1.7.0). The
  board row's 100,722 figure was stale; measured live pre-dispatch.
- 10 new topic files `references/split-*.md` hold the moved body (core rules +
  edit tools, write-tests+lessons corpus kept whole at 64,463 chars,
  build-before-commit, small-commits+shared tree, worktree mode,
  side-effects+suite caps, verify-then-report, task format,
  dispatch+failure handling, model selection+examples).
- NO CONTENT LOSS, foreman-verified independently of the worker's own report:
  the 10 split files reassemble BYTE-IDENTICALLY (`cmp` clean) to baseline lines
  21-501; kept region (frontmatter + purpose, lines 1-20) accounts for the rest
  (104,405 + 443 = 104,848 exact). Independent 8/8 baseline-line spot sampling
  also hit. New corpus totals 109,265 chars (volume sanity 1.04 vs 0.99 floor).
- No pre-existing references/ file was touched (find -newer: empty; none of the
  129 pre-existing files starts with `split-`, so no collision).
- Worker report: `/tmp/t372-intskill002-report.md`; rollback:
  `/tmp/coding-hermes-worker-pre-split-t372/SKILL.md`.
- Note: this SKILL.md is tracked in the home repo `/home/kara`, which has NO
  remote — no push target exists; the filesystem split plus this evidence
  artifact is the record.

## 1. The diff file list (verbatim)

- **Command run:**
  ```bash
  git diff --name-only --cached
  ```
- **Observed output:**
  ```text
  .coding-hermes/board/board.jsonl
.coding-hermes/board/events.jsonl
.coding-hermes/board/tasks.jsonl
.coding-hermes/evidence/INT-SKILL-001.md
.coding-hermes/evidence/INT-SKILL-002.md
.coding-hermes/waves/crier-2026-09-20-20-49-46.json
.gitreins/tasks.yaml
  ```
- **Counts:** `7` unique path(s) — `0` source-bearing, `7` non-source.

## 2. The classifier verdict line (verbatim)

- **Command run:**
  ```bash
  git diff --name-only --cached | bash scripts/lib/judge-diff-class.sh
  ```
- **Observed output (stdout):**
  ```text
  verdict=board-only
  ```
- **Observed output (stderr):**
  ```text
  judge-diff-class: classified: board-only (7 unique path(s): 0 source-bearing, 7 non-source)
  ```
- **Why each path is non-source:** the three board JSONL files → `.coding-hermes/**`;
  the two evidence artifacts → `.coding-hermes/**`; the wave manifest →
  `.coding-hermes/**`; `.gitreins/tasks.yaml` → `.gitreins/**`.

## 3. The tier-1-only verdict path (verbatim)

The close earned a Tier 1 verdict and NO Tier 2 verdict — that absence is the
point of this artifact, so name the artifact that does exist.

- **Command run:**
  ```bash
  cat .gitreins/history/2026-09-20/a6bc37f7/summary.md
  ```
- **Observed output:**
  ```text
  # Verdict: int-skill-002-skill-split

  **Task:** INT-SKILL-002: split coding-hermes-worker SKILL.md (104848c) under the 100K skill_manage limit with zero content loss
  **Evaluated:** 2026-09-20T21:47:26.642067
  **Result:** ✓ PASS
  Stage tier1: PASS
      ✓ guard: Tier 1 Guards: PASS  (test mode: full)
  Overall: PASS ✓
  ```
- **Tier 1 verdict path:** `.gitreins/history/2026-09-20/a6bc37f7/verdict.json` (saved verdict id `6df81ecc`)
- **Tier 1 result:** `PASS` (`secrets / go_build / go_vet / go_tests` — test mode: full)
- **Tier 2:** **not run** — authorized by section 2 of this artifact
  (board-only) under the DF-CRIER-278 rule.

## 4. Why the judge has nothing to grade

The close changed foreman bookkeeping only — board rows, wave manifest, evidence
artifacts, gitreins records. The substantive deliverable (the
coding-hermes-worker skill split) lives outside this repository, so a tier-2
exploration budget would buy nothing: its subject is not in the diff.

## External commits

- none (no remote exists for the home repo that tracks
  `.hermes/skills/coding-hermes-worker/SKILL.md`; no other repository changed)
