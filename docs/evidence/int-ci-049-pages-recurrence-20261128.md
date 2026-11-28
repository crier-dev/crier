# INT-CI-049 — Pages deploy recurrence at run 46127605174: attribution and green-chain verification

Tick: `crier-foreman-2026-11-28-12-00-24`
Measured: 2026-11-28T12:10Z–12:15Z (UTC), repo tip `5d088585` on `main`, working tree clean.
Mode: **verification-only** (no repository code change; no worker dispatched).

## 1. The failing run, attributed to a step and a cause

```
gh run view 46127605174 --repo crier-dev/crier --json headSha,conclusion,status,createdAt,event
-> 458c25e1 failure completed 2026-11-28T08:49:37Z push

gh api repos/crier-dev/crier/actions/runs/46127605174/jobs
-> job "deploy" (id 140925572223) failure, steps:
   Set up job            success
   Run actions/checkout@v5 success
   Assemble site         success
   Setup Pages           success
   Upload artifact       success
   Wait for artifact to reach the index  success
   Deploy to GitHub Pages                FAILURE
   Post Run actions/checkout@v5 success
   Complete job          success

gh api repos/crier-dev/crier/check-runs/140925572223/annotations
-> Error: No artifacts named "github-pages-1" were found for this workflow run.
   Ensure artifacts are uploaded with actions/upload-artifact@v4 or later.
-> Fetching artifact metadata failed. Is githubstatus.com reporting issues with
   API requests, Pages, or Actions? Please re-run the deployment at a later time.
```

`gh run view 46127605174 --log-failed` returned no bytes (same shape the INT-CI-048
capture recorded); the job payload above is the attribution, read from the API instead.

## 2. Which mechanism — identical to INT-CI-044, and its mitigation was already live

INT-CI-044 (`complete`, fix commit `a50f6e81`) diagnosed exactly this shape:
deploy-pages reporting `Found 0 artifact(s)` ~0.1s after a SUCCESSFUL upload step —
artifact-index propagation lag — and landed a bounded "Wait for artifact to reach the
index" step between Upload and Deploy in `.github/workflows/pages.yml`.

That step is present in the tree **at the failing head**, and it PASSED in the failing run:

```
git show 458c25e1:.github/workflows/pages.yml | grep -n "Wait for artifact\|Deploy to GitHub Pages"
62:      - name: Wait for artifact to reach the index
81:      - name: Deploy to GitHub Pages
```

So the recurrence is the same mechanism, but the repo-owned mitigation was active and
did not prevent it: the annotation's own second line points at a GitHub-side inability to
fetch artifact metadata. No repository change is indicated by this evidence; the next
corrective action is operational (keep the wait step; if the recurrence rate rises, add a
bounded retry around the deploy step), not a code fix on this row.

## 3. Next exact-head Pages runs are green

```
gh run view <id> --repo crier-dev/crier --json headSha,conclusion,status,createdAt,event

46127605174  458c25e1  failure  completed  2026-11-28T08:49:37Z  push   <- the row's red
46131554832  ed5ef2b9  success  completed  2026-11-28T09:40:20Z  push
46131707184  9d664631  success  completed  2026-11-28T09:42:18Z  push
46136155856  5d088585  success  completed  2026-11-28T10:38:50Z  push
```

No `.github/workflows/**` commit exists between `458c25e1` and `ed5ef2b9`:

```
git log --oneline --since="2026-11-28" -- .github/workflows/
(empty)
```

i.e. the three green runs are the same workflow, at newer exact heads, after the red —
the recurrence arm the row's AC asks for ("if the mechanism is identical, record
recurrence and the next corrective action").

## 4. Verdict

- AC PROOF arm: **satisfied** — failing step identified ("Deploy to GitHub Pages") with
  its concrete cause quoted, plus the next exact-head Pages runs green.
- Code change: **none**, correctly — the mitigation shipped under INT-CI-044 was live and
  the residual failure is upstream artifact-metadata unavailability.
- Closed **verification-only**; the row's own recurrence record is the deliverable.
