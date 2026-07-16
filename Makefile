.DEFAULT_GOAL := check

BINARY := sandman
CMD := ./cmd/sandman

# VERSION is the SemVer string injected into the sandman binary at build
# time via `go build -ldflags '-X main.version=$(VERSION)'`. When VERSION
# is not provided on the command line, it defaults to a git-derived string
# so every `make build` from a different commit produces a distinct value
# (e.g. `3fb9a014`, `v0.0.0-3-g3fb9a014`, `3fb9a014-dirty` for a dirty
# working tree). External release tooling overrides VERSION on the command
# line to inject the canonical SemVer without touching Go code. The Go-code
# `Version()` fallback chain in cmd/sandman/main.go covers `go install
# ./cmd/sandman` (no Makefile) with the linker-populated buildinfo
# pseudo-version, falling back to the literal `dev` when neither path
# provides a value. Worktree quirk: `git -C .` runs `git describe` in the
# current worktree (so it reports the worktree's HEAD), whereas
# `runtime/debug.ReadBuildInfo().Main.Version` uses the parent worktree's
# HEAD when the worktree's `.git` is a file pointer — the Makefile path
# therefore produces worktree-correct strings.
VERSION ?= $(shell git -C . describe --tags --always --dirty 2>/dev/null || git -C . rev-parse --short HEAD 2>/dev/null || echo dev)

LDFLAGS := -ldflags '-X main.version=$(VERSION)'

.PHONY: check build install fmt test vet clean

check: fmt vet test
	@echo "All checks passed."

fmt:
	@echo "Formatting Go code..."
	gofmt -w .

vet:
	@echo "Running go vet..."
	go vet ./...

test:
	@echo "Running tests..."
	go test -race -v ./...

build:
	@echo "Building $(BINARY)..."
	go build $(LDFLAGS) -o $(BINARY) $(CMD)

install:
	@echo "Installing $(BINARY)..."
	go install $(CMD)

clean:
	@echo "Cleaning build artifacts..."
	rm -f $(BINARY)
