VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install test test-short fuzz chaos lint

build:            ## static binary in ./bin (CGO off = runs on any Linux)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ephemeris ./cmd/ephemeris

install:          ## puts `ephemeris` in $(go env GOPATH)/bin
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/ephemeris

test:             ## unit + integration (integration skips without an engine)
	go test -race ./...

test-short:       ## unit tests only
	go test -short -race ./...

fuzz:
	go test -run x -fuzz FuzzAllowHost -fuzztime 30s ./internal/netpolicy
	go test -run x -fuzz FuzzParseBytes -fuzztime 30s ./internal/spec

chaos: build      ## kill -9 the CLI and prove the reaper cleans up
	./test/chaos.sh ./bin/ephemeris

lint:
	gofmt -l . | (! grep .) && go vet ./...