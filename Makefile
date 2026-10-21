.PHONY: build test fmt fmt-check check contract benchmark install clean

build:
	mkdir -p bin
	go build -o bin/gopherharness ./cmd/gopherharness
	go build -o bin/gopherharness-tui ./cmd/gopherharness-tui
	go build -o bin/gopherharness-eval ./cmd/gopherharness-eval
	go build -o bin/gopherharness-contract ./cmd/gopherharness-contract
	go build -o bin/gopherharness-gateway ./cmd/gopherharness-gateway

test:
	go test ./...

fmt:
	gofmt -w cmd internal

fmt-check:
	@files="$$(gofmt -l cmd internal)"; if [ -n "$$files" ]; then printf '%s\n' "$$files"; exit 1; fi

check: fmt-check
	go vet ./...
	go test -race -count=1 -timeout 120s ./...

contract:
	go test ./internal/protocol -run TestGoldenModelOutputContract -count=1

benchmark:
	go run ./cmd/gopherharness-eval --offline --benchmark benchmarks/benchmark.json --fixtures benchmarks --workspaces artifacts/go-workspaces --artifact artifacts/go-benchmark.json

install:
	go install ./cmd/gopherharness ./cmd/gopherharness-tui ./cmd/gopherharness-eval ./cmd/gopherharness-contract ./cmd/gopherharness-gateway

clean:
	go clean
