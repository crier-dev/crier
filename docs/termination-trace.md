# Termination Trace Investigation — DF-CRIER-463

## Finding
Worker proc `proc_c20adf2f9069` (task DF-CRIER-454) was SIGKILLed after
9m59s wall / 1.6G peak. The process result JSON records:
- exit_code: -9
- completion_reason: "exited"  
- termination_source: "" (EMPTY)

## Investigation
EXCLUDED root causes:
- NOT orphan-scope reaper (last reaps 05:48, scope only 10min old, 1.6G < 4GiB cgroup cap)
- NOT gateway restart (active since 2026-10-20 11:56)
- NOT host OOM (46G available, no kernel OOM line)

## Conclusion
The SIGKILL came from an external process. The termination_source should be
set by the Hermes process supervisor, not by crier. This is a crier-board
finding that lives in the infrastructure domain, not the Go service.

## Action Required
Fix in the Hermes agent harness: the background worker supervisor must
attribute the kill source (cgroup reaper, systemd, explicit kill, or "unknown — no supervisor recorded a stop").
