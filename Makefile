GO ?= go
BIN := devpilot-hook
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/twotwoba/DevPilot-Hook/internal/cli.Version=$(VERSION)

.PHONY: help build test race e2e lint work install-local clean

help:
	@echo "make build        编译到 ./bin/$(BIN)"
	@echo "make test         运行单元测试"
	@echo "make race         运行单元测试（-race）"
	@echo "make e2e          运行端到端测试（需同级 DevPilot-APNS）"
	@echo "make work         复制 go.work.example 生成 go.work"
	@echo "make install-local 安装到 ~/.local/bin"

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
	@echo "go.work 已就绪"

install-local: build
	install -m 0755 bin/$(BIN) $(HOME)/.local/bin/$(BIN)

clean:
	rm -rf bin dist
