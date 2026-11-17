Crier README health sweep — RELEASE-BOUNDARY / NETWORK transcript
Tick: crier-readme-2026-11-17-16-40-16
Date: 2026-11-17T12:04:03-05:00
Canonical: /home/kara/crier

=== TAG / RELEASE EXISTENCE FOR THE README PIN (v0.7.0) ===
### CMD: git -C /home/kara/crier tag -l v0.7.0

### RC: 0

### CMD: git -C /home/kara/crier ls-remote --tags origin refs/tags/v0.7*

### RC: 0

### CMD: gh release view v0.7.0 --repo crier-dev/crier
release not found
### RC: 1

### CMD: gh release list --repo crier-dev/crier --limit 3
v0.6.1	Latest	v0.6.1	2026-11-11T21:38:41Z
v0.6.0		v0.6.0	2026-11-09T18:51:28Z
v0.5.0		v0.5.0	2026-11-08T17:35:29Z
### RC: 0

### CMD: gh release view v0.6.1 --repo crier-dev/crier --json tagName,isDraft,isPrerelease,publishedAt,assets
{"assets":[{"apiUrl":"https://api.github.com/repos/crier-dev/crier/releases/assets/736995725","contentType":"application/octet-stream","createdAt":"2026-11-11T21:38:39Z","downloadCount":3,"id":"RA_kwDOTWHoA84r7amN","label":"","name":"crier-mcp_darwin_arm64","size":23990850,"state":"uploaded","updatedAt":"2026-11-11T21:38:40Z","url":"https://github.com/crier-dev/crier/releases/download/v0.6.1/crier-mcp_darwin_arm64"},{"apiUrl":"https://api.github.com/repos/crier-dev/crier/releases/assets/736995566","contentType":"application/octet-stream","createdAt":"2026-11-11T21:38:36Z","downloadCount":4,"id":"RA_kwDOTWHoA84r7aju","label":"","name":"crier-mcp_linux_amd64","size":24596642,"state":"uploaded","updatedAt":"2026-11-11T21:38:39Z","url":"https://github.com/crier-dev/crier/releases/download/v0.6.1/crier-mcp_linux_amd64"},{"apiUrl":"https://api.github.com/repos/crier-dev/crier/releases/assets/736995570","contentType":"application/octet-stream","createdAt":"2026-11-11T21:38:36Z","downloadCount":3,"id":"RA_kwDOTWHoA84r7ajy","label":"","name":"crier-mcp_linux_arm64","size":23330978,"state":"uploaded","updatedAt":"2026-11-11T21:38:39Z","url":"https://github.com/crier-dev/crier/releases/download/v0.6.1/crier-mcp_linux_arm64"},{"apiUrl":"https://api.github.com/repos/crier-dev/crier/releases/assets/736995562","contentType":"application/octet-stream","createdAt":"2026-11-11T21:38:36Z","downloadCount":3,"id":"RA_kwDOTWHoA84r7ajq","label":"","name":"crier_darwin_arm64","size":28214050,"state":"uploaded","updatedAt":"2026-11-11T21:38:39Z","url":"https://github.com/crier-dev/crier/releases/download/v0.6.1/crier_darwin_arm64"},{"apiUrl":"https://api.github.com/repos/crier-dev/crier/releases/assets/736995572","contentType":"application/octet-stream","createdAt":"2026-11-11T21:38:36Z","downloadCount":4,"id":"RA_kwDOTWHoA84r7aj0","label":"","name":"crier_linux_amd64","size":29130914,"state":"uploaded","updatedAt":"2026-11-11T21:38:39Z","url":"https://github.com/crier-dev/crier/releases/download/v0.6.1/crier_linux_amd64"},{"apiUrl":"https://api.github.com/repos/crier-dev/crier/releases/assets/736995568","contentType":"application/octet-stream","createdAt":"2026-11-11T21:38:36Z","downloadCount":3,"id":"RA_kwDOTWHoA84r7ajw","label":"","name":"crier_linux_arm64","size":27394210,"state":"uploaded","updatedAt":"2026-11-11T21:38:39Z","url":"https://github.com/crier-dev/crier/releases/download/v0.6.1/crier_linux_arm64"},{"apiUrl":"https://api.github.com/repos/crier-dev/crier/releases/assets/736995731","contentType":"application/x-shellscript","createdAt":"2026-11-11T21:38:39Z","downloadCount":4,"id":"RA_kwDOTWHoA84r7amT","label":"","name":"install.sh","size":11221,"state":"uploaded","updatedAt":"2026-11-11T21:38:39Z","url":"https://github.com/crier-dev/crier/releases/download/v0.6.1/install.sh"},{"apiUrl":"https://api.github.com/repos/crier-dev/crier/releases/assets/736995738","contentType":"application/octet-stream","createdAt":"2026-11-11T21:38:39Z","downloadCount":6,"id":"RA_kwDOTWHoA84r7ama","label":"","name":"SHA256SUMS","size":595,"state":"uploaded","updatedAt":"2026-11-11T21:38:40Z","url":"https://github.com/crier-dev/crier/releases/download/v0.6.1/SHA256SUMS"}],"isDraft":false,"isPrerelease":false,"publishedAt":"2026-11-11T21:38:41Z","tagName":"v0.6.1"}
### RC: 0


=== ASSET URL PROBES (documented install pin resolves or 404s) ===
### CMD: curl -sSL -o /dev/null -w '%{http_code}' --head <url>  (each URL separately)
404  https://github.com/crier-dev/crier/releases/download/v0.7.0/SHA256SUMS
404  https://github.com/crier-dev/crier/releases/download/v0.7.0/crier_linux_amd64
404  https://github.com/crier-dev/crier/releases/download/v0.7.0/install.sh
200  https://github.com/crier-dev/crier/releases/download/v0.6.1/SHA256SUMS
200  https://github.com/crier-dev/crier/releases/latest/download/SHA256SUMS

=== BINARY SELF-REPORT (does a tip build describe a v0.7.0 tag?) ===
### CMD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/bin/crier -version
crier v0.6.1-312-g0aee6438
### RC: 0

### CMD: git -C /home/kara/crier describe --tags --abbrev=0
v0.6.1
### RC: 0


=== OPEN ROW RE-PROBE: README-015 (anchor/link drift) ===
### CMD: python3 /home/kara/.hermes/skills/coding-hermes/coding-hermes-readme/scripts/check-md-anchors.py /home/kara/crier/README.md   (the row's own acceptance command)
anchors_checked=32 broken=0
### RC: 0

### CMD: grep -n '#uses|CHAT-SESSIONS.md|observability-metrics' /home/kara/crier/README.md
23:Meet the mascots → <a href="https://crier-dev.github.io/crier/#uses">builder · home · business · crier</a></i></p>
311:The single [delivery outcome vocabulary](specs/CHAT-SESSIONS.md#delivery-outcome-vocabulary)
2101:| `CR_ENABLE_PPROF` | `false` | Opt-in: register `GET /debug/pprof/` (plus `cmdline`, `profile`, `symbol`, `trace`, `heap`, `goroutine`, `block`, `mutex`, `threadcreate`) for live Go profiling. Default off — unset means the path is not registered and answers `404`. Not auth-exempt: with `CR_AUTH_TOKEN` set it requires the Bearer header like any other authenticated route. See [Observability](#observability-metrics--profiling). |
2102:| `CR_ENABLE_METRICS` | `true` | Register `GET /metrics` serving the Prometheus text exposition format (v0.0.4) — deliveries, webhook outcomes, guard decisions, federation hold depth, relay events, WS subscribers, HTTP requests, the identity/health series (`crier_up`, `crier_build_info{version,commit,dirty}`, `crier_store_healthy{backend}`, `crier_start_time_seconds`), and the live inbox queue (`crier_inbox_queue_depth`, `crier_inbox_queue_leased`, `crier_inbox_queue_oldest_age_seconds`, `crier_inbox_shed_total`; CR-FEAT-035). **On by default** — an unset value registers the surface; set it to `false` (or `0`/`no`) to leave the path unregistered and answering `404`. Not auth-exempt: with `CR_AUTH_TOKEN` set it requires the Bearer header like any other authenticated route. See [Observability](#observability-metrics--profiling). |
2807:ch:trace row=MON-CRIER-007 spec=docs/observability-spec.md#3.1 test=cmd/server/observability_namespace_test.go::TestMetricsLegacyFamiliesRetired doc=README.md#observability-metrics--profiling evidence="curl -s http://127.0.0.1:21251/metrics | grep -cE '^crier_'" prompt=none:worker-brief witness=none:not-pushed verdict=none:pending-tier2 commit=none:worker memory=none:worker
### RC: 0

### CMD: python3 /home/kara/.hermes/skills/coding-hermes/coding-hermes-readme/scripts/readme-link-check.py /home/kara/crier
line 6 200 https://img.shields.io/badge/License-MIT-yellow.svg  <- https://img.shields.io/badge/License-MIT-yellow.svg
line 2103 200 https://a2a-protocol.org/  <- https://a2a-protocol.org
local_missing=0 external_non_200=0

=== INSTALL.md PATH-A CLAIM AS PUBLISHED ===
### CMD: sed -n '165,185p' /home/kara/crier/INSTALL.md

## 3. Path A — install the prebuilt binaries (no toolchain)

> ### ⚠️ This path fails closed on the release that exists today
>
> `crier-dev/crier` currently has exactly one release, `v0.1.0-rc2`, and its
> asset list is **empty** (`gh release view v0.1.0-rc2 --json assets` →
> `{"assets":[]}`). The installer fetches `SHA256SUMS` first and refuses to
> install anything it cannot verify, so on a fresh box the documented one-liner
> ends like this — measured, exit status 1, **nothing installed**:
>
> ```
> ==> download
> curl: (22) The requested URL returned error: 404
> install.sh: FAIL: download failed: https://github.com/crier-dev/crier/releases/latest/download/SHA256SUMS
>        (a 404 here means the tag has no such asset, or the release is a
>        prerelease and so is not 'latest' — pass --version <tag> explicitly;
>        see https://github.com/crier-dev/crier/releases)
> ```
>
> That is the installer working as designed — but it means **there is no

### CMD: git -C /home/kara/crier log -1 --format='%H %ci %s' -- INSTALL.md
287589824682a9ceeecdf1b78c36304589577df9 2026-11-16 22:03:48 -0500 chore(deps): bump Go toolchain to 1.26.11 and remediate 9 govulncheck findings (RELEASE-031)

=== END ===
