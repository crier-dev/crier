Crier README health sweep — EXECUTION arms transcript
Tick: crier-readme-2026-11-17-16-40-16
Date: 2026-11-17T11:58:58-05:00
Worktree: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip

=== TOOLCHAIN ===
### CMD: go version
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
go version go1.26.11 linux/amd64
### RC: 0

--- go.mod floor ---
### CMD: grep '^go ' go.mod
3:go 1.26.11
### RC: 0
--- README stated floor ---
### CMD: grep -n 'go1.2\|Go 1.2' README.md
378:This path requires Git, GNU Make, and Go 1.26.11 or later. It checks out the
413:- Go 1.26.11 or later
442:curl -LO https://go.dev/dl/go1.26.11.linux-amd64.tar.gz   # linux-arm64 on an ARM machine
443:tar -C "$HOME/sdk" -xzf go1.26.11.linux-amd64.tar.gz      # -> $HOME/sdk/go/bin/go
445:go version                                               # go version go1.26.11 linux/amd64
449:`go/` directory. `go1.26.11` is this repo's minimum; any newer stable release
462:> every command. Captured verbatim on go1.26.11 — it prints your own absolute path
476:> go1.26.11 this is a warning rather than a fatal error (run the two commands above
497:> Measured on go1.26.11: the warning above appears whenever `GOROOT` and `GOPATH`
2889:- **CI** — GitHub Actions, matrix build Go 1.26.11
### RC: 0

=== DOCUMENTED BUILD PATH ===
### CMD: make build build-mcp
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
go build -ldflags "-X github.com/crier-dev/crier/internal/buildinfo.Version=v0.6.1-312-g0aee6438 -X github.com/crier-dev/crier/internal/buildinfo.Commit=0aee643863fcdbeda8c224d9ae90cd18fe81325c -X github.com/crier-dev/crier/internal/buildinfo.BuildTime=2026-11-17T16:58:58Z" -o bin/crier ./cmd/server
### RC: 0

### CMD: ./bin/crier -version
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
crier v0.6.1-312-g0aee6438
### RC: 0

### CMD: ./bin/crier-mcp --version
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
crier-mcp v0.6.1-312-g0aee6438
### RC: 0


=== DOCUMENTED TEST/LINT GATES ===
### CMD: make lint
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
go vet ./...
### RC: 0

### CMD: make release-pin-check VERSION=v0.6.1
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
bash scripts/check-release-pin.sh 'v0.6.1'
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/README.md:334:v0.7.0 != v0.6.1
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/README.md:336:v0.7.0 != v0.6.1
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/README.md:360:v0.7.0 != v0.6.1
FAIL  check-release-pin: 3 of 3 pinned occurrence(s) in /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/README.md do not match v0.6.1 — bump every pin above together with the CHANGELOG entry (docs/releases.md section 1) before cutting this release
make: *** [Makefile:353: release-pin-check] Error 1
### RC: 2

### CMD: make release-pin-check VERSION=v0.7.0
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
bash scripts/check-release-pin.sh 'v0.7.0'
PASS  check-release-pin: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/README.md matches v0.7.0 (3 pinned occurrence(s) across the 3 pin sites)
### RC: 0


=== LINK CHECK (repo-root form) ===
### CMD: python3 /home/kara/.hermes/skills/coding-hermes/coding-hermes-readme/scripts/readme-link-check.py .
### CWD: /home/kara/.hermes/cache/scratch/crier-readme-20261117/tip
line 1610 OK      examples/agent-ecosystem/
line 1637 OK      docs/AGENT-ECOSYSTEM.md
line 1638 OK      specs/AGENT-ECOSYSTEM.md
line 1647 OK      specs/DETECTION.md
line 1946 OK      docs/federated-peer-auth.md
line 1955 OK      specs/NAMESPACES.md
line 2044 OK      docs/service-delivery-auth.md
line 2045 OK      docs/service-delivery-auth.md
line 2048 OK      specs/CRIER-PROTOCOLS.md
line 2049 OK      specs/CRIER-PROTOCOLS.md
line 2103 OK      specs/A2A-OPTION.md
line 2103 OK      specs/A2A-OPTION.md
line 2117 OK      specs/CHAT-PERMISSIONS.md
line 2217 OK      specs/CHAT-PERMISSIONS.md
line 2302 OK      specs/CHAT-ADDRESSING.md
line 2319 OK      specs/CHAT-PERMISSIONS.md
line 2566 OK      specs/CHAT-STORAGE.md
line 2661 OK      docs/openapi.yaml
line 2744 OK      deploy/prometheus-scrape.example.yml
line 2814 OK      docs/architecture.md
line 2815 OK      docs/specs.md
line 2816 OK      docs/mesh-protocol.md
line 2817 OK      docs/openapi.yaml
line 2818 OK      docs/integration-guide.md
line 2819 OK      docs/traceability-doctrine.md
line 2820 OK      docs/federated-peer-auth.md
line 2820 OK      docs/federated-rooms.md
line 2821 OK      docs/AGENT-ECOSYSTEM.md
line 2822 OK      specs/LLM-MESSAGE-GUARD.md
line 2823 OK      specs/A2A-OPTION.md
line 2824 OK      examples/demo.sh
line 2831 OK      AGENTS.md#the-commit-gate--what-it-covers-and-what-it-does-not
line 2899 OK      specs/NAMESPACES.md
line 2906 OK      LICENSE
line 3 200 https://img.shields.io/badge/Go-1.26.11%2B-00ADD8?logo=go  <- https://img.shields.io/badge/Go-1.26.11%2B-00ADD8?logo=go
line 4 200 https://github.com/crier-dev/crier/actions/workflows/ci.yml/badge.svg  <- https://github.com/crier-dev/crier/actions/workflows/ci.yml/badge.svg
line 5 200 https://github.com/crier-dev/crier/actions/workflows/bunker-e2e.yml/badge.svg  <- https://github.com/crier-dev/crier/actions/workflows/bunker-e2e.yml/badge.svg
line 6 200 https://img.shields.io/badge/License-MIT-yellow.svg  <- https://img.shields.io/badge/License-MIT-yellow.svg
line 2103 200 https://a2a-protocol.org/  <- https://a2a-protocol.org
local_missing=0 external_non_200=0
### RC: 0


=== FROM-SOURCE FALLBACK SECTION (README 376-412) ===
### Install both binaries from source (available now)

This path requires Git, GNU Make, and Go 1.26.11 or later. It checks out the
existing `v0.1.0-rc2` tag, uses the repository's `build` and `build-mcp` targets,
and installs both binaries without relying on GitHub release assets:

```bash
git clone --branch v0.1.0-rc2 --depth 1 https://github.com/crier-dev/crier.git
cd crier
make build build-mcp
mkdir -p "$HOME/.local/bin"
install -m 0755 bin/crier bin/crier-mcp "$HOME/.local/bin/"
export PATH="$HOME/.local/bin:$PATH"
```

Confirm what landed:

```bash
crier -version        # crier v0.1.0-rc2-<commit> — the tag's own build identity
crier-mcp --version   # the same build identity
crier -port 8767      # the server; crier -help prints every flag
```

Tester? [TESTERS.md](TESTERS.md) §1 covers the install path. The prerequisites
and expanded build notes below explain how to install Go without root. An
installing-onto-a-fresh-box walkthrough — with the prerequisite chain, the
service units, and what was measured on a bare Debian 13 — is
[INSTALL.md](INSTALL.md); note that this snippet's `--branch v0.1.0-rc2` pin
gives you **that tag's code**, which predates configuration `main` documents
(`CR_SESSION_BACKEND`, for one, does not exist in the `v0.1.0-rc2` tree) — drop
the pin to build what `main` describes. Source
builds carry the same identity as release assets: `scripts/release-artifacts.sh`
stamps `internal/buildinfo` exactly as `make build` and `make build-mcp` do (a
`make docs-check` claim fails if those paths diverge).

### Prerequisites


=== quickstart-adjacent claims ===
### CMD: grep -n 'v0.1.0-rc2' README.md INSTALL.md TESTERS.md docs/releases.md
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/README.md:379:existing `v0.1.0-rc2` tag, uses the repository's `build` and `build-mcp` targets,
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/README.md:383:git clone --branch v0.1.0-rc2 --depth 1 https://github.com/crier-dev/crier.git
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/README.md:394:crier -version        # crier v0.1.0-rc2-<commit> — the tag's own build identity
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/README.md:403:[INSTALL.md](INSTALL.md); note that this snippet's `--branch v0.1.0-rc2` pin
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/README.md:405:(`CR_SESSION_BACKEND`, for one, does not exist in the `v0.1.0-rc2` tree) — drop
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/INSTALL.md:170:> `crier-dev/crier` currently has exactly one release, `v0.1.0-rc2`, and its
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/INSTALL.md:171:> asset list is **empty** (`gh release view v0.1.0-rc2 --json assets` →
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/INSTALL.md:242:-version   : crier v0.1.0-rc2-c34a6c25     (a tag build)  /  d6a8e2a (branch build)
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/INSTALL.md:247:### ⚠️ Do not pin `--branch v0.1.0-rc2` — the README's from-source recipe does
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/INSTALL.md:250:`git clone --branch v0.1.0-rc2 --depth 1 …`. That clones a **tag cut on
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/INSTALL.md:256:| `--branch v0.1.0-rc2` (`c34a6c2`) | **0** | **silently ignored** | **no** |
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/INSTALL.md:463:| `CR_SESSION_BACKEND=sqlite` seems to do nothing | you are running a build from the `v0.1.0-rc2` tag, which predates it | §4: clone the default branch |
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/INSTALL.md:488:make release-artifacts VERSION=v0.1.0-rc2 RELEASE_OUTDIR=/tmp/dist
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/INSTALL.md:489:mkdir -p /tmp/release/latest/download /tmp/release/download/v0.1.0-rc2
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/INSTALL.md:491:cp /tmp/dist/* /tmp/release/download/v0.1.0-rc2/
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/INSTALL.md:494:#   sh install.sh --version v0.1.0-rc2 --base-url http://<host>:18911
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/docs/releases.md:49:make release VERSION=v0.1.0-rc2
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/docs/releases.md:85:git push origin v0.1.0-rc2
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/docs/releases.md:86:git push gitlab v0.1.0-rc2   # only if the gitlab remote is configured
/home/kara/.hermes/cache/scratch/crier-readme-20261117/tip/docs/releases.md:105:a hand duty that can be forgotten (that is exactly how v0.1.0-rc2 shipped with
### RC: 0
=== END ===
