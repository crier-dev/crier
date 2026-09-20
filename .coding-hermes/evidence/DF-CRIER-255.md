# DF-CRIER-255 — board-only close: tier-2 skipped on a zero-source diff

- **Task:** `DF-CRIER-255` — [hygiene:P2] AGENTS.md still owes the load-safety rule — the file is write-protected for tick agents, so DF-CRIER-254 landed it in docs/load-reproduction.md only
- **Tick / date:** tick `374` — `2026-09-20` (UTC)
- **Merge base used:** `9c24bd0` (parent of worker commit `c4e7da3`; the work landed as one commit directly on main, so `c4e7da3^` == merge-base — the diff under judgement is exactly the worker's change)
- **Close command run:** `gitreins task complete df-crier-255-agents-md-load-safety --skip-tier2`
- **Classification:** `verdict=board-only` — zero source-bearing paths

## 1. The diff file list (verbatim)

- **Command run:**
  ```bash
  git diff --name-only c4e7da3^..c4e7da3
  ```
- **Observed output:**
  ```text
  AGENTS.md
  ```
- **Counts:** `1` unique path(s) — `0` source-bearing, `1` non-source.

## 2. The classifier verdict line (verbatim)

- **Command run:**
  ```bash
  git diff --name-only c4e7da3^..c4e7da3 | bash scripts/lib/judge-diff-class.sh
  ```
- **Observed output (stdout):**
  ```text
  verdict=board-only
  ```
- **Observed output (stderr):**
  ```text
  judge-diff-class: classified: board-only (1 unique path(s): 0 source-bearing, 1 non-source)
  ```
- **Why each path is non-source:** `AGENTS.md` -> `*.md (any depth)`.

(Note: a first attempt with `git diff --name-only $(git merge-base main HEAD)...HEAD` produced an empty path list — merge-base equals HEAD here because the work is already the tip commit; the parent-of-worker-commit range above is the correct expression of the task's diff. The empty-diff refusal (rc=3) behaved exactly as DF-CRIER-278 designed.)

## 3. The tier-1-only verdict path (verbatim)

- **Command run:**
  ```bash
  gitreins task complete df-crier-255-agents-md-load-safety --skip-tier2
  ```
- **Observed output:**
  ```text
  Stage tier1: PASS
      ✓ guard: Tier 1 Guards: PASS  (test mode: full)
  Overall: PASS ✓
    📋 Verdict saved: be25fda5
  ```
- **Tier 1 verdict path:** `.gitreins/history/2026-09-20/b0083fcc/verdict.json`
- **Tier 1 result:** `PASS` (guard battery in full mode: secrets/gitleaks, go_build, go_lint (go vet), go_tests — the suite that green-lights the whole repo, not just the diff)
- **Tier 2:** **not run** — authorized by section 2 of this artifact
  (board-only) under the DF-CRIER-278 rule.

## 4. Why the judge has nothing to grade

The deliverable is a four-line prose section (`## Load safety`) inserted into AGENTS.md between the commit-gate section and `## Board` — pure documentation restating the DF-CRIER-254 contract (bounded harness, caps, load gate, kill-pattern rule) that `make load-repro-selftest` already proves and CI already runs. There is no source diff for a Tier-2 merit evaluator to grade beyond what the full-mode Tier-1 guard verified green.

Foreman acceptance verification (independent of the worker's self-report): `git show --stat HEAD` = AGENTS.md only (+4); section anchored at line 201 directly before `## Board`; all six required facts present (13/13 grep checks: setsid, load-repro.sh, loadgen.py, default 4 / max 8, default 30 / max 300, refused, /proc/loadavg, 8.0, exit 3, pkill/pgrep self-match, load-repro-selftest, CI); protected-file toggle read back `true` after the sanctioned toggle window.

## External commits

- none (no repository change off-repo)
