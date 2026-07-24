VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf dev)
GOFLAGS ?= -buildvcs=false

.PHONY: build test check clean

build:
	go build $(GOFLAGS) -ldflags "-X main.version=$(VERSION)" -o agent-relay ./cmd/agent-relay

test:
	go test -count=1 ./...

check:
	go test -count=1 ./...
	go vet ./...
	go test -count=1 -race ./...

clean:
	$(RM) agent-relay
