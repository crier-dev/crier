.PHONY: build test lint run clean

build:
	go build -o bin/crier ./cmd/server

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

generate:
	go generate ./...
