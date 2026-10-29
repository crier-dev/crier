# Diagnostics — crier A2A real-use tick 2026-10-29

Target HEAD: ca27c8554ead58d0219f20bf0fec6db27b0e6857.

Scratch runtime: port 18891, SQLite registry/session stores under `/tmp/crier-df-a2a-20261029.sqlite`, `CR_A2A_ENABLED=true`, `CR_GUARD_ENABLED=false`, `CR_REQUIRE_AGENT_SIG=false`. Runtime `/version` reported `0.2.0-215-gca27c855`, commit `ca27c855`, modified=true because the local build tree contains unrelated untracked residue.

Real client probe: `/tmp/crier-df-a2a-probe.sh`; transcript: `/tmp/crier-df-a2a-probe.out`. It registered `a2a-dogfood-agent`, discovered the Agent Card, sent and retried one A2A message, read the task, listed tasks, tested unsupported version/content type, unknown-card behavior, and inbox stats.

Observed task id: `ba06ad008c023750fbc3a37d`. First send was `replay=new`; exact retry was `replay=replayed`, same task id, and `idempotent_replay=true`. Stats were queue_depth=1 and leased_count=0.

Warm timing: `/tmp/crier-df-a2a-warm.sh {#}`; `hyperfine --warmup 3 --runs 20`; 12.3 ms ± 0.6 ms, min 11.1 ms, max 13.1 ms, user 2.8 ms, system 8.9 ms.

Cold timing: `/tmp/crier-df-a2a-cold.sh`; `hyperfine --runs 5`; 197.9 ms ± 6.4 ms, min 190.5 ms, max 205.1 ms, user 101.5 ms, system 72.2 ms. No user-noticeable performance issue; no profile and no PERF row.

Fresh bunker evidence: `/tmp/crier-dogfood-20261029-125920-bunker.jsonl`; agent 046824f2 on bunker-mvp. Harvested agent evidence showed fresh-install OK, upgrade OK, UI probe OK, ci-pass native rc=1 plus act setup-go error (`version: not found`), chaos-disconnect INFO due to unavailable live dependency, chaos-resource INFO, and controller Docker lifecycle SKIPPED. Collect returned `COLLECTED agent=046824f2 rows=20`; post-collect census omitted the agent.

The act/native failure is harness evidence and overlaps QA-CRIER-48/QA-CRIER-7; no new product row was filed. Product source and board state were not modified.
