CR-GAP-069 — the AGENTS.md paragraph that still has to be applied
=================================================================

The worker tick could NOT write AGENTS.md: writes to agent-instruction files are
approval-gated in this runtime and the approval prompt timed out (silence is not
consent), so the edit was NOT applied and was not retried. Everything else in the
task — the checker, its 14-proof selftest, the Makefile targets and the CI step —
is landed in the commit that added this file's sibling checker.

To apply: insert this paragraph in AGENTS.md, "## The commit gate — what it
covers, and what it does not", immediately AFTER the `make gofmt-selftest`
paragraph ("…and the zero-file refusal below.") and BEFORE the
`make make-docker-selftest` paragraph.

---8<--- paragraph begins ---8<---

`make demo-cleanup-check` is the fourth arm (CR-GAP-069) and is a CI step too:
the previous three read a file's syntax, this one reads a PROCESS LIFE — a
tracked shell script that SPAWNS a crier server must register an EXIT trap that
kills the pid it started AND (when it binds a port) assert after the start that
the port's holder is that pid, because a script that hooks the server with `&`
and no trap leaks the process and the port, which is exactly what the dogfood
federation demo did on `:18767`/`:18877`. Its limits are stated, not implied: it
is STATIC TEXT analysis (it starts no server, signals no process and follows no
run-time-built path — a server started inside a container is out of scope, since
that lifecycle belongs to docker), it classifies a script only on an ACTUAL spawn
— a comment, a heredoc body, an `echo`/`fatal` message, a `grep` pattern, a
`go build`, or the binary's own `-version`/`-help`/`-stop` flags are mentions,
never spawns, so `scripts/bunker-matrix.sh` and `examples/demo.sh`, which only
name the binary, are out of scope by design (a `bash -c '<cmd>'` command string
IS analyzed, one level deep, because what it spawns is spawned by the script) —
its bounded-execution exemption is FORM-based equivalence (a `timeout` on the
spawn or a `timeout … bash -c` launcher executor is accepted as the reaping
mechanism, as in `scripts/check-mcp-stdout.sh`, without proving the timeout
fires), the trap is owed only by a BACKGROUNDED spawn (a foreground run cannot
outlive the script) while the ownership assertion is not waived by that
exemption, and (b) is required to EXIST rather than to run before the spawn.
`make demo-cleanup-selftest` proves the rejection path on fixtures under
`${TMPDIR:-/tmp}` — trap-less, trap-only and bounded-but-unowned shapes, the
mention-only non-classification, the spawn-shape census, every fail-closed rule,
a NEUTER proof, and a regression proof that the five example runners,
`scripts/e2e-battery.sh` and `scripts/check-mcp-stdout.sh` are still accepted.

---8<--- paragraph ends ---8<---
