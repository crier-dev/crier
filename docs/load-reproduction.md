# Load reproduction — the bounded harness (DF-CRIER-254)

Some flake classes only reproduce **under load** ("fails 4/24 runs on a loaded box" —
DF-CRIER-252 is exactly that). This document is the rule and the tool for that work.

## The incident this replaces

A load-dependent reproduction was driven with an ad-hoc shell counter loop that
backgrounded one burn unit per iteration, e.g. a `for` loop spawning
`timeout 300 nice -n 0 sh -c '<shell no-op loop>'` per iteration. Nothing owned those
units: when the spawner exited they were reparented to the user systemd manager
(`systemd --user`) and outlived every caller. Measured on 2026-09-18: 73 -> 278
concurrent burn units, each burning ~13% CPU, ~2000 processes, loadavg 220/346/237,
and the shared host — which also runs the Hermes gateway, the cron scheduler and
DuckBrain — was unusable for hours. Every judge run that re-read the criterion
respawned them, because the criterion asked for verification on a loaded box.

A repro harness that can outlive its own process is not a test. It is an incident.

## The rule

- **Never** hand-roll a busy-wait / burn loop inline in a command line, and never
  `setsid` one.
- Use `scripts/load-repro.sh` (or `scripts/loadgen.py` directly). Both are bounded,
  self-cleaning and refuse to make a busy host busier.
- Never kill by `pkill -f` / `pgrep -f` with a pattern that also appears in your own
  argv: the pattern matches the caller and you kill yourself.
- Load-class work is serialized per host (the wrapper takes a lock); do not run two
  load reproductions at once.

## Usage

```bash
# bounded load while a target command runs; exits with the TARGET's exit code
scripts/load-repro.sh --workers 2 --seconds 30 -- go test ./cmd/server -count=1

# above the load threshold the run is skipped, with the reason recorded
scripts/load-repro.sh --workers 2 --seconds 30 -- make test
# -> SKIPPED: 1-minute loadavg 21.40 is above the threshold 8.00 — refusing to add
#    load to a host that is already loaded.   (exit 3)

# the generator on its own, machine-readable
python3 scripts/loadgen.py --workers 4 --seconds 60 --cpus 0-3 --json
```

| knob | default | hard cap | refusal |
| --- | --- | --- | --- |
| `--workers` | 4 | **8** | exit 2, message names the cap |
| `--seconds` | 30 | **300** | exit 2, message names the cap |
| `--load-threshold` | **8.0** | — | above it: exit 3, `SKIPPED: …` naming the measured 1-minute loadavg |
| `--loadavg-file` | `/proc/loadavg` | — | override used by the tests to pin a synthetic value |
| `--cpus` | unset | host's allowed set | exit 2 naming the CPUs outside it |

Exit codes: `0` ok · `1` a child SURVIVED teardown (also wins over 128+signum) ·
`2` refusal/misuse (out-of-cap, bad `--cpus`, unreadable loadavg file — fail closed,
an unreadable figure is never read as "idle") · `3` the load gate skipped the run ·
`128+signum` interrupted, children still torn down.

## Why nothing can outlive its run

1. **Kernel-level parent death.** Every burner arms
   `prctl(PR_SET_PDEATHSIG, SIGKILL)`, so even a `SIGKILL` of the runner — the case no
   `finally`/`atexit` can cover — kills the children in the kernel. A child that finds
   its parent already gone exits instead of spinning.
2. **Owned teardown.** `TERM -> join(5) -> KILL -> VERIFY`: the runner verifies every
   started pid is gone and exits 1 naming any survivor.
3. **Interruption is covered.** `scripts/load-repro.sh` traps `EXIT/INT/TERM/HUP` and
   tears its generator, its burners and its target down, then prints one machine-readable
   summary line (`load-repro: {...}`) recording the signal and whether the run was clean.
4. **No detachment.** No `setsid`, no reparenting window: the children are owned by the
   process that started them, for their whole life.

## Proving it, and keeping the proof honest

```bash
make load-repro-selftest     # also a CI step
```

The selftest asserts each property above — cap refusals, the load-gate skip driven by a
synthetic `--loadavg-file`, zero survivors after a normal exit, zero survivors after a
`SIGKILL` of the runner mid-run, the wrapper's signal teardown, the lock refusing a
second concurrent run, and a **NEUTER proof** (a copy of the survivor check with its
verdict forced to success must accept a live burner, so a green reading cannot be
vacuous). Every fixture run is `<= 2 workers`; the generator window is `2 s` except in
assertion 11, which uses `10 s` so its mid-flight premise (generator alive with >= 2
burners AND the wrapper's target alive) stays observable — the premise itself is still
asserted, so the wider window is not a weaker test. The selftest never loads the box
and never leaves a burner behind, even when it is interrupted.

The selftest finds those processes with **one whole-machine snapshot per poll
iteration** — a single `python3` invocation over `/proc/*/stat` — and never one fork per
pid. The fork-per-pid shape it replaced cost 6.4–20.6 s per pass on a 1672-pid box (and
166.8 s once under load), so a single poll could outlive the fixture it was observing.
