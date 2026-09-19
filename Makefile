.PHONY: help build build-mcp mcp test test-short test-integration lint run stop clean docker-build coverage coverage-html coverage-check docs-check generate port-guard-selftest scratch-port-rotation-selftest transport-retry-selftest load-repro-selftest shell-yaml-check shell-yaml-selftest install-hooks make-docker-check make-docker-selftest gofmt-check gofmt-selftest mcp-stdout-check mcp-stdout-selftest

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
	@echo "  shell-yaml-check  Check every tracked shell script (bash -n) and .github/workflows/*.yml (actionlint, or the PyYAML fallback) — DF-CRIER-206"
	@echo "  shell-yaml-selftest  Prove that checker still rejects broken shell/YAML and accepts a clean pair (DF-CRIER-206)"
	@echo "  make-docker-check  Check every tracked Makefile (make -n dry-parse) and Dockerfile (hadolint, or the built-in python3 parse) — DF-CRIER-209"
	@echo "  make-docker-selftest  Prove that checker still rejects a broken Makefile/malformed Dockerfile and accepts a clean set (DF-CRIER-209)"
	@echo "  gofmt-check       Check every tracked .go file with gofmt (go vet does not read formatting) — a drifting file fails (DF-CRIER-189)"
	@echo "  gofmt-selftest    Prove that checker still rejects a drifting .go file and accepts a clean one, incl. a neuter proof (DF-CRIER-189)"
	@echo "  mcp-stdout-check  Run the documented MCP launcher(s) and prove their stdout carries only JSON-RPC frames, never make's recipe echo or build output (DF-CRIER-137)"
	@echo "  mcp-stdout-selftest  Prove that checker still rejects a launcher that contaminates stdout and refuses one that prints nothing, incl. a neuter proof (DF-CRIER-137)"
	@echo "  install-hooks     Install scripts/hooks/pre-commit into .git/hooks (idempotent) so a green commit states its scope (DF-CRIER-206)"
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

generate:
	go generate ./...
