# Bunker network-isolation runbook (INT-CI-020)

One page. What `bunker spawn --network-mode` does and does not prove, which
servers can enforce what, and the gate every crier QA battery must run before
it claims containment.

**The law: a flag is not a boundary.** Bunker CLI builds that predate INT-CI-020
validate `--network-mode systemd` as a MODE NAME only. The flag rides the spawn
RPC as a proto field, so a daemon that predates the network-isolation surface
ignores the unknown field, returns success and creates an agent in the host
network namespace. On those builds nothing in the CLI's exit code, its output, or
the connection bundle distinguishes that from real containment. (A build with
INT-CI-020 refuses BEFORE the RPC when the daemon cannot report the mode, and
destroys the agent when the daemon's own record does not come back verified — see
§5. Deployed hosts still run 0.1.4/0.2.x daemons, so the gate below remains the
contract for now.)

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

Probed 2026-11-07 from the operator workstation over the configured server set.
**Refresh before trusting this table and quote the date you probed** — it is a
snapshot of a moving fleet, not a property of bunker:

```bash
for s in $(grep '    name:' ~/.bunker/config.yaml | awk '{print $3}'); do
  out=$(timeout 25 bunker status --server "$s" 2>&1)
  printf '%s | %s | %s\n' "$s" \
    "$(printf '%s' "$out" | grep -m1 'Version:' | sed 's/^ *//')" \
    "$(printf '%s' "$out" | grep -m1 'Net mode:' | sed 's/^ *//')"
done
# then for one live agent per host:
bash scripts/bunker-isolation-gate.sh verify --agent <id> --server <name> --mode systemd
```

| server | daemon | reachable | reports a net-isolation surface | `systemd` enforceable for an exec-scoped battery |
|---|---|---|---|---|
| `bunker-las-01` | — | **no** (connection refused) | — | **no** — offline |
| `bunker-las-02` | 0.1.4 | yes | **no** — "predates network-isolation reporting" | **no** — the flag is silently ignored; spawns additionally fail at `slice-limits` (`memory.swap.max=max`, measured 2026-11-01) |
| `bunker-las-03` | — | **no** (connection refused, 2026-11-07) | — | **no** — offline; this was the 2026-11-01 false-cut host, §4 |
| `bunker-las-04` | — | **no** (i/o timeout) | — | **no** — offline |
| `cube-las-00` | — | **no** (i/o timeout) | — | **no** — offline |
| `hermes-fleet-01` | — | **no** (i/o timeout) | — | **no** — offline |
| `bunker-mvp` | 0.2.1 | yes | yes — reports `shared` by default | **no for exec** — with `--network-mode systemd` it reports the mode + boundary, but the exec session still has egress (`exec-not-contained`), §4 |
| `dedi-2` | 0.2.1 | yes | yes — `shared` | **no for exec** — same dockerd-unit scope caveat as `bunker-mvp` |
| `judgebox` | 0.2.1 | yes | yes — `shared` | **no for exec** — same dockerd-unit scope caveat |
| `hermes-fleet-02` | 0.1.4 | yes | **no** — "predates network-isolation reporting" | **no** — silent no-op |
| `hermes-fleet-03` | 0.1.4 | yes | **no** | **no** — silent no-op |
| `hermes-fleet-04` | 0.1.4 | yes | **no** | **no** — silent no-op |
| `hermes-fleet-05` | 0.1.4 | yes | **no** | **no** — silent no-op |
| `hermes4friends` | 0.1.4 | yes | **no** — 4 running agent homes all read "not verified" | **no** — measured refusal, §4 |

Totals on that probe: **3 of 14** configured servers report the surface at all
(all 0.2.1, all defaulting to `shared`), **6 are 0.1.4** and ignore the flag,
**5 are unreachable**. Nothing in this snapshot gives a battery loopback-only
containment for an `exec`-scoped probe. Treat every `--network-mode systemd`
spawn on these hosts as **advisory**, and prove the boundary with the gate.

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

### Re-probe, 2026-11-07 (the row's host is gone; the refusal is not)

`bunker-las-03` now refuses connections, so the original false cut cannot be
re-run there. The same fail-closed verdict was re-measured on another
reachable pre-surface daemon, `hermes4friends` (0.1.4, 4 live agent homes):

```
$ bash scripts/bunker-isolation-gate.sh verify --agent luisa-sossa \
    --server hermes4friends --mode systemd --no-egress-probe
BUNKER ISOLATION: UNVERIFIED reason=daemon-predates-surface mode=systemd \
  agent=luisa-sossa server=hermes4friends                          # rc 1

$ bash scripts/bunker-isolation-gate.sh spawn --agent intci020-refuse-20261107 \
    --server hermes4friends --mode systemd --ttl 10m
BUNKER ISOLATION: UNVERIFIED reason=daemon-predates-surface mode=systemd …
bunker-isolation-gate: refusing to spawn with --network-mode systemd — no agent
  was created.                                       # rc 1, refused BEFORE spawn

$ bunker info intci020-refuse-20261107 --server hermes4friends
bunker: get agent: not_found: agent "intci020-refuse-20261107" not found
```

That is the consumer-side contract intact on today's fleet: the flag is never
trusted, and a refused spawn leaves no uncontained agent behind. It does **not**
make `bunker spawn --network-mode systemd` itself safe on a deployed host — the
producer-side fix landed on bunker branch `wt/INT-CI-020` and is not deployed
yet; see §5.

## 5. Upstream (bunker repo) work — landed, plus what is still missing

The producer-side fix belongs to the bunker project. Seams 1–2 were re-verified
against bunker HEAD `047afeb9` on 2026-11-07 (`internal/cli/spawn.go` and
`spawn_egress_probe.go` carried **zero** `NetworkIsolation` references and there
was no capability field), and the fix has since LANDED on bunker branch
`wt/INT-CI-020`:

- `44881051` `fix(bunker): enforce --network-mode systemd or reject clearly (INT-CI-020)`
- `fceb1080` `docs(changelog): record the INT-CI-020 network-mode enforcement`

What that branch does (each layer fails closed):

1. **Capability probe, BEFORE the spawn** — `ServerInfoResponse` gains
   `repeated string network_modes_supported = 14` (re-exported from
   `internal/netmode`; empty = the daemon predates enforcement, never "all
   supported"). `ensureDaemonSupportsNetworkMode` refuses an enforced
   `--network-mode` against a daemon that does not report it, with an error
   naming the consequence (agent created in the host network namespace) and the
   upgrade path. This is the arm that protects today's fleet: it works against
   an already-deployed 0.1.x daemon without touching the host. `shared` and an
   unset flag stay inert so pre-existing invocations are byte-identical.
2. **Post-spawn read-back** — the spawn response carries no agent summary, so
   the CLI reads `GetAgent().NetworkIsolation` and requires the canonical
   requested mode (hidepid→procvis via `config.CanonicalNetworkMode`), a
   boundary line and `verified`; otherwise it DESTROYS the agent and fails
   (`spawn_netmode_probe.go`).
3. **Daemon read-back** — after the dockerd unit is up the daemon reads
   `PrivateNetwork=yes` / `ProtectProc=invisible` back from the RUNNING unit
   with a detached, bounded `systemctl show` and fails the spawn at
   `StageNetworkReadback` on a mismatch, so only a verified boundary is
   recorded (`network_verified`) or reported. `bunker info` renders
   `[verified: …]` / `[NOT verified: …]` on the existing
   `Network Isolation: <mode> — <boundary>` line (the prefix this repo's gate
   parses is unchanged).

### Live proof of the refuse arm (2026-11-07: new CLI, pre-surface daemon)

Built from branch `wt/INT-CI-020` and run against `hermes4friends` (0.1.4, four
live agents), the enforced mode is now refused before anything is created:

```
$ bunker status --server hermes4friends | grep 'Net mode'
  Net mode: not reported by this daemon — it predates network-isolation reporting

$ bunker spawn intci020-live-refuse-20261107 --server hermes4friends \
    --network-mode systemd --ttl 10m
bunker: refusing to spawn with --network-mode "systemd": the daemon at this
server predates network-isolation enforcement (ServerInfo carries no
network-mode capability) and would silently ignore the flag — the agent would
be created in the HOST network namespace with full egress while this command
reported success. Upgrade bunkerd … or drop the flag          # rc 1

$ bunker info intci020-live-refuse-20261107 --server hermes4friends
bunker: get agent: not_found: agent "intci020-live-refuse-20261107" not found

$ bunker list --server hermes4friends | tail -1
Total: 4 agents (server: hermes4friends)      # 4 before, 4 after — nothing created
```

**Deploy order:** a CLI and a daemon from this branch refuse on a mixed pair.
Upgrade `bunkerd` and `bunker` together (`/opt/bunker` + `/usr/local/bin`, the
redeploy recipe): a new CLI against an older daemon refuses every enforced
`--network-mode`, while an older CLI against a newer daemon behaves as before.
The positive arm (mode applied and read back) needs a daemon from this branch;
it is proven in-tree, not yet on a host.

Still open:

- **Deploy** — no host runs the branch yet; today's fleet is 0.1.4/0.2.x
  daemons with older CLIs, so §1–§4 (this repo's gate) remain the contract.
- **Scope-aware reporting (was seam 2)** — `netmode.BoundaryFor(systemd)`
  already says "covers only processes in the agent's dockerd unit", but
  `bunker info` still does not print the exec-session caveat on the same line
  as the verification clause. A battery reading `info` alone can still assume
  the agent's shell is contained; the gate's `exec-not-contained` verdict is
  what states it today.
- **Host coverage** — `bunker-las-01..04` run 0.1.4 (or are offline), and
  `bunker-las-02` still fails spawns at `slice-limits`
  (`memory.swap.max=max` vs required `0`).

Until a host runs a daemon and CLI with these layers, **the crier gate is the
only thing standing between a battery and a false cut** — that is why it is
wired in below rather than left optional.

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
