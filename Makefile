# hi — Claude Code multi-backend proxy
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "1.0.0")
BUILD_TIME := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME)

BIN := hi
GO := go
GOFMT := gofmt

.PHONY: all build clean test lint install dist

all: build

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/hi/

# Cross-platform release builds.
dist:
	GOOS=linux   GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-linux-amd64   ./cmd/hi/
	GOOS=linux   GOARCH=arm64 $(GO) build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-linux-arm64   ./cmd/hi/
	GOOS=darwin  GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-darwin-amd64  ./cmd/hi/
	GOOS=darwin  GOARCH=arm64 $(GO) build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-darwin-arm64  ./cmd/hi/
	GOOS=windows GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-windows-amd64.exe ./cmd/hi/
	@echo ""
	@echo "Builds complete:"
	@ls -lh dist/
	@echo ""
	@du -sh dist/

clean:
	rm -rf $(BIN) dist/

test:
	$(GO) test ./... -v -count=1

lint:
	$(GO) vet ./...

fmt:
	$(GOFMT) -s -w .

install: build
	install -m 755 $(BIN) /usr/local/bin/$(BIN) 2>/dev/null || install -m 755 $(BIN) $(HOME)/.local/bin/$(BIN)
	@echo "Installed $(BIN) to /usr/local/bin/ or ~/.local/bin/"
