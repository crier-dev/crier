.PHONY: help build build-mcp test test-short test-integration lint run clean docker-build coverage coverage-html coverage-check docs-check generate

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
	@echo "  run               Build and run the server (default :8767)"
	@echo "  test              Full test suite, no caching (go test ./... -count=1 -timeout 60s)"
	@echo "  test-short        Unit tests only — skips integration, no Docker required"
	@echo "  test-integration  PostgreSQL-backed registry tests (requires Docker)"
	@echo "  lint              Static analysis (go vet ./...)"
	@echo "  coverage          Run tests and print total coverage"
	@echo "  coverage-html     Generate coverage.html"
	@echo "  coverage-check    Fail if coverage is below the 70% threshold"
	@echo "  docs-check        Execute prose claims in docs/claims.yaml against the live server — prose drift fails the build (CR-GAP-055)"
	@echo "  clean             Remove built binaries"
	@echo "  docker-build      Build crier and crier-mcp Docker images"
	@echo "  generate          Run go generate ./..."
	@echo ""
	@echo "Config is env-driven (CRIER_PORT, CR_DATABASE_URL, CR_AUTH_TOKEN, CR_REQUIRE_AGENT_SIG) — see README.md."
	@echo "The server binary also takes CLI flags: ./bin/crier --help prints full usage, -port overrides CRIER_PORT, -db-url overrides CR_DATABASE_URL, -version prints the build identity (version, commit, dirty marker)."

build:
	go build -ldflags "$(CRIER_LDFLAGS)" -o bin/crier ./cmd/server

build-mcp:
	go build -ldflags "$(CRIER_LDFLAGS)" -o bin/crier-mcp ./cmd/crier-mcp

test:
	go test ./... -count=1 -timeout 60s

test-short:
	go test -short ./...

test-integration:
	go test -tags=integration -count=1 -timeout 5m ./internal/registry

lint:
	go vet ./...

run: build
	./bin/crier

clean:
	rm -rf bin/

docker-build:
	docker build -t crier:latest .
	docker build -f Dockerfile.mcp -t crier-mcp:latest .

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

generate:
	go generate ./...
