# DF-CRIER-338 verification

Scope: offline checker, real consumer subprocesses with fixture executables/SDK,
and whole-battery transport fixtures. No vendor credentials or model calls were
used. The committed `df-crier-338-offline.jsonl` explicitly labels every row as
transport-fixture evidence, not evidence that those six vendor runtimes ran.

## Checker cases (executed from the repository root)

Generate the retained fixture bodies:

```bash
python3 examples/agent-ecosystem/battery/test_binding.py --fixtures /tmp/DF-CRIER-338-fixtures
C=examples/agent-ecosystem/battery/check-harness.py
F=/tmp/DF-CRIER-338-fixtures
N=4bb57a79-dd26-45be-a3ed-bf78d52e7b68
```

Each following command was executed with the shown exit code:

| Command | rc | Observed reason |
| --- | --- | --- |
| `python3 "$C" 200 "$N" < "$F/wrong.json"` | 1 | missing/mismatched runtime nonce echo |
| `python3 "$C" 200 "$N" < "$F/missing.json"` | 1 | missing/mismatched runtime nonce echo |
| `python3 "$C" 200 "$N" < "$F/replayed.json"` | 1 | missing/mismatched runtime nonce echo |
| `python3 "$C" 200 "$N" < "$F/genuine.json"` | 0 | live=true, resolved fixture exec, positive bytes, nonce_matched=true |
| `python3 "$C" 200 "$N" < "$F/canned.json"` | 1 | live=false (canned/non-executed harness) |
| `python3 "$C" 200 "$N" < "$F/metadata-only.json"` | 1 | missing/mismatched runtime nonce echo (metadata alone is insufficient) |

For the paired-prompt digest control:

```bash
D=$(python3 "$C" 200 "$N" < "$F/same-text.json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["content_sha256"])')
python3 "$C" 200 "$N" "$D" < "$F/same-text.json"
python3 "$C" 200 "$N" "$D" < "$F/different-text.json"
```

The digest acquisition returned rc=0 and
`04508bbbcadf2034ddb0e99d1254bde840347eb58caaf576f307bf02df2bb0f8`.
The same-text case returned rc=1, `same runtime stdout digest for different prompts
(nonce markers removed)`; different-text returned rc=0. The consumer subprocess
regression additionally drives two different prompts/nonces into a real fixture
CLI that returns constant answer text: raw stdout hashes differ solely because
of the fresh markers, but the nonce-redacted digest check correctly fails.

## Negative control

The checker from pre-fix commit `3ca90c09ed839b5d17eeded2a313e6992af36d5a`
was extracted with `git show <sha>:examples/agent-ecosystem/battery/check-harness.py`
to `/tmp/DF-CRIER-338-prefix-checker.py`. Running
`python3 /tmp/DF-CRIER-338-prefix-checker.py 200 < "$F/<case>.json"`
returned rc=0 for each of `wrong`, `missing`, `replayed`. The new checker returns
rc=1 for all three without relaxing any pre-existing live/exec/bytes/text checks.

## Gates

All commands below returned rc=0:

```bash
python3 examples/agent-ecosystem/battery/test_binding.py
node --test examples/agent-ecosystem/battery/execution.test.mjs
bash -n examples/agent-ecosystem/battery/battery.sh
python3 -m py_compile examples/agent-ecosystem/battery/check-harness.py examples/agent-ecosystem/battery/test_binding.py
node --check examples/agent-ecosystem/consumer/consumer.mjs
node --check examples/agent-ecosystem/battery/execution.test.mjs
GOMAXPROCS=4 go build -p 4 ./...
GOMAXPROCS=4 go vet -p 4 ./...
GOMAXPROCS=4 go test -p 4 ./... -count=1 -short -parallel 4
/home/kara/gitreins/.venv/bin/python -m gitreins guard --full
bash scripts/check-shell-yaml.sh examples/agent-ecosystem/battery/battery.sh
git diff --check
```

Python: 8 tests. Node: 18 tests. Whole-battery fixture tallies:
no-key = 5 pass / 0 fail / 7 skip (rc=0);
canned-with-key, replayed, missing, same-text = 7 pass / 6 fail / 0 skip (rc=1 each);
valid live-shaped fixture = 13 pass / 0 fail / 0 skip (rc=0).
Evidence-copy failure remains rc=1. Each live harness verdict now requires two
successful request proofs; the six harness verdicts still count once each.

## Assurance and limit

An evidence reader can now compare this request's expected nonce with the runtime
text marker and extracted echo, recompute its text digests, group it by run UUID
and start time, and verify the second prompt did not repeat the first answer.
Previously, a plausible `live:true` response from any earlier request passed.
Nonce binding does not cryptographically attest a remote consumer: a malicious
nonce-aware endpoint can fabricate new matching bodies and varied text. The
checked-in consumer's stdout/SDK extraction path is covered by subprocess tests;
no claim of unforgeability against arbitrary consumer code is made.
