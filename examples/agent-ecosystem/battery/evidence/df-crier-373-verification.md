# DF-CRIER-373 — verifier-owned runtime binding

Base revision: `01645704edda1bd97655275878776b8c2379a444`.
All fixtures below are offline test processes and synthetic transport; none is evidence of a paid provider/model completion.

## Contract and wiring

`battery.sh` calls `probe-runtime.py` for each prompt. The probe mints and records its own nonce before network activity, opens the consumer's observation stream before delivery, captures raw child pipe bytes separately from the returned body, then invokes the shared checker. The nonce ledger spans all six harnesses and both prompts; every attempted challenge is consumed, even on failure. A body cannot choose the expected nonce, digest or capture source.

The consumer's observation route streams only `runCli` child stdout/stderr chunks. It does not stream request metadata, response fields or consumer diagnostic logs; opening it does not execute anything. Pi's SDK is already inside the same bounded subprocess. Normal registration/credentials and FAILED-with-stderr behavior remain intact. No Crier server, board or state/config file was edited.

`request_sha256` is extracted from the runtime's returned marker. The verifier independently computes its expected value from compact UTF-8 JSON containing `battery_nonce`, then `text`, from the payload it actually sent. This is distinct from the retained `stdout_sha256` response-integrity check. Both challenge markers are removed before comparing paired answer content.

## Executed fixture outputs

Command:

    python3 examples/agent-ecosystem/battery/test_binding.py

Observed HTTP fake-consumer/subprocess results:

    fixture echo-without-run: FAIL / NOT_EXECUTED; runtime_nonce_observed=False; exit 1
    fixture run-and-echo: PASS / SUPPORTED; runtime_nonce_observed=True; exit 0
    fixture runtime-only: FAIL / ECHOED_WITHOUT_RUN; runtime_nonce_observed=True; exit 1
    fixture digest-mismatch: FAIL / DIGEST_MISMATCH; runtime_nonce_observed=True; exit 1
    fixture replay: FAIL / NONCE_REPLAY; runtime_nonce_observed=True; exit 1
    Ran 12 tests
    OK

The replay case first completes a real fake-runtime invocation successfully, then returns that original nonce/body on a second fresh request. Both request nonces are verifier-minted and consumed; the old reply is refused. Separate unit coverage refuses reuse of the exact same consumed nonce and any unminted nonce.

Command:

    node --test examples/agent-ecosystem/battery/execution.test.mjs

Observed:

    fixture live-with-key: 13 pass / 0 fail / 0 skip; exit 0
    fixture echo-without-run: 7 pass / 6 fail / 0 skip; exit 1
    fixture replayed: 7 pass / 6 fail / 0 skip; exit 1
    actual consumer + fake runtime: SUPPORTED; nonce in child pipes; verifier request digest matched
    tests 74
    pass 74
    fail 0

The Node suite also runs the shipped consumer with fake executables/SDKs and the production Python probe, including a UTF-8 request. A stderr-only nonce is observed independently but cannot replace the required nonce in the body. Existing provider-key refusal, timeout, process-group kill, exit-3/stderr, empty-output and paired-content controls remain green. CI invokes both suites without provider keys.

## Pre-change RED proof

The historical battery and checker were extracted together with `git show` from the base revision into a temporary directory; the worktree production files were not reverted. `DF_CRIER_CHECKER` and `DF_CRIER_BATTERY` select those historical copies only in the new tests.

The same forged body (no runtime started) against the historical checker returned exit 0:

    {"nonce_matched": true, "reason": "live=true exec=/fixture/runtime stdout_bytes=73", "result": "PASS"}

The new `BindingTests.test_no_capture_cannot_pass_cli` consequently failed against that old checker with `AssertionError: 0 != 1` (test exit 1). Against the updated checker it passes.

The new whole-battery test against the historical battery reached its echo-without-run fixture and printed:

    PASS  round-trip pi-agent via crier prompt 1 (live=true exec=/fixture/opencode stdout_bytes=202)
    PASS  round-trip opencode via crier prompt 1 (live=true exec=/fixture/opencode stdout_bytes=202)
    == battery done: 13 pass / 0 fail / 0 skip (.../echo-without-run.jsonl)
    expected: 1
    actual: 0

The test exited 1 because the old battery accepted every fake harness; the new battery refuses all six. Full local transcripts: `/tmp/df373-red-python.log`, `/tmp/df373-red-battery.log`, `/tmp/df373-python.log`, `/tmp/df373-node.log`.

## Assurance limit

Current assurance clarification (DF-CRIER-419; the historical run transcripts above are unchanged):
SUPPORTED means the capture bytes reported by the consumer equal its reply, hashed before decode; it is not attestation against a consumer fabricating both channels.
Under the trusted shipped observer, those bytes are teed from child stdout, but
SUPPORTED alone does not prove that the requested work ran. The consumer serves
both channels and can fabricate them without a child. HTTP transport success
alone is never sufficient. The observation endpoint is a diagnostic surface for
an isolated example stack, not a public Crier API.

## Direct gates

- `go build -p 4 ./...` — exit 0.
- `go vet -p 4 ./...` — exit 0.
- `go test -p 4 ./cmd/server ./internal/relay -count=1 -timeout 600s` — exit 0; server 93.406s, relay 6.904s.
- `make docs-check` — exit 0.
- `make shell-yaml-check` — exit 0, including the CI step and changed battery shell.
- `make make-docker-check` — exit 0.
- `node --check` on the consumer and Node tests — exit 0.
- `git diff --check` — exit 0.

- `go test -p 4 ./... -count=1 -short -parallel 4 -timeout 600s` — exit 0, full repository short suite; transcript `/tmp/df373-go-full.log`.
- `gitreins guard --full` — exit 0, `Tier 1 Guards: PASS (test mode: full, whole tree)`; secrets clean, Go build/lint OK and Go tests passed. This was a real 130-second run, not a no-Go short-circuit. Log: `.gitreins/logs/guard-20261019T163828.135509Z.log`.

Go commands use `-p 4` to cap package build concurrency; they cover the full requested package sets.
