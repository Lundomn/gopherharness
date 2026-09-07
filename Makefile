.PHONY: build test check contract install clean

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
	PICO_PYTHON_ROOT=/Users/ljy/Documents/pico /Users/ljy/Documents/pico/.venv/bin/python scripts/cross_language_contract.py --go-command "go run ./cmd/gopherharness-contract"

install:
	go install ./cmd/gopherharness ./cmd/gopherharness-tui ./cmd/gopherharness-eval ./cmd/gopherharness-contract

clean:
	go clean
