# DF-CRIER-464 — live verification of the UAT promotion gate

The gate is `scripts/deploy-ui-test.sh` (with `scripts/deploy-ui-test-lib.sh`), proven by
`scripts/deploy-ui-test-selftest.py`. This file records the two runs the coordinator performed
against the REAL instance on 2026-10-21 (`crier-ui-test-net`, container `crier-ui-test-8adbf50c`,
edge `crier-ui-test-edge`, volume `crier-ui-540a593-data`), because a selftest drives fixtures and
a fixture is not the instance.

## Why the gate exists (the measured failure it answers)

`crier-ui-test:8adbf50` was built from `/home/kara/crier` while that checkout held 12 modified
tracked files (+214/-5) belonging to a different writer. The binary reported honestly —
`crier vui-test-8adbf50-8adbf50d-dirty`, `/version` `{"modified":true}` — but the tag said
`8adbf50`, the instance was announced as commit `8adbf50`, and it was not: it carried unreviewed
code, and `modified` is a boolean that names none of the twelve files.

A second, separate defect surfaced in the same deploy: the container was started without
`CR_REGISTRY_BACKEND=sqlite` / `CR_REGISTRY_SQLITE_PATH`, so the registry fell back to memory and
`GET /agents` returned `{"agents":[]}` against a volume that already held six registered agents.
Both are recorded in board row `DF-CRIER-464`.

## Run 1 — REFUSED (the dirty image, exit 1)

```
$ scripts/deploy-ui-test.sh --image crier-ui-test:8adbf50 --commit 8adbf50 \
      --admin-token-file /tmp/crier-admin-token.txt ; echo "REFUSE_RC=$?"
REFUSED: candidate provenance does not match; nothing promoted
expected commit=8adbf50; reported commit=8adbf50d; dirty=true
REFUSE_RC=1
```

Nothing was removed: the refusal happens before the old container is touched, which is the
property that matters — a gate that believes it can refuse while rebuilding is not a gate.

## Run 2 — PROMOTED (the clean image, exit 0)

```
$ scripts/deploy-ui-test.sh --image crier-ui-test:8adbf50-clean --commit 8adbf50 \
      --admin-token-file /tmp/crier-admin-token.txt --container crier-ui-test-8adbf50c
expected commit=8adbf50; reported commit=8adbf50d; dirty=false
verified /version read 1/3: expected commit=8adbf50; reported commit=8adbf50d; dirty=false
verified /version read 2/3: expected commit=8adbf50; reported commit=8adbf50d; dirty=false
verified /version read 3/3: expected commit=8adbf50; reported commit=8adbf50d; dirty=false
PROMOTED: crier-ui-test:8adbf50-clean as crier-ui-test-8adbf50c \
  (sha256:8e9da2cfffa4b1b334d295678be54175709f3b53a0e7ff03fb26b08d912c8208); \
  three live provenance reads agree
```

After the promotion the instance was still addressable:

```
$ curl -s $UAT/agents -H "X-Operator-Token: ***" | jq -c '[.agents[].id]'
["atlas","blackhole-bot","dead-bot","df-user-20261020","echo-bot","scout"]
```

**That last line passed for the wrong reason** and the gate was not credited for it: the script
preserves the currently-running container's environment, and that container happened to carry the
registry variables because they were added by hand. A fresh promotion, or a container that had
never been corrected, would still have dropped the registry to memory. The `agents` read-back is
therefore an ENFORCED step and an ENFORCED assertion now, not an inherited accident — the
criterion stayed open until the script itself wrote `CR_REGISTRY_BACKEND=sqlite` /
`CR_REGISTRY_SQLITE_PATH=/data/registry.db` and compared the pre-removal agent count against the
post-start one.

## Run 3 — PROMOTED from the DEFECT STATE (the measured proof that the gate enforces, not inherits)

The first promotion kept its agents because the container it replaced had been corrected by hand.
So the gate was re-run from the opposite starting point: the live container was deliberately
recreated **without** `CR_REGISTRY_BACKEND` / `CR_REGISTRY_SQLITE_PATH` on the same volume, which
reproduces the original defect exactly —

```
DEFECT-STATE agents: {"agents":[]}
```

— and then the gate was pointed at it:

```
$ scripts/deploy-ui-test.sh --image crier-ui-test:8adbf50-clean --commit 8adbf50 \
      --admin-token-file /tmp/crier-admin-token.txt --container crier-ui-test-8adbf50c
expected commit=8adbf50; reported commit=8adbf50d; dirty=false
pre-removal agents before=0
verified /version read 1/3 … 2/3 … 3/3: commit=8adbf50d; dirty=false
registry agents before=0; after=6
PASS: registry durability; agents before=0; after=6
PROMOTED: … ; three live provenance reads agree; registry durability PASS (agents before=0; after=6)
RC=0
```

Six agents reappeared from a container that had none, on a build the gate itself refused to
describe as anything other than `8adbf50d`, clean. That is the property: the promotion writes the
registry posture, it does not hope the previous container had it.

## What the selftest covers (fixtures, not the instance)

`python3 scripts/deploy-ui-test-selftest.py` → `RESULT 39/39 passed`, including: a `-dirty` image is
refused with all three values printed; a mismatched commit is refused; unreadable provenance is
refused rather than treated as clean; a later disagreement cannot be hidden by an earlier clean
read; a JSON `dirty` marker refuses even when `modified` is false; removal failure never starts a
replacement; a start failure never reports success; inherited `CR_REGISTRY_*` / `CR_DATABASE_URL`
lines are stripped and the registry posture is written explicitly; fewer agents after start than
before is a FAIL; equal or more is a PASS; and a malformed / missing / null / object / multi-document
`/agents` body is reported **unverified**, never zero. A NEUTER copy — with only the dirty decision
disabled — ACCEPTS the fixture the real script refuses, so the green cannot be vacuous.

## Limits, stated

- The gate proves provenance and posture, not behaviour: it does not run the chat harnesses, and a
  clean, correctly-configured image can still be wrong.
- Agent-count comparison proves the registry SURVIVED the recreate, not that every registration is
  readable or correct.
- `--commit` matches on prefix, so a 7-character commit identifies a build uniquely only while the
  repository does not grow a second commit with that prefix.
