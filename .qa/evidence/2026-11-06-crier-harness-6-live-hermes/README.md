CRIER-HARNESS-6: live delivery into a real Hermes session — PASSED on this host (2026-11-06). Both legs are env-gated (`CRIER_TEST_LIVE_HARNESS=1`) and drive the operator's installed `hermes` v0.22.5 CLI into a REAL session in `~/.hermes/state.db`. Exact commands and full output are in `agent.log`.

CRITERION 1 (POST /ingress/messages → delivery.state=dispatched): EVIDENCED by `cmd/server/hermes_live_delivery_test.go::TestIngressDispatchesIntoARealHermesSession`, which boots the real server (`startTestServerWithEnv`, `CR_HARNESS_ENABLED=true`, `CR_HARNESS_DEFAULT=hermes`, `CR_HARNESS_HERMES_BINARY=<real hermes>`) and POSTs to the served route. Observed: `POST /ingress/messages status=200`, `delivery={Mode:harness Harness:hermes State:dispatched Error:}` for both pushes. The adapter-level half (`internal/harness/hermes/live_delivery_test.go::TestLiveHermesDeliveryLandsInTheRealSession`) proves the same receipt without the HTTP layer.

CRITERION 2 (token in Hermes's own transcript, path quoted): EVIDENCED. The transcript is SQLite, not a per-session `.jsonl` on this build: file `/home/kara/.hermes/state.db`, table `messages`, keyed by `session_id`. Run 1 found `crier-live-hermes-1794027201677028949-one` at `row_id=6812994` (role=user) in session `local-2eb69303ed6dc7d9bb998391a77b06c27d4300ff79d324cb3fe0cfc3bd6e692f`; run 2 found `crier-live-hermes-ingress-1794027319742441604-one` in session `local-71f0f2dbc5bfbea2176b4ed02b93f2a60404c20bf05991ef951b1e1b75bbd0eb` (4 rows at read time). The store is opened `mode=ro`; nothing in either test writes or deletes a transcript.

CRITERION 3 (second push continues the SAME session): EVIDENCED. Each leg pushes a second token and asserts the continuation four ways — same harness session id in the receipt (`Created:false`), same transcript file, both tokens present, and the transcript GREW (run 1: 4 → 6 rows; run 2: 4 → 6 rows) while the store still holds exactly one session row for that id. A silent re-create would leave one of the two tokens missing, which is what the assertions refuse.

CRITERION 4 (which transport delivered): NAMED — CLI resume. The adapter ran `hermes chat -Q --resume <session-id> -q <text>`, the continue surface, which is the only one that reports `dispatched` (the gateway `POST /v1/responses` create path reports `accepted`, and it needs `adapters.hermes.gateway_url` plus the operator's gateway token — the gateway on this host answers 401 without it). Both legs therefore bind a session that already exists (the state a deployment reaches after its first recorded bind) and prove the resume transport. The create path was NOT used in either run.

Session hygiene: each run mints exactly ONE fresh session via `hermes chat -Q --in <tempdir> -q <seed>` and appends to it. No existing session is resumed, renamed, pruned or deleted; the seed sessions remain readable in the operator's store (`Crier delivery probe`).

Verification commands (both from `/home/kara/worktrees/crier-CRIER-HARNESS-6`):

    go build ./... && go vet ./... && go test -short -count=1 ./internal/harness/...
    CRIER_TEST_LIVE_HARNESS=1 go test -count=1 -timeout 600s -run 'LiveHermes' ./internal/harness/hermes/
    CRIER_TEST_LIVE_HARNESS=1 go test -count=1 -timeout 900s -run 'TestIngressDispatchesIntoARealHermesSession' ./cmd/server/

Results: build OK, vet OK, `./internal/harness/...` all ok; live leg 1 PASS (50.98s); live leg 2 PASS (104.55s).

## Trace

ch:trace row=CRIER-HARNESS-6 spec=specs/CRIER-HARNESS.md#2 test=internal/harness/hermes/live_delivery_test.go::TestLiveHermesDeliveryLandsInTheRealSession+cmd/server/hermes_live_delivery_test.go::TestIngressDispatchesIntoARealHermesSession doc=.qa/evidence/2026-11-06-crier-harness-6-live-hermes/README.md prompt=none:dispatched-brief evidence=.qa/evidence/2026-11-06-crier-harness-6-live-hermes/agent.log witness=/home/kara/.hermes/state.db#messages verdict=none:foreman-judges commit=none:self-referential-hash memory=none:foreman-writes-memory
STATUS: complete
TIER: T1
EVIDENCE: .qa/evidence/2026-11-06-crier-harness-6-live-hermes/agent.log
