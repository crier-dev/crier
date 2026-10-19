# DF-CRIER-392 — independent capture binding

## Mechanism and boundary

Keep the existing body nonce and trimmed `stdout_sha256` contract. Add
`stdout_raw_sha256`, and select the additive `?stream=stdout` observer mode.
The verifier reads its own capture file as bytes, hashes it before any decoding
or normalization, checks the raw hash and length, then checks strict UTF-8 and
exact trimmed capture/reply equality. The existing observer without a query
still streams stdout+stderr. This avoids interleaving stderr into a stdout hash.
An older nonce in either channel is NONCE_REPLAY.

The consumer still serves the observation HTTP endpoint. This is independent
capture-to-body integrity, NOT independent hostile-consumer spawn attestation.
No claim is made that a malicious consumer cannot fabricate both channels.

## Three required negative controls, proved RED on the original checker

Pre-change revision: `28693d2dfbb7bd2bacd14fdca1ec0317cf1a83e0`.
Extract that revision's `examples/agent-ecosystem/battery/check-harness.py` to
`/tmp/DF-CRIER-392-prechange-checker.py`, then run:

    DF_CRIER_CHECKER=/tmp/DF-CRIER-392-prechange-checker.py python3 -m pytest -q -s examples/agent-ecosystem/battery/test_binding.py -k 'request_body_tee or child_spawn or successful_capture_replayed'

Actual pre-change failure text (exit 1, 3 failed, 17 deselected):

    test_request_body_tee_without_child
    E       assert 'SUPPORTED' == 'DIGEST_MISMATCH'

    test_child_spawn_with_fabricated_reply
    E       assert 'SUPPORTED' == 'DIGEST_MISMATCH'

    test_successful_capture_replayed_under_fresh_body
    E       assert 'NOT_EXECUTED' == 'NONCE_REPLAY'

The third control first captures a genuinely successful child run, verifies it,
then replays its stdout with a fresh, self-consistent response body. The old
checker rejected that pair, but did not identify the captured nonce replay.
The first control tees the request bytes verbatim, including a literal nonce
marker inside request text, without a runtime child. The second runs a child
but fabricates a different, internally consistent delivery reply.

After the fix, those same controls return DIGEST_MISMATCH, DIGEST_MISMATCH,
and NONCE_REPLAY. New controls also cover changed whitespace/CRLF/invalid bytes,
copying the real capture hash onto different body text, raw byte-count mismatch,
and ECMAScript versus Python whitespace trimming. They use real subprocess
pipes/files under /tmp, with no sockets or provider calls. Python's existing
transport fixture is now file-backed too. The Node suite retains its existing
loopback-only HTTP integration harness; it makes no external provider calls.

## Verification

    python3 -m pytest examples/agent-ecosystem/battery/test_binding.py
    20 passed

    node --test examples/agent-ecosystem/battery/execution.test.mjs
    tests 76; pass 76; fail 0

Shipped good consumer + fake runtime: SUPPORTED. Whole battery good fixture:
13 pass / 0 fail / 0 skip, exit 0. Echo-without-run fixture:
7 pass / 6 fail / 0 skip, exit 1. Legacy stderr observation still yields
ECHOED_WITHOUT_RUN when the body lacks the nonce. The stdout-only integration
control emits valid UTF-8 plus surrounding whitespace and stderr diagnostics;
it passes only with the exact stdout raw hash and byte count.

    go build -p 4 ./...
    go vet -p 4 ./...
    go test -p 4 ./... -count=1 -short -parallel 4

All exited 0. `git diff --check` passed.
`gitreins guard` exited 0: secrets clean, DEGRADED PASS because its three Go
checks skipped (no Go files staged). This guard result is not test evidence;
the suites above were run directly.

## Exact new specification limit paragraph

Limit: SUPPORTED binds a fresh single-use challenge and the delivered reply to
independently captured stdout bytes; with the trusted shipped consumer's
stdout-only pipe tee, it establishes that a spawned process emitted those bytes.
The nonce remains visible in the request, and the observation endpoint is served
by the consumer, not a verifier-owned OS pipe: a malicious consumer that
fabricates both channels consistently can still pass without spawning a child.
This is neither hidden-challenge proof nor hostile-consumer attestation, and it
does not establish model authorship, answer quality, or provider/model identity.
Do not expose this diagnostic stack to untrusted clients. No Crier server change
is required.
