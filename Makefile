# Build the remote-shell-mcpd daemon into ./bin/.
# Flags match the daemon build in .goreleaser.yaml: CGO off, stripped,
# version injected into main.version. Tagged release archives come from goreleaser.

BINARY := bin/remote-shell-mcpd
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build run
build:
	@case "$$(uname -s)" in \
		Darwin|Linux) ;; \
		*) echo "remote-shell-mcpd builds on macOS and Linux only" >&2; exit 1 ;; \
	esac
	mkdir -p bin
	CGO_ENABLED=0 go build \
		-trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o $(BINARY) \
		./cmd/remote-shell-mcpd
	@echo "built $(CURDIR)/$(BINARY) ($(VERSION))"

run: build
	./$(BINARY)
