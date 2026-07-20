.PHONY: build build-mcp test test-short test-integration lint run clean docker-build generate

build:
	go build -o bin/crier ./cmd/server

build-mcp:
	go build -o bin/crier-mcp ./cmd/crier-mcp

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

generate:
	go generate ./...
