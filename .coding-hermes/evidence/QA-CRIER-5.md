# QA-CRIER-5 — board-only close: tier-2 skipped on a zero-source diff

- **Task:** `QA-CRIER-5` — [P1] Bunker battery timed out with zero recorded coverage
- **Tick / date:** tick `371` — `2026-09-20` (UTC)
- **Merge base used:** `199b803e3cbe1cb6238339c7291a1094e88ef831` (HEAD the battery ran against; parent of the close commit)
- **Close command run:** `gitreins task complete QA-CRIER-5 --skip-tier2`
- **Classification:** `verdict=board-only` — zero source-bearing paths

## 0. The deliverable (why this close is off-repo)

The task's deliverable was a fresh full bunker battery run against crier after the
QA-CRIER-23 harness fixes (tick 364, commit `28ca788` + `go_suite_env_failure()`
in `~/.hermes/scripts/bunker-qa.sh`, outside this repo). The run executed
2026-09-20 19:48–20:04 UTC: `bunker-qa.sh launch` spawned temp agent `143a841c`
on `bunker-las-02`, synced HEAD `199b803`, ran every cell detached, and
`collect` pulled **15 evidence rows** (11 OK, 1 N/A, 1 INFO, 1 FAIL, 1
UNVERIFIED) to `/tmp/crier-t371-battery.jsonl`, destroying the temp agent. No
file in this repository changed — the proof is the evidence file and the cell
statuses; the two new harness-level findings it surfaced were filed as
QA-CRIER-30 (upgrade cell FAIL) and QA-CRIER-31 (chaos-resource cap not applied
in the child) in the same close commit.

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
.coding-hermes/evidence/QA-CRIER-5.md
.gitreins/tasks.yaml
  ```
- **Counts:** `5` unique path(s) — `0` source-bearing, `5` non-source.

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
  judge-diff-class: classified: board-only (5 unique path(s): 0 source-bearing, 5 non-source)
  ```
- **Why each path is non-source:** the three board JSONL files → `.coding-hermes/**`;
  `.gitreins/tasks.yaml` → `.gitreins/**`;
  `.coding-hermes/evidence/QA-CRIER-5.md` → `.coding-hermes/**`.
  A pre-artifact run over the first four paths printed the same verdict
  (`judge-diff-class: classified: board-only (4 unique path(s): 0 source-bearing,
  4 non-source)`).

## 3. The tier-1-only verdict path (verbatim)

The close earned a Tier 1 verdict and NO Tier 2 verdict — that absence is the
point of this artifact, so name the artifact that does exist.

- **Command run:**
  ```bash
  cat .gitreins/history/2026-09-20/32d1a6cc/summary.md
  ```
- **Observed output:**
  ```text
  # Verdict: QA-CRIER-5
  **Evaluated:** 2026-09-20T20:22:04.072590
  **Result:** PASS
  - tier1
      - guard: Tier 1 Guards: PASS  (test mode: full)
  Overall: PASS
  ```
- **Tier 1 verdict path:** `.gitreins/history/2026-09-20/32d1a6cc/verdict.json` (saved verdict id `a49c960b`)
- **Tier 1 result:** `PASS` (`secrets / go_build / go_vet / go_tests` — test mode: full)
- **Tier 2:** **not run** — authorized by section 2 of this artifact
  (board-only) under the DF-CRIER-278 rule.

## 4. Why the judge has nothing to grade

The close changes the QA-CRIER-5 board row (status → complete with the battery
tally), appends two QA finding rows and three events to the board JSONL, and
flips the `.gitreins/tasks.yaml` status — plus this artifact. Every path is
non-source; the operational proof of the task is the battery evidence described
in section 0, which a local-diff judge cannot see by construction, so the
DF-CRIER-278 rule substitutes the classifier + this artifact for a tier-2 run.

## External commits

- none (no repository change off-repo; harness state lives in
  `~/.hermes/scripts/bunker-qa.sh`, an untracked fleet-local script, and the
  battery evidence in `/tmp/crier-t371-battery.jsonl`)
