# unwedge build/test/release helpers.
#
# The daemon (unwedged) runs on the controller, itself a vEdge 1000, so its
# release target is linux/mips64 (big-endian). The client (unwedge) and MCP
# bridge (unwedge-mcp) run wherever the agent/CI is, so they build for the host
# by default and are also cross-compiled for common targets.

GO       ?= go
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
BIN      := bin

CMDS := unwedged unwedge unwedge-mcp

.PHONY: all build test race vet tidy proto clean \
        daemon-mips64 client-host release fmt

all: build

## build: build all commands for the host
build:
	@mkdir -p $(BIN)
	@for c in $(CMDS); do \
		echo "build $$c ($(shell $(GO) env GOOS)/$(shell $(GO) env GOARCH))"; \
		$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/$$c ./cmd/$$c || exit 1; \
	done

## daemon-mips64: build unwedged for the vEdge 1000 controller (big-endian MIPS64)
daemon-mips64:
	@mkdir -p $(BIN)
	GOOS=linux GOARCH=mips64 $(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/unwedged.mips64 ./cmd/unwedged
	@echo "built $(BIN)/unwedged.mips64"

## release: cross-compile the full release matrix
release: daemon-mips64
	@mkdir -p $(BIN)
	GOOS=linux GOARCH=mips64 $(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/unwedge.mips64 ./cmd/unwedge
	GOOS=linux GOARCH=amd64  $(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/unwedge.linux-amd64 ./cmd/unwedge
	GOOS=linux GOARCH=arm64  $(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/unwedge.linux-arm64 ./cmd/unwedge
	GOOS=linux GOARCH=amd64  $(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/unwedge-mcp.linux-amd64 ./cmd/unwedge-mcp
	GOOS=darwin GOARCH=arm64 $(GO) build -ldflags '$(LDFLAGS)' -o $(BIN)/unwedge-mcp.darwin-arm64 ./cmd/unwedge-mcp
	@echo "release binaries in $(BIN)/"

# Release binaries must be statically linked. Built natively on amd64, `go build`
# defaults to CGO on and dynamically links glibc, which OpenWrt's musl-based
# x86_64 packaging rejects ("missing dependencies for the following libraries:
# libc.so.6"); the cross-built arches have no C toolchain so they are already
# static. unwedge is pure Go, so disabling cgo is safe and keeps every arch
# consistently static and portable. (The race target needs cgo, so it is left
# to inherit the default.)
release dist: export CGO_ENABLED := 0

# Architectures for which we publish prebuilt release tarballs. mips64 is the
# vEdge 1000 controller (big-endian); the others are for running the client/MCP.
DIST_ARCHES ?= mips64 amd64 arm64
# Strip a leading "v" from a tag so v0.1.0 -> 0.1.0 for artifact names.
DISTVER ?= $(patsubst v%,%,$(VERSION))

## dist: build per-arch release tarballs (unwedge-<ver>-linux-<arch>.tar.gz) in bin/
## Tarballs are deterministic so a given (version, Go toolchain, arch) is reproducible.
dist:
	@mkdir -p $(BIN)
	@for arch in $(DIST_ARCHES); do \
		d="unwedge-$(DISTVER)-linux-$$arch"; \
		echo "packaging $$d"; \
		rm -rf "$(BIN)/$$d"; mkdir -p "$(BIN)/$$d"; \
		for cmd in $(CMDS); do \
			GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
				-o "$(BIN)/$$d/$$cmd" ./cmd/$$cmd || exit 1; \
		done; \
		cp README.md config.example.yaml "$(BIN)/$$d/"; \
		tar --sort=name --mtime='UTC 2020-01-01' --owner=0 --group=0 --numeric-owner \
			-czf "$(BIN)/$$d.tar.gz" -C "$(BIN)" "$$d"; \
		rm -rf "$(BIN)/$$d"; \
	done
	@cd $(BIN) && sha256sum unwedge-$(DISTVER)-linux-*.tar.gz | tee SHA256SUMS
	@echo "release tarballs in $(BIN)/"

## test: run unit tests
test:
	$(GO) test ./...

## race: run unit tests with the race detector
race:
	$(GO) test -race ./...

## vet: static checks
vet:
	$(GO) vet ./...

## fmt: gofmt the tree
fmt:
	$(GO) fmt ./...

## tidy: tidy go.mod
tidy:
	$(GO) mod tidy

## proto: regenerate gRPC stubs (requires buf + protoc-gen-go[-grpc] on PATH)
proto:
	buf generate

## clean: remove build output
clean:
	rm -rf $(BIN)
