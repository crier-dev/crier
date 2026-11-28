# OPS-CRIER-004 — crier-ci-sink restart loop: orphan adopted, loop frozen

Tick `crier-foreman-2026-11-28-07-13-39`. Host: this box (user systemd).

## Defect (as filed, re-verified live at tick start)

`crier-ci-sink.service` (user unit, `Restart=always`, `RestartSec=3`) reported
`ActiveState=activating / SubState=auto-restart` with `MainPID=0` and
`NRestarts=44745`. Port `:19012` was held by **pid 2927**,
`/usr/bin/python3 /home/kara/bin/crier-ci-sink.py 19012`, `ppid=1`,
cgroup `/hermes-leftover.scope`, started Fri 2026-11-20 19:51:24 — an unmanaged
instance systemd never adopted, so the supervised unit could never bind.

## What was done

1. **Orphan adopted-by-replacement.** `kill -TERM 2927` (exact PID taken from the
   port owner, no pattern match) at 2026-11-28T02:31:56-05:00.
2. The supervised unit's next auto-restart bound the port at **t+4s**:
   `ActiveState=active`, `MainPID=2101902`, and the `:19012` listener pid is
   `2101902` — one process, supervised.
3. Functional probe: `POST /hooks/blocking` → `{"choices":[{"message":
   {"role":"assistant","content":"ECHO: ops-crier-004 probe"}}]}`; `GET /health`
   → `{"status":"ok"}`.
4. **Tracked deploy artifacts** (this commit): `deploy/crier-ci-sink.service`
   (byte-identical to the installed unit), `deploy/crier-ci-sink.service.d/
   10-start-limit.conf` (`StartLimitIntervalSec=600`, `StartLimitBurst=12`), and
   `deploy/install-ci-sink.sh` (idempotent installer, does **not** restart).

## Measurements

| Field | T0 2026-11-28T02:32:04-05:00 | T1 2026-11-28T02:38:39-05:00 |
|---|---|---|
| `ActiveState` | active | active |
| `NRestarts` | 44862 | **44862** (unchanged) |
| `MainPID` | 2101902 | 2101902 |
| `:19012` holder | 2101902 | 2101902 |
| journal `Address already in use` count | 14695 | **14695** (unchanged) |

Pre-fix baseline for the same counters, 2026-11-28T02:25:40-05:00:
`NRestarts=44745`, bind-failure count 14580 — i.e. the counter was advancing
~115 occurrences per 6 minutes while the orphan held the port.

## Residual (owner gate — row left PENDING for this arm)

The start-limit drop-in is **authored and tracked but not installed live**: the
install invocation (`deploy/install-ci-sink.sh` → writes
`~/.config/systemd/user/…` + `systemctl --user daemon-reload`) hit the unattended
approval gate and did not run. Silence is not consent, so it was not retried.

To finish AC2's durable arm:

```
cd <canonical crier checkout> && ./deploy/install-ci-sink.sh
```

Then `systemctl --user show crier-ci-sink.service -p StartLimitIntervalUSec,StartLimitBurst`
should report `10min` / `12`. It applies on the next natural restart; no restart
is required or performed.

Second residual: the ExecStart target `/home/kara/bin/crier-ci-sink.py` still
lives outside this repository (host-local, untracked) — a future row should
either vendor it under `deploy/` or document it as a host fixture.
