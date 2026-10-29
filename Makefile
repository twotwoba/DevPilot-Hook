GO ?= go
BIN := devpilot-hook
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/twotwoba/DevPilot-Hook/internal/cli.Version=$(VERSION)

.PHONY: help build test race e2e lint work install-local clean

help:
	@echo "make build        build to ./bin/$(BIN)"
	@echo "make test         run unit tests"
	@echo "make race         run unit tests (-race)"
	@echo "make e2e          run end-to-end tests (requires sibling DevPilot-APNS)"
	@echo "make work         copy go.work.example to go.work"
	@echo "make install-local install to ~/.local/bin"

build:
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BIN) ./cmd/devpilot-hook

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

e2e:
	$(GO) test -race -tags=e2e ./test/e2e -v

lint:
	$(GO) vet ./...

work:
	@test -f go.work || cp go.work.example go.work
	@echo "go.work is ready"

install-local: build
	install -m 0755 bin/$(BIN) $(HOME)/.local/bin/$(BIN)

clean:
	rm -rf bin dist
