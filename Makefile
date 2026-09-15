.PHONY: build test check contract benchmark install clean

build:
	mkdir -p bin
	go build -o bin/gopherharness ./cmd/gopherharness
	go build -o bin/gopherharness-tui ./cmd/gopherharness-tui
	go build -o bin/gopherharness-eval ./cmd/gopherharness-eval
	go build -o bin/gopherharness-contract ./cmd/gopherharness-contract

test:
	go test ./...

check:
	gofmt -w cmd internal
	go vet ./...
	go test -race ./...

contract:
	go test ./internal/protocol -run TestGoldenModelOutputContract -count=1

benchmark:
	go run ./cmd/gopherharness-eval --offline --benchmark benchmarks/benchmark.json --fixtures benchmarks --workspaces artifacts/go-workspaces --artifact artifacts/go-benchmark.json

install:
	go install ./cmd/gopherharness ./cmd/gopherharness-tui ./cmd/gopherharness-eval ./cmd/gopherharness-contract

clean:
	go clean
