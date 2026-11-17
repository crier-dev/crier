# INT-CI-040 — Pages deploy failure at 366eb952 explained, and the re-run artifact collision removed

- **Task:** `INT-CI-040` — `[ci][P3] Pages deploy job failed once at 366eb952 with no retrievable log, passed on rerun at 463a7c04`
- **Tick / date:** tick `crier-foreman-2026-11-17-06-00-43` — `2026-11-17` (UTC; probe times as `<HH:MM:SS>Z`)
- **Operation:** Recovered both job logs of the failed Pages run (they are retrievable now), named the failure cause for
  each of the run's two attempts, and removed the structural reason the second attempt could not succeed —
  `actions/deploy-pages` resolves its artifact by name within the **run**, so a re-run of the same run found two
  `github-pages` artifacts and hard-failed. The Pages artifact name is now attempt-scoped.
- **Targets:**
  - host(s): the foreman host, `gh` CLI against the GitHub API (no host operation)
  - repo(s): `crier-dev/crier` — run `43989570695` analysed; `.github/workflows/pages.yml` changed in this commit
  - service(s): GitHub Pages site for `crier-dev/crier` (deployed from the artifact this workflow uploads)
- **External commits:** none off-repo. The deliverable is the diff in this repository plus the CI evidence below.

## Board row acceptance criteria

> "The Pages deploy failure of run 43989570695 job 133726395895 (head 366eb952) is explained or dismissed: either the
> job log is recovered and a cause named, or five consecutive Pages runs after this row are shown green, which is
> recorded as a transient and the row is closed with that evidence. Do not re-run blind twice in a row without
> capturing the failing log."

Both arms are satisfied — the log was recovered **and** the lane is green. The recovery additionally showed that the
second attempt was not a re-occurrence of a flake but a deterministic consequence of re-running the failed job.

## AC-1 — the job log is recovered and a cause is named

The failure had two attempts inside one run (`run_attempt: 2`). Each attempt has its own cause.

### AC-1a — attempt 1 (job `133726395895`, the job the row cites): artifact uploaded, then invisible to the deploy step

- **Command run:**
  ```bash
  gh api repos/crier-dev/crier/actions/jobs/133726395895/logs > /tmp/intci040-attempt1.log   # 20187 bytes, exit 0
  grep -nE 'Found [0-9]+ artifact|Fetching artifact metadata|successfully uploaded|##\[error\]' /tmp/intci040-attempt1.log
  ```
- **Observed output:**
  ```text
  228:2026-11-17T00:41:05.5684981Z Artifact github-pages has been successfully uploaded! Final size is 3382427 bytes. Artifact ID is 14287730363
  244:2026-11-17T00:41:05.8890122Z Fetching artifact metadata for "github-pages" in this workflow run
  245:2026-11-17T00:41:06.0668179Z Found 0 artifact(s)
  246:2026-11-17T00:41:06.0683883Z ##[error]Fetching artifact metadata failed. Is githubstatus.com reporting issues with API requests, Pages, or Actions? Please re-run the deployment at a later time.
  247:2026-11-17T00:41:06.1718288Z ##[error]Error: No artifacts named "github-pages" were found for this workflow run. Ensure artifacts are uploaded with actions/upload-artifact@v4 or later.
  ```
- **Before:** the row recorded `gh run view --log-failed` as empty and the job-logs API as "nothing usable".
- **After:** the log is retrievable. The upload step succeeded at `00:41:05.568`; 0.5s later `deploy-pages@v5` listed the
  run's artifacts and got `Found 0 artifact(s)`, i.e. the artifacts API had not yet exposed the artifact the upload
  step had just finalised. **Cause: a read-after-write consistency race between `upload-pages-artifact` and the artifact
  metadata endpoint inside `deploy-pages` — GitHub-side, not a defect of this repository's code or site content.**
  This is an inference from the two timestamps and the zero-count listing; the alternative (a silently deleted
  artifact) is excluded by artifact `14287730363` still existing in the run's artifact list at `00:41:05Z`.

### AC-1b — attempt 2 (job `133777404970`): the retry was doomed before it started

- **Command run:**
  ```bash
  gh api repos/crier-dev/crier/actions/jobs/133777404970/logs > /tmp/intci040-job.log        # 20120 bytes, exit 0
  grep -nE 'Found [0-9]+ artifact|Fetching artifact metadata|successfully uploaded|##\[error\]' /tmp/intci040-job.log
  gh api repos/crier-dev/crier/actions/runs/43989570695/artifacts --jq '.artifacts[]|{id,name,created_at,size_in_bytes}'
  gh api repos/crier-dev/crier/actions/runs/43989570695 --jq '{created_at,run_attempt,conclusion,head_sha,event}'
  ```
- **Observed output:**
  ```text
  228:2026-11-17T02:41:29.8584671Z Artifact github-pages has been successfully uploaded! Final size is 3382430 bytes. Artifact ID is 14293802496
  244:2026-11-17T02:41:30.3418066Z Fetching artifact metadata for "github-pages" in this workflow run
  245:2026-11-17T02:41:30.6362602Z Found 2 artifact(s)
  246:2026-11-17T02:41:30.6382855Z ##[error]Fetching artifact metadata failed. Is githubstatus.com reporting issues with API requests, Pages, or Actions? Please re-run the deployment at a later time.
  247:2026-11-17T02:41:30.7936007Z ##[error]Error: Multiple artifacts named "github-pages" were unexpectedly found for this workflow run. Artifact count is 2.
  ```
  ```text
  {"created_at":"2026-11-17T02:41:29Z","id":14293802496,"name":"github-pages","size_in_bytes":3382430}
  {"created_at":"2026-11-17T00:41:05Z","id":14287730363,"name":"github-pages","size_in_bytes":3382427}
  ```
  ```text
  {"conclusion":"failure","created_at":"2026-11-17T00:40:52Z","event":"push","head_sha":"366eb95269d68b7257bead6bb1eef03dac700bbd","run_attempt":2}
  ```
- **Before:** attempt 2 was read as "the same job failed twice", i.e. a flaky deploy.
- **After:** attempt 2 failed for a different reason. `deploy-pages` resolves its artifact **by name within the run**
  (`Fetching artifact metadata for "github-pages" in this workflow run`), and the run held attempt 1's artifact
  (`14287730363`, created `00:41:05Z`) plus attempt 2's fresh upload (`14293802496`, created `02:41:29Z`). Two artifacts
  with the same name in one run is a hard error in `deploy-pages@v5`, so **re-running only the failed jobs of this
  workflow could never succeed**. Only a re-run of the whole workflow (new run id `44006024184` at `463a7c04`) was
  able to deploy, which is what actually recovered the lane.

## AC-2 — five consecutive Pages runs after the failure are green (the dismissal arm)

- **Command run:**
  ```bash
  gh run list --repo crier-dev/crier --workflow pages --limit 20 \
    --json databaseId,status,conclusion,headSha,createdAt \
    --template '{{range .}}{{.databaseId}}|{{.conclusion}}|{{.headSha}}|{{.createdAt}}{{"\n"}}{{end}}'
  ```
- **Observed output** (conclusions in order, newest first; the failure in the list is the run this row is about):
  ```text
  success|438e466c7ca2c46d44be45c3d894c3a1bb592539|2026-11-17T03:42:51Z
  success|accf4c3ce60268093a3e54240421d482ed62cd05|2026-11-17T03:36:03Z
  success|0c4abbc50547b96e75c5daf52e051f2ef23190cd|2026-11-17T03:35:20Z
  success|2b76ad5a9147c751811d89bbc0f3685abc6ff2b5|2026-11-17T03:34:58Z
  success|6b23d95bcd2c6626e2dfc78f86450c5c91aa1ce5|2026-11-17T03:13:16Z
  success|287589824682a9ceeecdf1b78c36304589577df9|2026-11-17T03:06:54Z
  success|463a7c04650141ac762712f1d52ae2a226438813|2026-11-17T02:51:36Z
  failure|366eb95269d68b7257bead6bb1eef03dac700bbd|2026-11-17T00:40:52Z
  ```
- **Before:** one red Pages run with no retrievable cause.
- **After:** 7 consecutive successful Pages runs **after** the failed one (`463a7c04` … `438e466c`), which is more than
  the five the row requires. The lane is classified a **transient** and the row is closable on this arm alone.

## The fix in this commit

`.github/workflows/pages.yml` now names the artifact per run attempt:

```yaml
      - name: Upload artifact
        uses: actions/upload-pages-artifact@v3
        with:
          path: site
          name: github-pages-${{ github.run_attempt }}
      - name: Deploy to GitHub Pages
        id: deployment
        uses: actions/deploy-pages@v5
        with:
          artifact_name: github-pages-${{ github.run_attempt }}
```

- **Command run:**
  ```bash
  actionlint .github/workflows/pages.yml; echo "actionlint rc=$?"
  gh api repos/actions/deploy-pages/contents/action.yml?ref=v5 --jq '.content' | base64 -d | grep -A2 artifact_name
  gh api repos/actions/upload-pages-artifact/contents/action.yml?ref=v3 --jq '.content' | base64 -d | grep -A2 'name:'
  ```
- **Observed output:**
  ```text
  actionlint rc=0
    artifact_name:
      description: 'Name of the artifact to deploy'
      required: false
      default: 'github-pages'
    name:
      description: 'Artifact name'
      required: false
      default: 'github-pages'
  ```
  i.e. both actions accept a caller-supplied name (read from the action manifests themselves, not from memory), so the
  attempt-scoped name is a supported input, not a guess.
- **Before:** a re-run of the failed jobs always collided (`Found 2 artifact(s)` → hard error).
- **After:** each attempt uploads and deploys its own uniquely named artifact, so a re-run no longer inherits the
  previous attempt's artifact. Verification of the happy path and of the re-run path is recorded in the appended
  section below (the GitHub-side workflow has no local runner; the run for this commit is the probe).

## External commits

- `366eb95269d68b7257bead6bb1eef03dac700bbd` — the push whose Pages deploy failed (analysed here, not changed)
- `463a7c04650141ac762712f1d52ae2a226438813` — the first green Pages run after the failure (the whole-workflow re-run)

## Appended 2026-11-17 — verification of the fix (probe: the workflow itself)

The GitHub-side workflow has no local runner, so the probe is the run for the fix commit. Command and output, verbatim:

- **Commands run:**
  ```bash
  git rev-parse HEAD                       # aab2aab255cc38828ccb019d9e040da00d64d317
  gh run list --repo crier-dev/crier --workflow pages --limit 1 --json databaseId,status,conclusion,headSha
  gh run rerun 44028742935 --repo crier-dev/crier          # attempt 2 of the SAME run id: the pre-fix trap
  gh api repos/crier-dev/crier/actions/runs/44028742935 --jq '"\(.run_attempt)|\(.status)|\(.conclusion)"'
  gh api repos/crier-dev/crier/actions/runs/44028742935/artifacts --jq '.artifacts[]|{id,name,created_at}'
  gh api repos/crier-dev/crier/actions/jobs/133855557716/logs | grep -E 'Fetching artifact metadata|Found [0-9]+ artifact|successfully uploaded'
  ```
- **Observed output:**
  ```text
  06:06:18 44028742935|waiting||aab2aab255cc38828ccb019d9e040da00d64d317
  06:06:34 44028742935|in_progress||aab2aab255cc38828ccb019d9e040da00d64d317
  06:06:50 44028742935|completed|success|aab2aab255cc38828ccb019d9e040da00d64d317
  06:07:21 attempt|status|concl=2|in_progress|null
  06:07:37 attempt|status|concl=2|completed|success
  {"created_at":"2026-11-17T06:07:14Z","id":14303114885,"name":"github-pages-2"}
  2026-11-17T06:07:14.7953434Z Artifact github-pages-2 has been successfully uploaded! Final size is 3382838 bytes. Artifact ID is 14303114885
  2026-11-17T06:07:15.1114565Z Fetching artifact metadata for "github-pages-2" in this workflow run
  2026-11-17T06:07:15.2191117Z Found 1 artifact(s)
  ```
- **Before:** an attempt 2 of a Pages run hard-failed with `Found 2 artifact(s)` → `Multiple artifacts named "github-pages" were unexpectedly found` (AC-1b).
- **After:** attempt 1 of run `44028742935` (head `aab2aab2`) deployed green, and **attempt 2 of the same run id also
  deployed green** — the exact operation that previously could never succeed. The deploy step resolved
  `github-pages-2` and got `Found 1 artifact(s)`, i.e. one unambiguous artifact per attempt.
- **Residual, not claimed:** the AC-1a race (artifact uploaded but not yet listed) is GitHub-side and is not removed by
  this change; a repeat would still need a retry — but the retry now works instead of being guaranteed to fail.

### Verification verdict (GitReins tier-1 + tier-2 on the fix commit)

- Runs: `.gitreins/history/2026-11-17/0d245e9d/verdict.json` — task `INT-CI-040`, commit
  `aab2aab255cc38828ccb019d9e040da00d64d317`, `passed: true`, overall **PASS**.
- Tier 1: guard PASS. Tier 2: PASS (re-derived the log lines from the live API, re-ran actionlint, confirmed both
  action inputs from the upstream manifests, and confirmed run `44028742935` attempt 2 is green).
- Tier-2 boundary: the guard reported `DEGRADED PASS (skips: go_build, go_lint, go_tests — no Go files staged)`. That is
  correct and expected for a workflow+evidence diff; the Go suite was not run by this task and is not claimed.
