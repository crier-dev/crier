# QA-CRIER-60 — timeout-kill assertions vs. a slow reaper

Claim under test: `boardbridge.TestTimeoutKillsProcessGroup` and harness
`TestAdapterContract/{pi,codex}/DeliverTimeoutKillsProcessGroup` are not
stress-resilient — they pass on a clean machine and fail under a memory cap and
a network cut.

## Mechanism (proven, not asserted)

Both assertions decided "the process group is dead" with `kill(pid, 0)` and
treated only `ESRCH` as death:

- base 7458b4d1 `internal/boardbridge/bridge_test.go:178` — `if errors.Is(err, syscall.ESRCH)`
- base 7458b4d1 `internal/harness/contract_test.go:1063` — `err := syscall.Kill(pid, 0)`

A child that has been killed but NOT yet reaped by its parent stays in the
process table in state `Z`, and `kill(pid, 0)` returns `nil` for it. Probe run on
this host (`scratch/zzombie`):

```
pid=4185908 state=Z kill0_err=<nil> (nil == pre-fix code says ALIVE)
```

So under load, when the reaper is slow (memory cap) or the adapter's own cleanup
is delayed (network cut), the pre-fix test observes a zombie and reports a
surviving process group. The failure is in the observation, not in the kill path.

## Fix (merged)

`waitDead()` / the boardbridge equivalent now poll `/proc/<pid>/stat` and accept
state `Z` (or `ErrNotExist`) as dead, with a bounded budget; the failure text
reports the observed state, the stat error and the budget, so a genuinely live
group still fails loudly.

## Evidence runs

| # | tree | command | result |
|---|------|---------|--------|
| 1 | base 7458b4d1 | `systemd-run --user --scope -p MemoryMax=3G -p MemorySwapMax=0 go test -count=1 -run TestTimeoutKillsProcessGroup ./internal/boardbridge/` | **ok 5.017s — FAILURE NOT REPRODUCED** |
| 2 | base 7458b4d1 | same scope, `-run 'TestAdapterContract/(pi|codex)/DeliverTimeoutKillsProcessGroup' ./internal/harness/` | **ok 0.836s — FAILURE NOT REPRODUCED** |
| 3 | merged edab98f8 | same scope, → `./internal/boardbridge/` | ok 5.067s |
| 4 | merged edab98f8 | same scope, `-run TestAdapterContract ./internal/harness/` | ok 7.853s |

Clean vs stressed agreement: the two legs agree at both trees (both pass). The
stress legs did not force the flake on this host, so the fix rests on the
mechanism proof above, not on a captured red.

## Not reproduced / open

- The **network-cut** leg of the original claim was not reproduced: this host
  cannot create a network namespace for the test (no `unshare` privilege), so the
  cut was not injected. Recorded as a named gap, not a pass.
- The memory-cap leg ran and did not red at the base commit.

Verdict on the mechanism: the adapter kill path does **not** leave a live process
group — it leaves an unreaped zombie, which the pre-fix assertion misread as
alive.
