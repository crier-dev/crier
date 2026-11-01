# Bunker network-isolation runbook (INT-CI-020)

One page. What `bunker spawn --network-mode` does and does not prove, which
servers can enforce what, and the gate every crier QA battery must run before
it claims containment.

**The law: a flag is not a boundary.** `--network-mode systemd` is validated by
the bunker CLI as a MODE NAME only. It rides the spawn RPC as a proto field, so
a daemon that predates the network-isolation surface ignores the unknown field,
returns success and creates an agent in the host network namespace. Nothing in
the CLI's exit code, its output, or the connection bundle distinguishes that
from real containment.

Gate: **`bash scripts/bunker-isolation-gate.sh`** (`verify` / `spawn`), proven
by `bash scripts/bunker-isolation-gate-selftest.sh` (24 hermetic arms, stub
bunker CLI, no host and no network).

---

## 1. Verdicts (exit codes are the contract)

| marker | exit | meaning |
|---|---|---|
| `BUNKER ISOLATION: VERIFIED mode=…` | 0 | the daemon reports the requested mode with a boundary AND the in-agent probe shows the session cannot reach the internet |
| `BUNKER ISOLATION: SKIPPED mode=shared` | 0 | no isolation was requested — nothing to enforce |
| `BUNKER ISOLATION: UNVERIFIED reason=…` | 1 | **fail closed.** Reason classes below |
| `BUNKER ISOLATION: UNVERIFIED-ALLOWED reason=…` | 0 | `--allow-unverified` was passed; loud warning, agent kept |

Reason classes: `daemon-predates-surface`, `daemon-cannot-verify`,
`boundary-missing`, `info-unparsed`, `info-failed`, `mode-mismatch`,
`exec-not-contained`, `egress-probe-skipped`, `probe-failed`,
`probe-tool-missing`, `loopback-missing`, `daemon-unreachable`.

`spawn` additionally **destroys** an agent whose boundary did not verify (unless
`--allow-unverified`), so a refused run never leaves an uncontained agent
behind; if the destroy itself fails the gate says so by name and still exits 1.

```bash
# existing agent, the battery's pre-flight
bash scripts/bunker-isolation-gate.sh verify --agent "$AGENT" --server "$SERVER" --mode systemd

# managed spawn: refuses BEFORE creating anything on a daemon that cannot
# enforce the mode, then verifies what it created
bash scripts/bunker-isolation-gate.sh spawn --agent lab1 --server <server> --mode systemd --ttl 4h
```

## 2. What each mode actually bounds

| mode | enforced by | covers | does NOT cover |
|---|---|---|---|
| `shared` (default) | nothing | — | everything; host network namespace, global port space |
| `systemd` | `--property=PrivateNetwork=yes` on the agent's **dockerd unit** | processes started *by that dockerd* (containers) | **`bunker exec` sessions, SSH, and anything the battery runs over exec** — they keep the host network |
| `procvis` (alias `hidepid`) | `ProtectProc=invisible` on the unit's mount namespace | unit processes' `/proc` view | **not a network boundary at all**; exec sessions keep the host `/proc` |

Two consequences that matter for batteries:

- A battery that runs its probes over `bunker exec` is **outside** the `systemd`
  boundary by the mode's own declaration. Its egress working is expected, and
  the gate reports exactly that as `exec-not-contained` — never as "verified
  because the flag was accepted".
- `procvis` must not be egress-probed; the gate skips the probe for it by
  category (an egress result is not evidence about a `/proc` boundary).

## 3. Per-server isolation support matrix

Probed 2026-11-01 with `bunker status --server <name>` (daemon version and the
`Net mode:` line) and the gate. **Refresh before trusting this table** — it is a
snapshot, not a property of the fleet:

```bash
bunker status --server <name> | grep -E 'Version|Net mode'
bash scripts/bunker-isolation-gate.sh verify --agent <id> --server <name> --mode systemd
```

| server | daemon | reachable | reports a net-isolation surface | `systemd` enforceable for an exec-scoped battery |
|---|---|---|---|---|
| `bunker-las-01` | unknown | **no** (connection refused) | unknown | **no** — offline |
| `bunker-las-02` | 0.1.4 | yes | **no** — "predates network-isolation reporting" | **no** — the flag is silently ignored; spawns additionally fail at `slice-limits` (`memory.swap.max=max`) |
| `bunker-las-03` | 0.1.4 | yes | **no** — "predates network-isolation reporting" | **no** — measured false cut, §4 |
| `bunker-las-04` | unknown | **no** (i/o timeout) | unknown | **no** — offline |
| `bunker-mvp` | 0.2.1 | yes | yes — reports `shared` by default | **no for exec** — with `--network-mode systemd` it reports `systemd` + boundary, but the exec session still has egress (`exec-not-contained`), §4 |
| `cube-las-00` | unknown | **no** (i/o timeout) | unknown | **no** — offline |

Nothing in this snapshot gives a battery loopback-only containment today.
Treat every `--network-mode systemd` spawn on these hosts as **advisory**.

## 4. Live evidence, 2026-11-01 (crier QA, bed for the row)

False cut on a pre-surface daemon (`bunker-las-03`, daemon 0.1.4) — the flag was
accepted and ignored, exactly as reported by QA-CRIER-49 attempt-3:

```
$ bunker spawn intci020-probe1 --server bunker-las-03 --ttl 1h --network-mode systemd
Agent created: intci020-probe1                                    # rc 0, no warning
$ bunker info intci020-probe1 --server bunker-las-03
  Network Isolation: not reported by this daemon — it predates
    network-isolation reporting; the network boundary is NOT verified
$ bunker exec intci020-probe1 --server bunker-las-03 -- \
    curl -sS -o /dev/null -w '%{http_code}' --max-time 10 https://proxy.golang.org
200                                                               # full egress
$ bunker exec intci020-probe1 --server bunker-las-03 -- ip route
default via <lan-gateway> dev … metric 600                        # default route present

$ bash scripts/bunker-isolation-gate.sh spawn --agent intci020-refuse1 \
    --server bunker-las-03 --mode systemd --ttl 1h
BUNKER ISOLATION: UNVERIFIED reason=daemon-predates-surface …      # rc 1
bunker-isolation-gate: refusing to spawn … no agent was created
$ bunker info intci020-refuse1 --server bunker-las-03
bunker: get agent: not_found                                       # nothing created
```

Scope, not enforcement, on a current daemon (`bunker-mvp`, daemon 0.2.1) — this
is the finding that reading the code would miss:

```
$ bash scripts/bunker-isolation-gate.sh spawn --agent intci020-mvp1 \
    --server bunker-mvp --mode systemd --ttl 1h
BUNKER ISOLATION: UNVERIFIED reason=exec-not-contained mode=systemd …
  reported boundary: private network namespace (loopback only): … covers only
    processes in the agent's dockerd unit
  in-agent probe:  egress=reachable loopback=refused-alive
bunker-isolation-gate: destroyed unverified agent 'intci020-mvp1'
```

So the daemon DID stamp the mode; the mode simply does not contain an exec
session. A battery that proves "no egress" by spawning with the flag and then
curling from `bunker exec` is measuring the host, not a boundary.

## 5. Upstream (bunker repo) work this row cannot do from crier

The producer-side fix belongs to the bunker project; these are the exact seams
(file:line against `deployBunker/bunker` HEAD `c893dc2b`, 2026-11-01):

1. **CLI post-spawn read-back** — mirror `internal/cli/spawn_egress_probe.go`
   (DF-BUNKER-82, which refuses a pre-egress daemon before the spawn): after
   `SpawnAgent`, read `GetAgent().NetworkIsolation` and refuse/destroy when the
   requested mode is not what came back. There is no `network_modes_supported`
   capability field on `ServerInfoResponse` today (`proto/bunker/v1/bunker.proto`
   fields 1–13: `default_network_mode`/`default_network_boundary` are the only
   signals) — adding one is the clean capability probe.
2. **Scope-aware reporting** — `netmode.BoundaryFor(systemd)` already says
   "covers only processes in the agent's dockerd unit"; `bunker info` should
   state the exec-session caveat on the same line so no reader assumes the
   agent's shell is contained.
3. **Daemon coverage** — `bunker-las-01..04` run 0.1.4 (or are offline); the
   network-isolation surface is NET-BUNKER-010, and `bunker-las-02` still fails
   spawns at `slice-limits` (`memory.swap.max=max` vs required `0`).

Until (1) lands, **the crier gate is the only thing standing between a battery
and a false cut** — that is why it is wired in below rather than left optional.

## 6. Wiring (already in the repo)

- `scripts/bunker-matrix.sh --network-mode MODE` runs the gate before the first
  cell and aborts the battery when the boundary is not verified. Without the
  flag the matrix behaves exactly as before.
- `.github/workflows/bunker-e2e.yml` runs the gate after the ensure-agent step
  and before the deploy battery (skipped, with a loud note, unless
  `BUNKER_NETWORK_MODE` is set).
- `make bunker-isolation-gate-selftest` — the 24-arm hermetic proof, run in CI.

Never prove containment by the flag value. Prove it with the marker, and only
for the process context the marker was measured in.
