# <TASK-ID> — board-only close: tier-2 skipped on a zero-source diff

<!--
Fill-in template for the BOARD-ONLY evidence artifact (docs/ops-evidence.md,
section "Board-only closes and the tier-2 skip" — DF-CRIER-278).

Use it when, and only when, the close's diff contains ZERO source files, i.e.

    git diff --name-only <merge-base>...HEAD | bash scripts/lib/judge-diff-class.sh

printed exactly `verdict=board-only`. Copy to
.coding-hermes/evidence/<TASK-ID>.md using the board row id, replace every <...>
placeholder, delete these comment lines, and commit the artifact in the SAME
commit as the close it authorizes.

If the classifier printed `verdict=source-bearing`, this template does NOT apply:
a source-bearing diff never qualifies for --skip-tier2. Do not delete the
evidence requirement to make a close easier — a wrong board-only verdict silently
drops the judge on a diff the rule meant to protect.

The two blocks marked "(verbatim)" are the evidence: paste the real output, not a
summary of it. Append-only: corrections are new dated sections. Redact every
secret as <redacted>.

The task id must match the filename. Keep the file list, the verdict line and the
tier-1 verdict path — those three ARE the authorization record.
-->

- **Task:** `<TASK-ID>` — <board row title, verbatim>
- **Tick / date:** tick `<N>` — `<YYYY-MM-DD>` (UTC)
- **Merge base used:** `<sha>` (`<how it was derived, e.g. git merge-base main HEAD>`)
- **Close command run:** `gitreins task complete <TASK-ID> --skip-tier2`
- **Classification:** `verdict=board-only` — zero source-bearing paths

## 1. The diff file list (verbatim)

- **Command run:**
  ```bash
  git diff --name-only <merge-base>...HEAD
  ```
- **Observed output:**
  ```text
  <the real path list, one path per line, pasted verbatim>
  ```
- **Counts:** `<N>` unique path(s) — `<0>` source-bearing, `<N>` non-source.

## 2. The classifier verdict line (verbatim)

- **Command run:**
  ```bash
  git diff --name-only <merge-base>...HEAD | bash scripts/lib/judge-diff-class.sh
  ```
- **Observed output (stdout):**
  ```text
  verdict=board-only
  ```
- **Observed output (stderr):**
  ```text
  judge-diff-class: classified: board-only (<N> unique path(s): 0 source-bearing, <N> non-source)
  ```
- **Why each path is non-source:** <one line per path, naming the rule it matches —
  e.g. `.coding-hermes/board/tasks.jsonl` -> `.coding-hermes/**`;
  `README.md` -> `*.md (any depth)`. A path you cannot name a rule for is not a
  board-only path; re-run the classifier rather than arguing it.>

## 3. The tier-1-only verdict path (verbatim)

The close earned a Tier 1 verdict and NO Tier 2 verdict — that absence is the
point of this artifact, so name the artifact that does exist.

- **Command run:**
  ```bash
  gitreins judge --status <job-id>   # or: ls -1 .gitreins/history/<date>/
  ```
- **Observed output:**
  ```text
  <the real output naming the run directory / job id and its status>
  ```
- **Tier 1 verdict path:** `.gitreins/history/<YYYY-MM-DD>/<job-id>/verdict.json`
- **Tier 1 result:** `<PASS>` (`<the tier-1 step list it covered: secrets / go_build /
  go_vet / go_tests>`)
- **Tier 2:** **not run** — authorized by section 2 of this artifact
  (board-only) under the DF-CRIER-278 rule.

## 4. Why the judge has nothing to grade

<One or two sentences. What the close actually changed (e.g. "the board row for
<TASK-ID>: status flipped to complete, one events.jsonl append"), and why there is
no source diff for a Tier-2 judge to evaluate. If any part of the deliverable is
source, STOP — this is the wrong template, and the close needs a real Tier 2 run.>

## External commits

- `<sha>` <subject> — `<repo>`  — or: none (no repository change off-repo)
