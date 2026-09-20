# QA-CRIER-20 — act leg filtered to the repo ci.yml workflow (tick 373)

Tick: crier-2026-09-20-22-29-40 (board tick 373)
Worker: hermes chat session 20260920_174528_24590e (glm-5.3-flash @ zai-glm-default)
Changed file: /home/kara/.hermes/scripts/bunker-qa.sh (shared QA harness — OUTSIDE the crier repo)

## Problem (row premise, measured 2026-09-18 on agent b1237b9c)
bunker-qa.sh's ci-pass cell invoked `act -q --pull` with NO workflow filter, so every repo
workflow ran inside act. Infra-gated workflows (crier's bunker-e2e.yml ghcr-publish job:
needs live bunker server + ghcr creds + real push event) contributed a PERMANENT red act leg.
Hosted CI at the same HEAD: 5/5 green — the red was harness scope, not a repo defect.

## Premise verified live pre-dispatch
- `grep -n "act"` in bunker-qa.sh: detect_cmds line 245 built the unfiltered command.
- crier workflows: bunker-e2e.yml, ci.yml, pages.yml; ghcr publish lives in bunker-e2e.yml.
- act on PATH (`~/.local/bin/act`) accepts `-W <file>` (help screen).

## Fix (2 hunks, one file)
1. detect_cmds() (~line 246): when the repo has workflows and ci.yml exists, the act command
   is now `act -q --pull -W <repo>/.github/workflows/ci.yml`; workflows-without-ci.yml keep
   the unfiltered form; no-workflow repos still get native-as-CI. FIX-note comment added in
   the file's house style.
2. Bottom case dispatch (~line 1294): new `__detect-cmds` arm — runs detect_cmds against any
   directory and prints DETECT_INSTALL/NATIVE/CI, making the detection layer testable from
   the host (same pattern as the existing `__gen-remote` arm).

## Foreman verification (all run by the foreman, not trusted from the worker report)
- `bash -n` → exit 0.
- `bunker-qa.sh __detect-cmds /home/kara/crier test-agent-0001` →
  `DETECT_CI=DOCKER_HOST=unix:///run/bunker/test-agent-0001/docker.sock ~/bin/act -q --pull -W /home/kara/crier/.github/workflows/ci.yml` (infra workflows excluded).
- Fixture A (workflows, no ci.yml) → unfiltered `act -q --pull` (no -W). Preserves harness
  behavior for repos whose relevant workflow is named differently.
- Fixture B (no workflows, go.mod) → DETECT_CI == DETECT_NATIVE (`go test ./... -count=1`).
- Isolation: `git -C /home/kara status` after the run shows exactly the 5 pre-existing
  modifications (fleet.toml, fleet-cooldown-policy.py, scan-git-secrets.py, 2 skills files),
  nothing staged, no new untracked QA20 artifacts.
- Plumbing (criterion 6): run() passes $DETECT_CI into build_remote_script(); its heredoc is
  UNQUOTED, so `$ci_cmd` expands at generation time and the filtered command is baked
  verbatim into the agent's qa-run.sh. No further change needed.

## Why no commit in ~/.hermes
`/home/kara` is a local-only git repo (no remote) whose tracked set includes .hermes/skills/**
and some scripts, but bunker-qa.sh has ALWAYS been untracked (along with its .bak-* backups) —
the fleet treats it as a live host tool. Committing it now would deviate from the established
convention without adding durable value (no remote to push). This evidence file, the board
event, and the .gitreins task record ARE the durable record, and they are pushed.

## Effect on future QA cycles
Next battery on any repo with a ci.yml runs ONLY that workflow in act: no more permanent red
leg from infra-gated jobs; ci-pass grades what a simulated runner can actually express. The
native suite remains the authoritative fallback grade (unchanged).
