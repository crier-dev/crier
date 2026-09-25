.PHONY: help build build-mcp mcp test test-short test-integration lint run stop clean docker-build coverage coverage-html coverage-check docs-check generate port-guard-selftest scratch-port-rotation-selftest transport-retry-selftest load-repro-selftest bunker-matrix-selftest shell-yaml-check shell-yaml-selftest install-hooks make-docker-check make-docker-selftest gofmt-check gofmt-selftest demo-cleanup-check demo-cleanup-selftest orphan-sweep-check heredoc-lint heredoc-lint-selftest mcp-stdout-check mcp-stdout-selftest judge-diff-class-selftest parity-check parity-selftest release

# Default pidfile pairing `make run` with `make stop` (DF-CRIER-194). It
# lives at the repo root, is written only after the port is bound, and is
# removed on graceful shutdown; override with PIDFILE= (or CR_PIDFILE).
PIDFILE ?= .crier.pid

# Build identity. The linker stamps internal/buildinfo, which both binaries
# (cmd/server and cmd/crier-mcp) read — one identity, one format, so the CLI,
# GET /version and the startup log can never disagree (DF-CRIER-127).
#
# VERSION defaults to the git description, so even a bare `make build` carries
# a real identity instead of the "dev" placeholder; a build with no tags
# resolves to the short commit. Override any of them at build time:
#   make build VERSION=1.2.3
# A bare `go build` (no ldflags) is still covered: internal/buildinfo falls
# back to the vcs.revision/vcs.time/vcs.modified metadata the Go toolchain
# records for any main package built inside a git checkout.
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

BUILDINFO_PKG := github.com/crier-dev/crier/internal/buildinfo
CRIER_LDFLAGS := -X $(BUILDINFO_PKG).Version=$(VERSION) -X $(BUILDINFO_PKG).Commit=$(COMMIT) -X $(BUILDINFO_PKG).BuildTime=$(BUILD_TIME)

# Default target (first target in the file) — bare `make` shows the menu.
help:
	@echo "Available targets:"
	@echo "  build             Compile the main server binary into bin/crier"
	@echo "  build-mcp         Compile the MCP server binary into bin/crier-mcp"
	@echo "  mcp               Build and run the MCP bridge in one command (stdio; the documented launcher — its stdout carries only JSON-RPC frames)"
	@echo "  run               Build and run the server (default :8767, pidfile $(PIDFILE))"
	@echo "  stop              Stop the server started by make run (reads $(PIDFILE); no pidfile = nothing to stop, exit 0)"
	@echo "  test              Full test suite, no caching (go test ./... -count=1 -timeout 60s)"
	@echo "  test-short        Unit tests only — skips integration, no Docker required"
	@echo "  test-integration  PostgreSQL-backed registry tests (requires Docker)"
	@echo "  lint              Static analysis (go vet ./...)"
	@echo "  coverage          Run tests and print total coverage"
	@echo "  coverage-html     Generate coverage.html"
	@echo "  coverage-check    Fail if coverage is below the 70% threshold"
	@echo "  docs-check        Execute prose claims in docs/claims.yaml against the live server — prose drift fails the build (CR-GAP-055)"
	@echo "  port-guard-selftest  Exercise the demo-harness port guards on a self-picked free port (QA-CRIER-9)"
	@echo "  scratch-port-rotation-selftest  Prove every example runner CHOOSES its scratch port (rotation, exhaustion, explicit override) — no provider (QA-CRIER-10)"
	@echo "  transport-retry-selftest  Exercise the deploy-leg transport retry/classifier on PATH shims — no host, no docker (INT-CI-001)"
	@echo "  load-repro-selftest  Prove the bounded load harness (caps, load gate, PDEATHSIG teardown, no survivors) — DF-CRIER-254"
	@echo "  bunker-matrix-selftest  Prove the bunker-matrix argument surface, its --local safety and its remote refusal — no docker, no bunker, no network (DF-CRIER-81)"
	@echo "  shell-yaml-check  Check every tracked shell script (bash -n) and .github/workflows/*.yml (actionlint, or the PyYAML fallback) — DF-CRIER-206"
	@echo "  shell-yaml-selftest  Prove that checker still rejects broken shell/YAML and accepts a clean pair (DF-CRIER-206)"
	@echo "  make-docker-check  Check every tracked Makefile (make -n dry-parse) and Dockerfile (hadolint, or the built-in python3 parse) — DF-CRIER-209"
	@echo "  make-docker-selftest  Prove that checker still rejects a broken Makefile/malformed Dockerfile and accepts a clean set (DF-CRIER-209)"
	@echo "  gofmt-check       Check every tracked .go file with gofmt (go vet does not read formatting) — a drifting file fails (DF-CRIER-189)"
	@echo "  gofmt-selftest    Prove that checker still rejects a drifting .go file and accepts a clean one, incl. a neuter proof (DF-CRIER-189)"
	@echo "  demo-cleanup-check  Fail when a tracked shell script spawns a crier server without an EXIT-trap cleanup + port-ownership assertion (CR-GAP-069)"
	@echo "  demo-cleanup-selftest  Prove that checker still rejects a trap-less / unowned server spawn and accepts the real tree, incl. a neuter proof (CR-GAP-069)"
	@echo "  orphan-sweep-check  Prove the tick-start orphan sweep still classifies dogfood-* / /tmp-compose containers and in-range port holders, fails closed on a missing/failing tool, and never invokes a mutating verb — with stub docker/ss, so it needs neither (DF-CRIER-281)"
	@echo "  parity-check      Assert the primary remote (origin) and the content mirror (gitlab) carry the same main — exact 0/0 or a loud failure naming both counts and the fix (REV5-CRIER-001)"
	@echo "  parity-selftest   Prove that checker still accepts parity and rejects mirror-behind, primary-behind, dual lineage, a missing remote, a branchless remote and an unreachable remote, incl. a neuter proof (REV5-CRIER-001)"
	@echo "  mcp-stdout-check  Run the documented MCP launcher(s) and prove their stdout carries only JSON-RPC frames, never make's recipe echo or build output (DF-CRIER-137)"
	@echo "  mcp-stdout-selftest  Prove that checker still rejects a launcher that contaminates stdout and refuses one that prints nothing, incl. a neuter proof (DF-CRIER-137)"
	@echo "  judge-diff-class-selftest  Prove the board-only diff classifier that authorizes gitreins --skip-tier2: verdicts, strict gate, empty-diff refusal, unknown flags, and a neuter proof (DF-CRIER-278)"
	@echo "  install-hooks     Install scripts/hooks/pre-commit into .git/hooks (idempotent) so a green commit states its scope (DF-CRIER-206)"
	@echo "  release           Cut a release: check clean tree + main, run the gates, create the annotated tag VERSION — never pushes (RELEASE-001; make release VERSION=v0.1.0-rc2, see docs/releases.md)"
	@echo "  clean             Remove built binaries"
	@echo "  docker-build      Build crier and crier-mcp Docker images"
	@echo "  generate          Run go generate ./..."
	@echo ""
	@echo "Config is env-driven (CRIER_PORT, CR_DATABASE_URL, CR_AUTH_TOKEN, CR_REQUIRE_AGENT_SIG) — see README.md."
	@echo "The server binary also takes CLI flags: ./bin/crier --help prints full usage, -port overrides CRIER_PORT, -db-url overrides CR_DATABASE_URL, -version prints the build identity (version, commit, dirty marker)."

build:
	go build -ldflags "$(CRIER_LDFLAGS)" -o bin/crier ./cmd/server

# DF-CRIER-137: the `@` keeps make from echoing this recipe line to STDOUT. The
# documented launcher `make build-mcp && ./bin/crier-mcp` feeds a strict MCP stdio
# client, and without the `@` make's recipe echo landed on that client's stdout
# before any JSON-RPC frame (measured; the same finding was filed as DF-CRIER-66,
# DF-CRIER-91 and DF-CRIER-137). Build FAILURES still print — make's own error
# line and go's compiler output go to stderr — and the exit status is unchanged.
# `make mcp-stdout-check` gates this; do not drop the `@` or re-prefix the recipe.
build-mcp:
	@go build -ldflags "$(CRIER_LDFLAGS)" -o bin/crier-mcp ./cmd/crier-mcp

# DF-CRIER-137: the same launcher as ONE command, so a client config does not
# need a shell `&&`. Each recipe line runs in its own shell, so `exec` replaces
# that shell and the bridge inherits make's stdin/stdout/stderr; a failed build
# stops make before the exec line (and its output stays on stderr). This is
# additive: `build-mcp` and the compound documented form keep working.
mcp: build-mcp
	@exec ./bin/crier-mcp

test:
	go test ./... -count=1 -timeout 60s

test-short:
	go test -short ./...

test-integration:
	go test -tags=integration -count=1 -timeout 5m ./internal/registry

lint:
	go vet ./...

run: build
	./bin/crier -pidfile $(PIDFILE)

stop:
	./bin/crier -stop -pidfile $(PIDFILE)

clean:
	rm -rf bin/

docker-build:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg BUILD_TIME=$(BUILD_TIME) -t crier:latest .
	docker build -f Dockerfile.mcp --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg BUILD_TIME=$(BUILD_TIME) -t crier-mcp:latest .

coverage:
	go test -short -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

coverage-html: coverage
	go tool cover -html=coverage.out -o coverage.html

coverage-check:
	@go test -short -count=1 -coverprofile=coverage.out ./... > /dev/null 2>&1; \
	COVERAGE=$$(go tool cover -func=coverage.out | grep '^total:' | awk '{print $$3}' | sed 's/%//'); \
	echo "Total coverage: $${COVERAGE}%"; \
	if [ "$$(echo "$${COVERAGE} < 70.0" | bc)" = "1" ]; then \
		echo "FAIL: Coverage $${COVERAGE}% is below 70% threshold"; \
		exit 1; \
	else \
		echo "PASS: Coverage $${COVERAGE}% meets 70% threshold"; \
	fi

docs-check:
	go test -short -count=1 -run 'TestDocsClaims' ./cmd/server

# QA-CRIER-9: the example harnesses refuse to run while their scratch port is
# occupied and assert that the process answering /health is the one they started.
# The selftest picks its own free port, so a busy runner cannot make it flake.
port-guard-selftest:
	bash scripts/lib/port-guard.sh --selftest

# QA-CRIER-10: every example runner that starts a scratch service must CHOOSE its
# port (bounded candidate rotation, holder evidence for every candidate skipped, a
# named failure when they are all occupied, a caller-named port checked and never
# rotated) instead of binding one fixed default. `make port-guard-selftest` proves
# the selector's own contract; this target proves the RUNNERS are wired to it:
# source invariants for all five (llm-mesh bridge + raw, federation-demo,
# hermes-gateway-demo, ws-mesh-demo) plus live rotate / exhaust / explicit arms for
# the three runners that had no such test — no provider, no key, no network. The
# ws-mesh runner's live arms also run in its Go suite (`go test ./examples/ws-mesh-demo/`),
# which `make test` executes.
scratch-port-rotation-selftest:
	python3 examples/scratch-port-rotation-selftest.py

# INT-CI-001: one transient ssh/scp reset on the deploy leg used to kill the whole
# bunker-e2e battery with no retry and no attribution (CI run 35302932314). This
# exercises scripts/lib/transport-retry.sh on PATH shims only — no ssh, no scp, no
# docker, no bunker host, no network — and proves each arm by COUNTING the shim's
# calls, so it cannot flake and cannot pass vacuously: a neuter proof (transient
# predicate forced false) must make ARM A fail, and restoring it must make ARM A
# pass again.
transport-retry-selftest:
	bash scripts/lib/transport-retry.sh --selftest

# DF-CRIER-254: the repo had no bounded way to put synthetic load on a box, so a
# load-dependent-flake reproduction was driven with an ad-hoc counter loop that
# backgrounded one burn unit per iteration and owned none of them: the units
# reparented to the user systemd manager and outlived every caller (73 -> 278
# concurrent burners, loadavg 220/346/237, the shared host unusable for hours).
# scripts/loadgen.py is the sanctioned replacement (hard caps, a load-average
# gate, PDEATHSIG + verified teardown), scripts/load-repro.sh wraps it around a
# target command, and this selftest proves each property — including a NEUTER
# proof, so the no-survivor reading cannot be vacuous. Every fixture run is
# <= 2 workers / <= 2 s and never leaves a burner behind.
load-repro-selftest:
	bash scripts/load-repro-selftest.sh

# DF-CRIER-81: scripts/bunker-matrix.sh gained --help/--local and a remote
# preflight, but its argument surface and the local mode's safety claims (never
# deploys, never calls the bunker CLI, probes the supplied host/port) are only
# real if they are measured. This selftest drives a SANDBOX copy of the matrix
# with a poisoned bunker-deploy.sh (canary + exit 42), a poisoned bunker CLI and
# a local fake crier (python3 stdlib http.server) — no docker, no bunker host,
# no network, fully deterministic.
bunker-matrix-selftest:
	bash scripts/bunker-matrix-selftest.sh

# DF-CRIER-206: the Tier-1 guard battery (secrets/go_build/go_lint/go_tests) never
# reads a shell script or a workflow YAML, so a .sh/.yml-only diff used to get a
# `Tier 1 Guards: PASS` that verified nothing about it. shell-yaml-check checks
# every TRACKED shell script (bash -n) and every .github/workflows/*.yml|*.yaml
# (actionlint when available, otherwise the PyYAML parse fallback — the mode and
# tool version are printed, never a silent skip). shell-yaml-selftest proves the
# checker still rejects a broken script and a malformed workflow and accepts a
# clean pair, on fixtures it creates under ${TMPDIR:-/tmp}.
# DF-CRIER-208: given an EXPLICIT file list the checker fails closed — a named
# *.yml/*.yaml outside .github/workflows/, or a list in which nothing classifies
# as shell or workflow, exits 1 with the paths named instead of reporting a green
# over files it never read. The default (no-argument) whole-repo mode is unchanged,
# and the pre-commit wrapper's list is always already-classified, so neither rule
# is reachable from the gate.
shell-yaml-check:
	bash scripts/check-shell-yaml.sh

shell-yaml-selftest:
	bash scripts/check-shell-yaml.sh --selftest

# DF-CRIER-209: the other half of the gate's blind spot. Tier 1 reads Go source
# only and the shell/YAML arm (above) reads shell scripts and workflow YAML, so a
# Makefile-only or Dockerfile-only diff still landed on a green that verified
# nothing about it. make-docker-check covers the tracked Makefiles and
# Dockerfiles: every makefile is dry-parsed (`make -n -f <file> <target>` over a
# target list derived from the file — .PHONY, else the first non-special target,
# else make's own default goal), so a spaces-indented recipe, an unterminated
# `define`, a bad `include` or an unresolvable prerequisite is a rejection that
# names the file:line; every dockerfile is checked with hadolint when it is on
# PATH and otherwise with a built-in python3 (stdlib-only) structural parse that
# catches an unknown instruction, a FROM with no image reference, a bad/duplicate
# stage alias, a continuation dangling at EOF, and a COPY --from that
# forward-references a stage defined later. The engines that ran are printed
# every run; a missing validator (no make, or neither hadolint nor python3) is
# exit 2, never a silent skip. This does NOT check the shell inside a recipe (a
# recipe calling a tracked script is covered by the shell/YAML arm instead).
# make-docker-selftest proves the checker still rejects a broken Makefile and a
# malformed Dockerfile and accepts a clean set, on fixtures it creates under
# ${TMPDIR:-/tmp} — including a NEUTER proof that the rejection is caused by the
# arm under test and not by accident.
make-docker-check:
	bash scripts/check-make-docker.sh

make-docker-selftest:
	bash scripts/check-make-docker.sh --selftest

# DF-CRIER-189: the third blind spot. Tier 1 reads Go source but its go_lint is
# `go vet`, which reports suspicious constructs and says nothing about
# FORMATTING, and neither of the two arms above reads a .go file — so a drifting
# file landed on a green that verified nothing about it (measured at HEAD
# 973f5e1: `gofmt -l` named internal/guard/types.go and
# internal/webhook/schema.go while the guard printed `Tier 1 Guards: PASS`).
# gofmt-check runs `gofmt -l` over every tracked .go file and rejects each file
# whose path comes back, printing its (bounded) `gofmt -d` diff and the fix line
# `gofmt -w <file>`; the verdict is gofmt's OUTPUT, never its exit status
# (`gofmt -l` exits 0 while reporting drift — only an unparseable source makes it
# exit nonzero, and that is reported too, with gofmt's own message). The resolved
# gofmt path and Go toolchain version are printed every run; gofmt missing from
# PATH is exit 2, never a silent skip. Given an EXPLICIT file list the checker
# fails closed (a list in which nothing classifies as a .go file, or a named .go
# path that does not exist, exits 1 with the paths named), and a DEFAULT run whose
# .go scope is empty is refused rather than reported as a PASS over 0 files.
# gofmt-selftest proves the checker still rejects a drifting fixture and accepts a
# clean one, on fixtures it creates under ${TMPDIR:-/tmp} — including a NEUTER
# proof that the rejection is caused by the arm under test and not by accident.
gofmt-check:
	bash scripts/check-gofmt.sh

gofmt-selftest:
	bash scripts/check-gofmt.sh --selftest

# CR-GAP-069: the fifth arm, and the only one whose subject is a PROCESS LIFE rather
# than a file's syntax. The dogfood federation demo leaked two orphan crier servers
# on :18767/:18877 (reaped by hand); the contract that prevents it lives in the demo
# harness — a script that backs the server with `&` must (a) register an EXIT trap
# that kills the pid it started and (b) assert after the start that the port's holder
# is that pid (assert_port_owned / port_holder_pid from scripts/lib/port-guard.sh).
# Every runner and scripts/e2e-battery.sh already do both; NOTHING enforced it, so an
# ad-hoc script could re-create the leak with no signal — which is what happened.
#
# demo-cleanup-check classifies a TRACKED shell script as server-spawning only when
# the text actually SPAWNS a crier server binary (./bin/crier, bin/crier,
# $WORKDIR/crier, $CRIER_BIN, $REPO/bin/crier, `go run ./cmd/server`, a backgrounded
# `make run`, or the MCP server bin/crier-mcp / `make mcp` / a `timeout … bash -c`
# launcher), in a backgrounded, wrapped or timeout-bounded form. A MENTION never
# classifies: comments, heredoc bodies (usage text, transcripts, inline python), the
# message commands (echo/printf/fatal/… — the `fatal "… make run, or ./bin/crier …"`
# shape in scripts/bunker-matrix.sh) and the non-executing commands (grep/awk/sed/…
# — the `grep -q "make run"` shape in scripts/bunker-matrix-selftest.sh) are text,
# and a `go build` that only writes the server binary is a BUILD, not a spawn. A
# classified script must meet BOTH requirements; every rejection names the file, the
# spawn line and each missing requirement ((a) trap, (b) ownership) by name. (b) is
# owed only by a script that binds a relay port — the relay always listens, so every
# relay spawner owes it; an MCP/stdio launcher binds nothing. A BOUNDED-EXECUTION
# script (timeout on the spawn, or a `timeout … bash -c` launcher executor such as
# scripts/check-mcp-stdout.sh) is exempt from (a) — the timeout is its reaping
# mechanism — but still owes (b) when it binds a port.
#
# It is a STATIC text check: it never starts a server and never signals a process
# (this host runs live crier servers owned by other sessions), and it fails closed —
# an explicit list with a nonexistent path, a non-shell path, or in which NOTHING
# classifies as server-spawning exits 1 with the paths named, and a default run whose
# scope is empty exits 2, so a green over files it never read is impossible.
# demo-cleanup-selftest proves all of that on fixtures it creates under
# ${TMPDIR:-/tmp} — including a NEUTER proof that the rejection is caused by the
# verdict and not by accident, and a regression proof that the real tree (the five
# example runners, e2e-battery.sh, check-mcp-stdout.sh) is still accepted.
demo-cleanup-check:
	bash scripts/check-demo-cleanup.sh

demo-cleanup-selftest:
	bash scripts/check-demo-cleanup-selftest.sh

# DF-CRIER-281 — CR-GAP-069's ask #2: the TICK-START ORPHAN SWEEP. demo-cleanup-check
# (the arm above) reads TRACKED shell scripts, so it cannot see the leak shape that
# actually costs this shared host: an ad-hoc dogfood/QA run whose compose file and
# scripts live under /tmp and which leaves containers (and a scratch-range port)
# behind. Measured on this host when the sweep was written: 20 containers named
# dogfood-* with compose configs under /tmp/dogfood-*, 7 more /tmp-compose stacks
# (df9r-*, qa3probe*-scaled-*), and a leaked `./bin/crier` holding :8767 — none of
# them visible to any gate.
#
# scripts/orphan-sweep.sh is REPORT-ONLY: it never stops, kills, removes or prunes
# anything, and findings never change its exit code. A container is an ORPHAN
# CANDIDATE when its name matches dogfood-* OR its
# com.docker.compose.project.config_files label names a file under /tmp/ — either
# rule, both reported, running or exited, with the container's status (incl. its
# age), that compose path, the docker ps port column and the PortMappings from
# `docker inspect`. Ports are every `ss -tlnp` listener in a configurable range
# (ORPHAN_SWEEP_PORT_MIN/ORPHAN_SWEEP_PORT_MAX, default 14000-29000 inclusive) with
# the holder's pid, process name and full command line (from /proc/<pid>/cmdline,
# with the ss record kept alongside); a holder whose command carries the range's own
# markers is still reported — the sweep never assumes a listener is benign — and a
# holder ss cannot attribute is reported as unattributable rather than skipped. It
# fails closed: a required tool (docker, ss, awk) missing from PATH or FAILING
# (`docker ps -a` / `ss` nonzero) is exit 2, never a green over a host it could not
# read, and an invalid port range is refused for the same reason.
#
# This target runs the SELFTEST only — never the live sweep. CI runners have no
# docker daemon and no dogfood stacks, and a checker that has only ever been seen to
# pass is decoration. The selftest drives the real script against STUB docker and ss
# executables placed first on PATH in a temp dir, so the sweep's own logic is
# exercised with no daemon, no socket table and no host process involved, and the
# stub log of EVERY invocation is what makes the report-only invariant checkable
# (no mutating verb was even invoked). It proves the classification rules
# (dogfood+/tmp, dogfood+/home via the name rule, non-dogfood+/tmp via the tmp rule,
# and the two negatives), the fail-closed paths (no docker, no ss, a failing docker,
# a non-numeric and an inverted range), the range filter with its inclusive bounds
# and its env override, the clean-host path (an explicitly empty census plus an empty
# socket table is a legal exit 0 with zero findings), and a NEUTER proof — a copy of
# the script whose classification is forced to always-no must FAIL the same fixture
# assertions, so a green selftest cannot be proving nothing.
orphan-sweep-check:
	bash scripts/orphan-sweep.sh --selftest

# REV5-CRIER-001: the fourth arm, and the only one whose subject is the REMOTES
# rather than the tree. This repo pushes main to two places — origin
# (github.com/crier-dev/crier) is the PRIMARY and the mirror is the subordinate
# CONTENT copy — and nothing in the repo asserted they carry the same commit. The
# tick battery compared them BY HAND, once per tick, with a `git rev-list
# --left-right --count origin/main...gitlab/main` typed into a shell, so the result
# lived in a chat log instead of the repo, and it was only run when someone
# remembered. The history that hand-check was catching: two diverged lineages
# around ticks 143-149, and a tick that never pushed the mirror at all (around
# ticks 79 and 101). A mirror that is BEHIND is the worse failure of the two,
# because clone/fetch are answered with a stale tree while everything looks green.
#
# parity-check fetches both remotes and requires `git rev-list --left-right
# --count A/main...B/main` to be exactly `0	0`. The two counts name opposite
# sides: left = A-only commits (so a nonzero left means B is behind), right =
# B-only commits (a nonzero right means A is behind). One side behind exits 1 with
# that side's push recipe; BOTH sides nonzero is dual lineage, which no push can
# reconcile, so it exits 1 with an ESCALATION and no push advice at all — the
# checker never force-pushes and never prints a copy-pasteable force command.
#
# It fails closed (exit 2, naming what was missing) on a missing remote, a failed
# fetch, a remote that answers but has no such branch, and unusable configuration —
# a remote this host cannot read is never reported as "no drift". The remotes it
# compares are overridable (PARITY_REMOTE_A / PARITY_REMOTE_B / PARITY_BRANCH) so
# parity-selftest can point it at throwaway bare repos under ${TMPDIR:-/tmp}.
#
# parity-selftest proves all of that on those fixtures, including a NEUTER proof
# that the rejection comes from the checker's own verdict. It is deliberately NOT a
# CI step (.github/workflows/ is untouched): CI runners have no gitlab credentials
# and no route to the mirror, so a parity job there could only ever exit 2 — this
# check belongs to the tick battery on the dev host, which is where both remotes
# are reachable. Same reasoning (and the same precedent) as heredoc-lint: different
# scope, run explicitly or from the tick.
parity-check:
	bash scripts/check-remote-parity.sh

parity-selftest:
	bash scripts/check-remote-parity-selftest.sh

# QA-CRIER-32: the fleet-shared QA generator ~/.hermes/scripts/bunker-qa.sh
# builds the per-agent remote script inside ONE UNQUOTED heredoc
# (build_remote_script's `cat <<EOF`), so every unescaped `$` and backtick in
# that body is expanded ON THE GENERATION HOST — three workers hit the class on
# 2026-09-21 (double-escaped `\$\(curl\)` + bare `$BX_TAG` shipping as exit 1; a
# backticked curl in a COMMENT executing at generation time with `curl: (2) no
# URL specified` on every run; two more backticks pulled from comments). No repo
# gate reads the generator (Tier 1 reads Go source; the file is fleet-shared and
# untracked), so heredoc-escape-lint.sh is the guard: it isolates the heredoc
# region and rejects unescaped/double-escaped backticks, double-escaped dollars,
# and bare expansions outside the documented generation-time bake allowlist.
# The default target is the LIVE generator; a missing file is exit 2 naming the
# path (fail closed, never a silent skip). heredoc-lint-selftest proves the lint
# accepts the current generator, rejects seeded bad fixtures (backtick,
# double-escape, bare unbound var, bare specials) with line numbers, and —
# NEUTER proof — a copy with its reject choke-point neutered accepts what the
# real lint rejects. Deliberately NOT wired into shell-yaml-check: the shell
# arm reads TRACKED repo scripts, this lints an UNTRACKED fleet-shared file —
# different scope, run it explicitly or from the QA tick.
heredoc-lint:
	bash scripts/lib/heredoc-escape-lint.sh

heredoc-lint-selftest:
	bash scripts/lib/heredoc-escape-lint-selftest.sh

# DF-CRIER-137: the documented MCP launcher's stdout contract. `make build-mcp && ./bin/crier-mcp`
# is what README.md and docs/integration-guide.md tell a client to run, and a strict
# stdio MCP client fails the handshake on any line that is not a JSON-RPC frame. The
# bridge has always been clean; the launcher was not — make echoed the `build-mcp`
# recipe to stdout (three filings: DF-CRIER-66/91/137). mcp-stdout-check runs the
# documented launcher(s) for real with one initialize frame on stdin and requires the
# FIRST stdout line to be the initialize response; a timeout is a failure, a launcher
# that prints nothing on stdout is exit 2 (never a green), and a missing tool
# (make/go/timeout) is exit 2 too. mcp-stdout-selftest proves the checker still
# rejects a contaminated launcher and refuses a silent one, including a NEUTER proof
# that the rejection comes from the checker's own verdict.
mcp-stdout-check:
	bash scripts/check-mcp-stdout.sh

mcp-stdout-selftest:
	bash scripts/check-mcp-stdout-selftest.sh

# DF-CRIER-278: the tier-2 judge's INPUT budget is the most expensive knob in the
# repo, and a board-only diff has nothing in it a larger budget would resolve.
# Measured on tick 365: `gitreins task complete df-crier-203-expires-null-panel`
# ran twice on three .coding-hermes JSONL files and zero source files — 33,824,117
# and 48,064,774 tokens_in (INCOMPLETE, "Cap exceeded: Input token budget (48.0M)
# exceeded (48.1M used)") against 188K-706K for a normal source-diff judge, because
# the evaluator's own repo exploration (file_scope: full) IS the cost. Raising the
# cap is rejected (a bigger cap buys a longer exploration, and the next run starves
# at a higher number), so the fix is fail-closed and crier-side: a classifier that
# authorizes `gitreins task complete --skip-tier2` ONLY for a diff with zero source
# files, plus the committed evidence artifact that keeps the authorization
# auditable (docs/ops-evidence.md). scripts/lib/judge-diff-class.sh decides it on
# the path list alone — non-source iff docs/**, *.md (any depth), .coding-hermes/**,
# .gitreins/**, LICENSE, NOTICE, .gitignore, .gitattributes, .github/**, the ROOT
# Makefile/Dockerfile*, or yaml under .github//docs/; everything else is source,
# including examples/**, scripts/**, specs/** and openapi yaml (those source trees
# win over the *.md rule, because spec/openapi files drive generated code and gates)
# and any unrecognized path — the conservative direction, since a wrong board-only
# verdict silently drops the judge on a diff the rule meant to protect. An empty
# diff is refused in EVERY flag combination (exit 3, "no diff = no authorization").
# This selftest proves the contract on synthetic path lists only (no git, no
# network): both verdicts, the --strict-verify gate, the --allow-source opt-in, the
# empty-diff refusal in all four flag combinations, unknown-flag misuse (exit 2),
# the mixed list, the duplicate-folded and CRLF counts, and every boundary row the
# header documents — plus a NEUTER proof that seds the classifier's verdict call
# site to a forced board-only and requires (a) the copy to differ, (b) the copy to
# FAIL the selftest's own source-bearing assertion, and (c) a full selftest run
# against the neutered pair to FAIL, so a verdict-forced-success classifier cannot
# pass it.
judge-diff-class-selftest:
	bash scripts/lib/judge-diff-class-selftest.sh

# DF-CRIER-206: .git/hooks/pre-commit is gitreins-generated and UNTRACKED, so the
# tracked wrapper scripts/hooks/pre-commit is the source of truth and this target
# puts it in place. Idempotent: re-running when the installed hook is already
# byte-identical is a no-op. MEASURED: `gitreins install` overwrites
# .git/hooks/pre-commit unconditionally, and `gitreins init --reset` overwrites it
# too (`gitreins init` without --reset leaves an existing hook alone) — re-run
# `make install-hooks` after either. The hook is installed as a COPY, not a
# symlink: writing to a symlinked hook writes THROUGH the link, so
# `gitreins install` would clobber the tracked file itself (measured).
#
# DF-CRIER-211 residue: the backup was unbounded — every install wrote a new
# `<hook>.bak-<utc-ts>` and nothing ever pruned, so a dev box accumulated one per
# repair run (3 on 2026-09-17 alone). The 5 most recent are kept; older ones are
# removed AFTER the new backup is written, so the target can never make the
# situation worse if the prune fails.
install-hooks:
	@root="$$(git rev-parse --show-toplevel 2>/dev/null || echo '$(CURDIR)')"; \
	src="$$root/scripts/hooks/pre-commit"; \
	[ -f "$$src" ] || { echo "install-hooks: ERROR: $$src is missing"; exit 1; }; \
	chmod +x "$$src"; \
	hooks_dir="$$(git rev-parse --git-path hooks 2>/dev/null || echo .git/hooks)"; \
	case "$$hooks_dir" in /*) ;; *) hooks_dir="$$root/$$hooks_dir" ;; esac; \
	mkdir -p "$$hooks_dir" || exit 1; \
	dst="$$hooks_dir/pre-commit"; \
	if [ -f "$$dst" ] && cmp -s "$$src" "$$dst"; then \
		echo "install-hooks: already installed — $$dst is byte-identical to scripts/hooks/pre-commit"; \
		exit 0; \
	fi; \
	if [ -e "$$dst" ] && [ ! -f "$$dst" ]; then \
		echo "install-hooks: ERROR: $$dst exists and is not a regular file"; exit 1; \
	fi; \
	if [ -f "$$dst" ]; then \
		bak="$$dst.bak-$$(date -u +%Y%m%dT%H%M%SZ)"; \
		cp "$$dst" "$$bak" || exit 1; \
		echo "install-hooks: backed up the previous hook to $$bak"; \
		keep=5; \
		ls -1t "$$dst".bak-* 2>/dev/null | tail -n +$$((keep + 1)) | while IFS= read -r old; do \
			rm -f "$$old" && echo "install-hooks: pruned hook backup $$old (keeping the $$keep most recent)"; \
		done; \
	fi; \
	cp "$$src" "$$dst" || exit 1; \
	chmod +x "$$dst" || exit 1; \
	cmp -s "$$src" "$$dst" || { echo "install-hooks: ERROR: installed hook is not byte-identical to $$src"; exit 1; }; \
	echo "install-hooks: installed $$dst (byte-identical copy of scripts/hooks/pre-commit)"

# RELEASE-001: v0.1.0-rc1 was cut by hand — `git tag -a` typed directly on main,
# the gates run ad hoc, and neither a changelog nor the procedure written down
# anywhere in the repo, so the next cut had to re-derive all of it. This target
# encodes the sequence, and each step is a REFUSAL rather than a warning:
#
#   VERSION must be passed explicitly   `origin VERSION` must be the command
#                                       line (or the environment) — the file's
#                                       own default is `git describe --dirty`,
#                                       which would silently tag the build
#                                       identity string instead of a release;
#                                       a git-describe-shaped value is refused
#                                       too, as a second line of defence.
#   the tag must look like a version    vX.Y.Z, optionally with a pre-release
#                                       suffix (v0.1.0-rc2, v0.2.0).
#   the working tree must be clean      `git status --porcelain` empty. The tag
#                                       is cut from a COMMITTED tree — cut the
#                                       tag first and the changelog entry for it
#                                       can never be in the commit it names.
#   the branch must be main             releases are cut from main, never from a
#                                       worktree branch or a detached HEAD.
#   the tag must not exist yet          re-tagging a published version is how
#                                       two different commits end up under one
#                                       number; cut a new rc instead.
#   the gates must be green             make build && make lint && make
#                                       test-short, each echoed before it runs.
#
# Then it creates the annotated tag `VERSION` with the message `crier VERSION`
# and STOPS: it prints the exact push command for every configured remote and
# pushes nothing, because the tag must only be published after CI is green on
# main. The changelog lives in CHANGELOG.md and the full procedure, the gate
# contract and the post-tag steps are in docs/releases.md.
#
# NOTE on how the gates are invoked: as a literal `make`, never the `$(MAKE)`
# recursive marker. GNU make treats a recipe line mentioning `$(MAKE)` as
# special and RUNS it even under `-n`, so `make -n release` — and the dry-parse
# this repo's own make-docker-check (DF-CRIER-209) runs over every .PHONY target
# — would have executed the whole build/lint/test battery instead of printing
# it. A literal `make` is not special-cased, so `-n` stays a dry run; the
# sub-make still inherits MAKEFLAGS and MAKELEVEL through the environment.
release:
	@set -e; \
	v='$(VERSION)'; \
	case "$(origin VERSION)" in \
	  'command line'|'environment'|'override') ;; \
	  *) echo "release: ERROR: refusing the default VERSION '$$v' (that is git describe output, not a release)"; \
	     echo "release: ERROR: pass the tag explicitly: make release VERSION=v0.1.0-rc2"; \
	     exit 1 ;; \
	esac; \
	case "$$v" in \
	  v[0-9]*.[0-9]*.[0-9]*) ;; \
	  *) echo "release: ERROR: VERSION '$$v' is not a vX.Y.Z tag (example: v0.1.0-rc2)"; exit 1 ;; \
	esac; \
	case "$$v" in \
	  *-[0-9]*-g[0-9a-f][0-9a-f]*) echo "release: ERROR: VERSION '$$v' looks like git describe output, not a tag"; exit 1 ;; \
	esac; \
	echo "release: version ......... $$v"; \
	echo "release: repository ...... $$(git rev-parse --show-toplevel)"; \
	echo "release: ensuring the working tree is clean"; \
	if [ -n "$$(git status --porcelain)" ]; then \
	  echo "release: ERROR: the working tree is dirty — commit or stash first:"; \
	  git status --short; \
	  exit 1; \
	fi; \
	echo "release: OK — working tree clean"; \
	branch="$$(git rev-parse --abbrev-ref HEAD)"; \
	echo "release: ensuring the branch is main (on '$$branch')"; \
	if [ "$$branch" != main ]; then \
	  echo "release: ERROR: releases are cut from main, not '$$branch'"; \
	  exit 1; \
	fi; \
	echo "release: OK — on main ($$(git rev-parse --short HEAD) $$(git log -1 --format=%s | cut -c1-60))"; \
	echo "release: ensuring the tag $$v does not exist yet"; \
	if git rev-parse -q --verify "refs/tags/$$v" >/dev/null 2>&1; then \
	  echo "release: ERROR: tag $$v already exists — pick a new version (do not re-tag a published one)"; \
	  exit 1; \
	fi; \
	echo "release: OK — tag $$v is free"; \
	echo "release: running the gates (a failure stops here, before any tag)"; \
	echo "release: + make build";        make build; \
	echo "release: + make lint";         make lint; \
	echo "release: + make test-short";   make test-short; \
	echo "release: OK — gates green (make build && make lint && make test-short)"; \
	echo "release: creating annotated tag $$v (message: crier $$v)"; \
	git tag -a "$$v" -m "crier $$v"; \
	echo "release: created — $$v -> $$(git rev-parse --short HEAD)"; \
	if git merge-base --is-ancestor HEAD origin/main >/dev/null 2>&1; then \
	  echo "release: HEAD is an ancestor of origin/main (as of the last fetch)"; \
	else \
	  echo "release: WARNING: HEAD is not on origin/main (as of the local ref) — push main and let CI go green before pushing this tag, and run 'git fetch origin' first if that ref is stale"; \
	fi; \
	echo "release: NOT pushed (this target never pushes — publish after CI is green on main)"; \
	echo "release: push it by hand:"; \
	echo "release:     git push origin $$v"; \
	if git remote | grep -qx gitlab; then \
	  echo "release:     git push gitlab $$v"; \
	else \
	  echo "release:     (no gitlab remote configured — origin only)"; \
	fi; \
	echo "release: changelog: CHANGELOG.md (Keep a Changelog); procedure: docs/releases.md"

generate:
	go generate ./...
