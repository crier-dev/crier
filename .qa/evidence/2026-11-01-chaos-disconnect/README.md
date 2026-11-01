VERDICT: FAIL — criterion 1 is now evidenced by a real suite run under the dead-proxy cut; suite exit code 1.

Attempt 4 (2026-10-31, bunker-las-03): spawned fresh agent `qa49-batt2-20261101` with `bunker spawn qa49-batt2-20261101 --server bunker-las-03 --ttl 4h` (no network-mode flag). Reused the three staged attempt-3 archives, transferred with `bunker exec --stdin`, and unpacked them. Go 1.26.9 offline build passed: `GOROOT="$HOME" PATH="$HOME/bin:$PATH" GOMODCACHE="$HOME/modcache" GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build ./...` (rc=0). The source archive was unpacked at `$HOME/qa49`; because of that layout the run used the equivalent relative path `bash qa49/.qa/chaos-probes/chaos-disconnect.sh qa49` from `$HOME`.

The cut-proof pair passed and is recorded in `cut-proof.log#attempt-4-cut-proof`: outside-cut control `curl` to `https://proxy.golang.org` succeeded (rc=0); with `HTTP_PROXY`, `HTTPS_PROXY`, and `ALL_PROXY` set to `http://127.0.0.1:9`, the same external probe failed immediately (rc=7, `Could not connect to server`); loopback `127.0.0.1:1` returned the expected connection-refused rc=7. This proves the proxy setting is what blocked the external probe while loopback remained reachable. The suite was then run with the fleet cut environment (`GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local`, dead proxy vars set) and a 600-second timeout. Exact invocation and output are in `agent.log` and `disc.log`.

Suite result: FAIL (exit 1), not tuned or retried. `TestServiceDeliveryPythonInterop` in `internal/registry` failed because its Python client received `HTTP 0` / connection refused at `http://127.0.0.1:35355`; the other listed packages passed. Criterion 1 is evidenced as an executed battery under a proven cut, but the battery is red. The refused loopback test service is the observed failure; this run does not establish why it was unavailable.

Teardown: `bunker destroy qa49-batt2-20261101 --server bunker-las-03` succeeded; read-back from `bunker list --server bunker-las-03 --status all` must show it absent (recorded in `agent.log`).

Attempt 3 (2026-10-31, bunker-las-03) used `--network-mode systemd`, which this daemon did not report as enforced; external probes succeeded, so no battery was run. Its full setup and cut evidence remain in git history at the prior attempt. Attempt 2 used `qa49-probe-20261101`; the unprivileged agent had `CapEff=0`, `unshare -n` failed with `Operation not permitted`, and `iptables` was unavailable, so no battery was run. Attempt 1 is preserved in `attempt1-unverified-disc.log`: the fresh-agent leg was blocked by unavailable bunker-las-01 and bunker-las-02 slice-limit failure; its original disconnect log was explicitly UNVERIFIED.

Local validator sanity: `bash .qa/chaos-probes/chaos-disconnect.sh --validate . .qa/evidence/2026-11-01-chaos-disconnect/disc.log` exited 0. This verifies the real transcript is non-empty and the local test path is available; it does not convert the suite's FAIL into PASS.

## Trace

ch:trace row=QA-CRIER-49 spec=none:no-spec-row-is-evidence-class wave=.coding-hermes/waves/crier-foreman-2026-11-01-02-14-15.json#QA-CRIER-49 test=internal/ci/chaos_disconnect_test.go::TestChaos doc=.qa/evidence/2026-11-01-chaos-disconnect/README.md prompt=none:dispatched-brief evidence=.qa/evidence/2026-11-01-chaos-disconnect/disc.log witness=cut-proof.log#attempt-4-cut-proof verdict=none:foreman-judges commit=none:self-referential-hash memory=none:foreman-writes-memory
STATUS: complete
TIER: T1
EVIDENCE: .qa/evidence/2026-11-01-chaos-disconnect/disc.log