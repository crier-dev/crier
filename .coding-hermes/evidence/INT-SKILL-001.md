# INT-SKILL-001 — board-only close: tier-2 skipped on a zero-source diff

- **Task:** `INT-SKILL-001` — [hygiene:P2] crier-foreman-ops SKILL.md is 102,276 chars — over the 100K skill limit, so no patch op applies to the live project skill
- **Tick / date:** tick `372` — `2026-09-20` (UTC)
- **Merge base used:** `e03a836ba2be53ba01056d58aed9aaa11eae436b` (pre-tick HEAD; parent of the close commit)
- **Close command run:** `gitreins task complete int-skill-001-skill-split --skip-tier2`
- **Classification:** `verdict=board-only` — zero source-bearing paths

## 0. The deliverable (why this close is off-repo)

The task's deliverable lives in the Hermes skill tree, not this repository: the
curator-managed skill `coding-hermes/crier-foreman-ops`. Split executed by worker
session `20260920_162504_224f7b` (glm-5.3-flash @ zai-glm, lane verified genuine —
no fallback line; attempt 1 `20260920_161208_ebae22` died on a transient
`Network connection lost` API error after writing nothing):

- `SKILL.md`: 100,887 → 5,178 chars (frontmatter version 1.0.0 → 1.1.0).
- 8 new topic files `references/split-*.md` hold the moved body (mesh+live-wire,
  repo/remotes/CI, board+closeout, productive ticks, E2E+battery,
  scheduler/cron/DuckBrain, webhook waves+federation, dogfood/docs-debt/tail).
- Nothing deleted: foreman's independent 11/11 baseline-line spot sampling hit;
  new corpus totals 108,657 chars ≥ 0.99×baseline; `diff -rq` vs the pre-split
  snapshot `/tmp/crier-foreman-ops-pre-split-t372` shows only the new split files.
- The blocked correction SHIPPED with the split: the false "RESPONSE `body` must
  be JSON-encoded string (RawMessage quirk)" sharp-edge is gone; the moved text
  now states the tick-332 measured truth (json.RawMessage, relayed verbatim,
  responder picks the type; MCP bridge string-body→null) with source refs.
- THE ACCEPTANCE CRITERION "a patch op applies successfully afterwards" was
  proven LIVE by the foreman post-split: a one-sentence skill_manage patch to the
  skill APPLIED (it had been refused at 100,177/100,887 chars on ticks 293-332).
- Worker report: `/tmp/t372-intskill001-report.md`; rollback: the /tmp snapshot.

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
  cat .gitreins/history/2026-09-20/fc97e115/summary.md
  ```
- **Observed output:**
  ```text
  # Verdict: int-skill-001-skill-split

  **Task:** INT-SKILL-001: split crier-foreman-ops SKILL.md (100887c) under the 100K skill_manage limit with zero content loss and the false mesh RESPONSE-body claim corrected
  **Evaluated:** 2026-09-20T21:47:20.285208
  **Result:** ✓ PASS
  Stage tier1: PASS
      ✓ guard: Tier 1 Guards: PASS  (test mode: full)
  Overall: PASS ✓
  ```
- **Tier 1 verdict path:** `.gitreins/history/2026-09-20/fc97e115/verdict.json` (saved verdict id `716abfb6`)
- **Tier 1 result:** `PASS` (`secrets / go_build / go_vet / go_tests` — test mode: full)
- **Tier 2:** **not run** — authorized by section 2 of this artifact
  (board-only) under the DF-CRIER-278 rule.

## 4. Why the judge has nothing to grade

The close changed foreman bookkeeping only — two board rows flipped to complete,
one wave manifest, one evidence artifact per task, and the gitreins task records.
The substantive deliverable (the crier-foreman-ops skill split) lives outside any
git repository the tier-2 judge could explore, so a tier-2 exploration budget
would buy nothing: its subject is not in the diff.

## External commits

- none (no repository change off-repo; the skill tree is not git-tracked at the
  edited paths, and the home repo /home/kara has no remote)
