.PHONY: help build build-mcp test test-short test-integration lint run clean docker-build coverage coverage-html coverage-check generate

# Version injected into cmd/server's version var via -ldflags.
# Override at build time: make build VERSION=1.2.3
VERSION ?= dev

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
	@echo "  clean             Remove built binaries"
	@echo "  docker-build      Build crier and crier-mcp Docker images"
	@echo "  generate          Run go generate ./..."
	@echo ""
	@echo "Config is env-driven (CRIER_PORT, CR_DATABASE_URL, CR_AUTH_TOKEN, CR_REQUIRE_AGENT_SIG) — see README.md. The server binary takes no CLI flags."

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/crier ./cmd/server

build-mcp:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/crier-mcp ./cmd/crier-mcp

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

generate:
	go generate ./...
