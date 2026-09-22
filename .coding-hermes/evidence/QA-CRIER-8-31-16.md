# QA-CRIER-8-31-16 — board-only close: tier-2 skipped on a zero-source diff

- **Task:** `QA-CRIER-8-31-16` — Post-harness-update battery re-run + stale-premise QA row closures (covers board rows `QA-CRIER-8`, `QA-CRIER-31`, `QA-CRIER-16`)
- **Tick / date:** tick `386` — `2026-09-22` (UTC)
- **Merge base used:** `17b4351` (HEAD the battery ran against; parent of the close commit)
- **Close command run:** `gitreins task complete QA-CRIER-8-31-16 --skip-tier2`
- **Classification:** `verdict=board-only` — zero source-bearing paths

## 0. The deliverable (why this close is off-repo)

The deliverable was a fresh full bunker-qa cell battery against crier **after**
the 2026-09-21 17:23 harness update to `~/.hermes/scripts/bunker-qa.sh`
(outside this repo; the file is untracked and fleet-shared), then closing the
QA rows whose premises that update provably resolved:

- **QA-CRIER-8** (chaos-corruption truncates a gitignored `.vfs` code-graph
  cache): the current generator's state-candidate discovery excludes
  `./.vfs/*`. The tick-386 battery cell reports `N/A "no db/state files in
  repo"` — the gitignored graph cache is never truncated.
- **QA-CRIER-31** (chaos-resource UNVERIFIED: cap not applied in the child):
  the QA-TASK-ROUTER-3 fix makes child-side ulimit evidence REQUIRED. The
  tick-386 cell is `PASS` — `cap=3145728KB child_saw=3145728
  subshell_saw=3145728 parent_saw=unlimited` — the suite ran capped and
  survived.
- **QA-CRIER-16** (bunker CLI v0.1.3 has no `--config`): superseded upstream —
  `bunker 0.1.4` (commit `00c3555`) verified live on the generation host with
  `--config <path>` and the `--config > $BUNKER_HOME/config.yaml >
  $HOME/.bunker/config.yaml` precedence string.

The run executed 2026-09-22: `bunker-qa.sh launch` spawned temp agent
`ad6c2006` on `bunker-las-02`, synced HEAD `17b4351`, ran the cells detached,
and `collect` pulled **16 evidence rows** (11 OK incl. launch/collect, 1 PASS,
2 INFO, 1 N/A, 1 FAIL) to `/tmp/bunker-qa-evidence-crier-t386.jsonl`,
destroying the temp agent. No file in this repository changed — the proof is
the evidence file and the cell statuses. The one FAIL (upgrade cell, no
installable-previous-release arm for Go repos) is the still-live premise of
**QA-CRIER-30**, which therefore stays open; QA-CRIER-13/14 premises were
additionally re-derived as stale against the current generator (capacity
preflight QA-OFF-BY-ONE-9; spawn destroyed-before-retry QA-HERMES-CANOPY-16).

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
.gitreins/tasks.yaml
  ```
- **Counts:** `4` unique path(s) — `0` source-bearing, `4` non-source.

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
  judge-diff-class: classified: board-only (4 unique path(s): 0 source-bearing, 4 non-source)
  ```
- **Why each path is non-source:** the three board JSONL files →
  `.coding-hermes/**`; `.gitreins/tasks.yaml` → `.gitreins/**`. The evidence
  artifact (this file, `.coding-hermes/evidence/QA-CRIER-8-31-16.md`) is added
  to the same commit per the closeout rule and classifies as `.coding-hermes/**`
  as well. A pre-artifact run over the first four paths printed the same
  verdict (quoted above); re-running the classifier on the final staged set
  including this artifact also prints `verdict=board-only` (5 paths, 0
  source-bearing).

## 3. The tier-1-only verdict path (verbatim)

- **Command run:**
  ```bash
  ls -la .gitreins/history/2026-09-22/ && find .gitreins/history/2026-09-22 -name verdict.json -newermt '10 minutes ago'
  ```
- **Observed output:**
  ```text
  .gitreins/history/2026-09-22/a4b8d1e8 (dir, Sep 22 06:47)
  .gitreins/history/2026-09-22/a4b8d1e8/verdict.json
  ```
- **Tier 1 verdict path:** `.gitreins/history/2026-09-22/a4b8d1e8/verdict.json`
- **Saved verdict id:** `5928ea59` (differs from the job dir `a4b8d1e8`, per the
  known closeout behavior — both cited here).
- **Tier 1 result:** `PASS` (`guard: Tier 1 Guards: PASS`, test mode: full —
  secrets / go_build / go_vet / go_tests)
- **Tier 2:** **not run** — authorized by section 2 of this artifact
  (board-only) under the DF-CRIER-278 rule.

## 4. Why the judge has nothing to grade

The close changed only board rows (three QA-CRIER rows flipped to complete
with evidence summaries), board bookkeeping (header counters, one audit
event), and `.gitreins/tasks.yaml` (the completed gitreins record). The
deliverable itself is a detached battery run on a temp bunker agent plus
harness-side fixes that live in the untracked fleet-shared
`~/.hermes/scripts/bunker-qa.sh` — no path in this repository's diff carries
source for a Tier-2 judge to evaluate.

## External commits

- none (no repository change off-repo; the harness fixes were landed by
  earlier QA cycles in the home-dotfiles scripts tree, which is untracked)
