# QA-CRIER-19 — ui-probe arm for Go-served HTML surfaces (tick 378)

Tick: crier-2026-09-21-04-47-45 · Worker: glm-5.3-flash @ zai-glm-default, session 20260921_000057_50bddb
Deliverable: /home/kara/.hermes/scripts/bunker-qa.sh (untracked fleet harness, no repo commit) — one hunk in the ui-probe block, +77/−3; backup bunker-qa.sh.bak-20260921-qa19.

## Defect
The ui-probe cell keyed only on package.json dev/start scripts, so crier (Go; serves 200 text/html at /docs on :8767, asserted by cmd/server/main_test.go:225) was graded N/A "no web UI detected" on every battery cycle — the shipped HTML surface was never probed. Twice-recurred (2026-09-18 + 2026-09-19 batteries).

## Fix (3-arm gate, board-pinned direction "probe the running service before accepting N/A")
1. npm arm unchanged (package.json dev/start → :3111 probe).
2. NEW `elif [ -n "$BIN" ]`: starts the repo's own detected start command (setsid + timeout 45, stdin detached), polls BUNKER_QA_UI_PORTS (default `3000 5173 8000 8080 8081 8767 9000`) for `200 text/html` on /docs then /, classifies OK / INFO-serves-non-HTML / INFO-not-a-server / INFO-no-port-answered; never FAIL for CLI-shaped repos. Teardown kills the process GROUP (go run's real listener is a child) and frees only ports the probe itself opened.
3. `else` N/A unchanged — now honest (detection actually attempted).

Measured deviations from the original brief, each documented in the block comment: cmd/ fallback queue (upstream detection resolves crier-mcp, a stdio server that exits on stdin EOF, before cmd/server); /docs polled before / (crier / is 404 JSON); setsid group-kill (wrapper kill orphans the go-run child); occupied-port baseline guard (fleet box serves foreign HTML on :5173 — canopy-vite — which falsely credited a prior design, and a bare `fuser -k` killed an unrelated socket-holder; guard prevents both).

## Verification
Worker probes V1–V6 all PASS (generated-body bash -n via `__gen-remote`; live crier fixture → `OK … :8767 (go run ./cmd/server)` in 5s; empty dir → honest N/A; quick-exit go.mod → INFO not FAIL; no listener leak on 8767/3111; group-kill proven on an inner process).
Foreman adversarial re-run (independent, /tmp/crier-t378-foreman-verify.sh): R1 generation rc=0 + body syntax OK · R2 live fixture → `CELL: ui-probe | OK | HTML surface at http://127.0.0.1:8767 (<start cmd: go run ./cmd/server>)` (exercises the cmd/ fallback queue live: crier-mcp exits → queue falls through to cmd/server) · R3 no listener leak · R4 canopy-vite still 200 on :5173 · R5 honest N/A · R6 INFO not-a-server (re-run with corrected harness; first N/A was a foreman harness bug, not the deliverable) · R7 whole-file bash -n OK.
Repo gates: go build/vet clean, gofmt clean, gitreins guard PASS full mode (guard-20260921T061048Z).

## Environment notes
- Concurrent sibling worker (hermes-canopy QA-HERMES-CANOPY-17) edited the same generator's sync_repo/buildx regions during this tick; regions disjoint, final file syntax-clean, re-verified post-exit.
- Off-by-one: discover missed for the class → post-debug submission sub_bd5436 (bash-generated-script-heredoc-escaping; heredoc escaping hit 3× fleet-wide in one day). Tier-2 verdict dir lands in .gitreins/history/2026-09-21/.
