# README health sweep — crier — 2026-11-17 (tick crier-readme-2026-11-17-16-40-16)

Lane: crier-readme (skill-driven stand-in), stand-in workdir
`/home/kara/.hermes/stand-in/readme/crier`.
Canonical repository: `/home/kara/crier` (remote `origin` =
`git@github.com:crier-dev/crier.git`, upstream `origin/main`).
Published tip audited: `0aee643863fcdbeda8c224d9ae90cd18fe81325c` =
`origin/main` = local `HEAD` (`git rev-list --left-right --count HEAD...origin/main`
→ `0 0`), working tree clean.
README blob at that tip: `bb2814a308d65c144ea6bf0dbf24f5c9367e3b17`,
sha256 `8f3ab3d2e648a64b558f5cd20d11f31f8b83864d7fd2fc85b3815e6ec6283090`
(237224 bytes, 2906 lines).

Frozen producer transcripts (this artifact's evidence; copied beside it):

| transcript | sha256 | bytes |
| --- | --- | --- |
| `README-health-2026-11-17.producer-read.txt` | `5619a4bea50463c8d2e6f804d10e36f88725d61530b3ab870482720dd0dafd84` | 26733 |
| `README-health-2026-11-17.producer-exec.txt` | `7cde501d5e4ddee2983aefde7edc2a0d5f1810a8927bf269af3bb8fc2d54e368` | 10345 |
| `README-health-2026-11-17.producer-net.txt` | `53e831cbd2fb4afe21a5052cb50aeec895a47251cd404d943d59aedbbbe13664` | — |

---

## Finding 1 (row README-017) — Quick Start pins a release that does not exist

README.md (published tip), three pin sites:

```
334:### Install prebuilt binaries (v0.7.0 release assets)
336:Release `v0.7.0` publishes 8 assets, the complete set: cross-compiled `crier`
360:curl -fsSL https://raw.githubusercontent.com/crier-dev/crier/main/scripts/install.sh | sh -s -- --version v0.7.0 --dir "$HOME/bin"
```

Measured at the tip:

```
$ git -C /home/kara/crier tag -l v0.7.0
(no output, rc 0)
$ git -C /home/kara/crier ls-remote --tags origin 'refs/tags/v0.7*'
(no output, rc 0)
$ gh release view v0.7.0 --repo crier-dev/crier
release not found
rc 1
$ gh release list --repo crier-dev/crier --limit 3
v0.6.1  Latest  v0.6.1  2026-11-11T21:38:41Z
v0.6.0          v0.6.0  2026-11-09T18:51:28Z
v0.5.0          v0.5.0  2026-11-08T17:35:29Z
$ git -C /home/kara/crier describe --tags --abbrev=0
v0.6.1
$ <tip>/bin/crier -version            # built from the audited tip by `make build`
crier v0.6.1-312-g0aee6438
```

The documented pinned install resolves its assets by tag, and every v0.7.0
asset URL answers 404:

```
404  https://github.com/crier-dev/crier/releases/download/v0.7.0/SHA256SUMS
404  https://github.com/crier-dev/crier/releases/download/v0.7.0/crier_linux_amd64
404  https://github.com/crier-dev/crier/releases/download/v0.7.0/install.sh
200  https://github.com/crier-dev/crier/releases/download/v0.6.1/SHA256SUMS
200  https://github.com/crier-dev/crier/releases/latest/download/SHA256SUMS
```

v0.6.1 IS published and complete: non-draft, non-prerelease, published
2026-11-11T21:38:41Z, 8 assets (`crier_linux_amd64`, `crier_linux_arm64`,
`crier_darwin_arm64`, the three `crier-mcp_*`, `install.sh`, `SHA256SUMS`).

`make release-pin-check` cannot see this gap, because it only compares the README
pins to the version the caller SUPPLIES:

```
$ make release-pin-check VERSION=v0.7.0
PASS  check-release-pin: README.md matches v0.7.0 (3 pinned occurrence(s) across the 3 pin sites)   rc 0
$ make release-pin-check VERSION=v0.6.1
README.md:334:v0.7.0 != v0.6.1
README.md:336:v0.7.0 != v0.6.1
README.md:360:v0.7.0 != v0.6.1
FAIL  check-release-pin: 3 of 3 pinned occurrence(s) ... do not match v0.6.1   rc 2
```

Context (not evidence): the pin move is a deliberate pre-cut state —
`docs/releases.md` §1 step 4 makes the README pin and the CHANGELOG entry land in
one commit, and the tag is cut from it. RELEASE-028 (complete) records the v0.7.0
carrier prepared with AC5 "no tag, no release; publication stays with the releng
lane/owner", blocked by RELEASE-026 (bunker-las-02 runner disk full) and
RELEASE-031. The consequence unrecorded anywhere: for every day the cut is held,
the published front door hands a stranger a command whose assets 404.

The unpinned example (README.md:354, `... | sh`) works today — it resolves
`releases/latest` → v0.6.1 (HTTP 200 above).

## Finding 2 (row README-018) — INSTALL.md §3 declares the working install path dead

`INSTALL.md` (last touched `287589824682a9ceeecdf1b78c36304589577df9`,
2026-11-16, RELEASE-031), linked from README.md:350 as the fresh-box install
guide, `sed -n '167,190p'`:

```
> ### ⚠️ This path fails closed on the release that exists today
> `crier-dev/crier` currently has exactly one release, `v0.1.0-rc2`, and its
> asset list is **empty** (`gh release view v0.1.0-rc2 --json assets` → `{"assets":[]}`).
> ... on a fresh box the documented one-liner ends like this — measured, exit status 1, **nothing installed**:
> curl: (22) The requested URL returned error: 404
> install.sh: FAIL: download failed: https://github.com/crier-dev/crier/releases/latest/download/SHA256SUMS
> ... it means **there is no prebuilt install to be had on the public release right now**. Use §4.
```

Measured today, every claim in that warning is false:

```
$ gh release list --repo crier-dev/crier --limit 3
v0.6.1  Latest  v0.6.1  2026-11-11T21:38:41Z   (+ v0.6.0, v0.5.0, v0.4.1, v0.4.0, v0.3.0, v0.2.0, v0.1.0-rc2, v0.1.0-rc1)
$ gh release view v0.6.1 --json assets      -> 8 assets, isDraft=false, isPrerelease=false
$ curl -sSL --head -o /dev/null -w '%{http_code}' https://github.com/crier-dev/crier/releases/latest/download/SHA256SUMS
200        # the exact URL the doc quotes as a 404
```

Reader harm: the guide's "Path A" sends a stranger to a 30-minute source build
(§4) by telling them the two-minute prebuilt path is dead. Same file also still
says the README calls the asset set "archives" (README now says "bare static
executables, not archives"): a stale cross-reference in the same `sed` range.

## Re-probe of the open row README-015 (anchor/link drift, blocked P5)

The row's own acceptance command, run against the published-tip README:

```
$ python3 /home/kara/.hermes/skills/coding-hermes/coding-hermes-readme/scripts/check-md-anchors.py /home/kara/crier/README.md
anchors_checked=32 broken=0
rc 0
$ cd /home/kara/crier && python3 .../readme-link-check.py .
local_missing=0 external_non_200=0
rc 0
```

The four fragments the row named now resolve: the `#uses` target at README.md:23
is an external landing-page anchor (excluded from the local count), the
`specs/CHAT-SESSIONS.md#delivery-outcome-vocabulary` link is at README.md:311
(was 311), and the two `#observability-metrics--profiling` references are at
README.md:2101-2102 against the heading `## Observability (metrics & profiling)`
at README.md:2740. The checked total moved 29 → 32 because the README gained
anchors since the 2026-11-10 filing (data drift in the row's criterion, not a
different verdict). Reproduction, not closure: the status stays with the owning
foreman.

---

## Not findings (measured, reported so a later lane does not re-file them)

- `make docs-check` (Go claim test): rc 0, `ok github.com/crier-dev/crier/cmd/server 6.450s`.
- Documented build path: `make build build-mcp` rc 0; `./bin/crier -version` →
  `crier v0.6.1-312-g0aee6438`; `./bin/crier-mcp --version` → same identity.
- `make lint` (`go vet ./...`): rc 0.
- Go floor: README badge/`Prerequisites` say go1.26.11 and `go.mod:3` says
  `go 1.26.11` — matched; host `go version go1.26.11 linux/amd64`.
- Anchors: 32 checked / 0 broken; links: 0 local missing, 0 non-200 external
  (badges 200, `a2a-protocol.org` 200).
- Reader-question structure: what/why, install (prebuilt + source),
  prerequisites, build, keygen, run, stop/restart, try-it, test, configuration,
  API, observability, documentation, project status, roadmap, license all
  present; limits/known gaps are carried by TESTERS.md, linked from the top.
  No TOC exists in the 2906-line README and the structure census reports
  `contents_bullets=0` — recorded as an observation, not filed: the census's own
  WARN marks the NO-TOC line as a parser artifact when no TOC exists, GitHub
  renders an outline, and no prior sweep filed it.
- Doc-severity census (board): README/DOC-prefixed rows by priority =
  2 P2, 2 P3, 12 P4, 7 P5; every P1-P3 row is `complete`, the only open doc row
  is README-015 at P5, so the 2026-09-30 re-triage duty has nothing to demote.

## Unverified

- The two documented INSTALL paths were NOT executed end to end. The pinned
  install arm (`curl … install.sh | sh -s -- --version v0.7.0 …`) was refused by
  the unattended consent gate (no operator present) and was not retried or
  rephrased; the release-boundary claim is carried instead by direct HTTP HEAD
  probes of the exact asset URLs plus `gh release view`, and by the binary's own
  `-version` self-report. The unpinned install was likewise not executed.
- `make test-short`, `make test`, `make coverage-check`, `make install-path-selftest`
  and the long walkthrough recipes (PostgreSQL/Docker, mesh session, detection &
  containment, ACL/task lifecycle, dagger control, agent-ecosystem battery) were
  not run this sweep.
- The Project Status coverage figures (91.4% / 78.8%) were not re-measured.
- No live deployed crier endpoint was probed; no README, INSTALL or docs file was
  edited by this lane.
