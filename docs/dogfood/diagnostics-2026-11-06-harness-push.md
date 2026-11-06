# Diagnostics trail — 2026-11-06 crier harness push dogfood

Lane tick `crier-dogfood-2026-11-06-12-41-09`. This is the "how it was built,
what broke, the right way" record for the run summarised in
[2026-11-06-harness-push-real-use.md](2026-11-06-harness-push-real-use.md).

## How the run was set up (reproduction)

```bash
# 1. published tip, verified before anything was measured
git -C /home/kara/crier rev-parse HEAD            # 5dc81b05…  (== origin/main at the time)
# .git/REBASE_HEAD existed with NO rebase-merge/ rebase-apply/ dir → stale marker.
# Per the perf rule a marked live tree is not timed, so work happened in a clone:

SCRATCH=/home/kara/.hermes/cache/scratch/dogfood-crier-20261106
git clone --no-hardlinks --single-branch --branch main /home/kara/crier $SCRATCH/src   # 3.0 s
cd $SCRATCH/src && git rev-parse HEAD && git status --short -uno   # same SHA, clean

# 2. build (documented path)
make build                                          # 3.40 s, bin/crier
./bin/crier -version                                # crier v0.2.0-455-g5dc81b05-5dc81b05

# 3. scratch server with the harness registry ON
mkdir -p $SCRATCH/run && cd $SCRATCH/run
CR_GUARD_ENABLED=false CR_HARNESS_ENABLED=true CR_HARNESS_DEFAULT=pi \
CR_HARNESS_PI_BINARY=/home/kara/.local/bin/pi \
  $SCRATCH/src/bin/crier -port 18911 -pidfile .crier.pid

# 4. the documented client, real ed25519 key
PYTHONPATH=$SCRATCH/src/clients/python python3 $SCRATCH/probe_harness.py
```

`probe_harness.py` registers `pi-bot` (`Crier(base_url, agent_id, key=hex seed)`),
then sends signed `POST /ingress/messages` bodies in four shapes and logs every
status/body/timing to `ev/probes.jsonl`.

## Errors hit while probing (and which were mine, not the product's)

| Symptom | Cause | Correct reading |
|---|---|---|
| `400 INGRESS_TEMPLATE_INVALID: target.template.kind "room" is not "channel" or "direct"` | my fixture used `kind:"room"` | my error; `kind` is a closed set. Fixed in the probe, not filed. |
| `409 agent already registered: "pi-bot"` | re-running the probe against the same live server | expected; `POST /agents` is create-only. Probe now tolerates 409. |
| `GET /sessions/{id}` → `404 not found` | **no such route exists** — `cmd/server/main.go` registers `/sessions`, `/sessions/{id}/messages`, `/sessions/{id}/state`, …, but never `GET /sessions/{id}` | my instrument error; the 404 is correct. Do **not** file it. The session is readable through `GET /sessions` (list) and `GET /sessions/{id}/messages`. |
| `400 INGRESS_INVALID_JSON: target: unknown field "harness"` | the product refusing the field the spec documents | **the finding** (DF-CRIER-500). Not a fixture mistake: the refusal's own accepted-field list (`session_id, template, agent_id, capability`) is the proof that no spelling of the key can work. |

## The right way to check "is the harness wired?"

Read the request structs, not the docs, for the accepted vocabulary:

```bash
sed -n '45,70p'  internal/ingress/request.go     # rawTarget / rawTemplate: the whole accepted key set
sed -n '445,458p' internal/session/http.go       # createSessionRequest: no harness field
sed -n '74,82p'  cmd/server/harnesswiring.go     # name == "" → {"mode":"store","state":"stored"}
```

and confirm no other writer exists:

```bash
grep -rn "\.Harness\s*=" --include=*.go internal cmd | grep -v _test
grep -rn "HarnessBindRecord(" --include=*.go .        # only cmd/server/harnesswiring.go:104
grep -rn "Harness" --include=*.go internal/registry | grep -v _test   # empty: agents carry no binding
```

## Environment notes

- Host binaries on this box: `codex` `pi` `opencode` `hermes` present, `claude`
  absent — so `harness list` reporting `claude-code available:false` is the
  probe telling the truth, not a broken adapter.
- `crier harness list` probes every adapter's `--version`; 1.30 s for five
  adapters is the cost of that, and it is the only per-invocation floor worth
  noting here (the pushes themselves are ~5 ms).
- The lane stand-in `/home/kara/.hermes/stand-in/dogfood/crier` holds only a
  `board` symlink; all product work was done from the verified checkout/clone.
