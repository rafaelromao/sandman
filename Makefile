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

.PHONY: check build install fmt fmt-check build-check test test-ci test-browser contracts vet staticcheck clean

# Keep Staticcheck on the repository's supported Go toolchain. Newer local
# toolchains may emit export data that v0.7.0 cannot consume.
STATICCHECK_VERSION := v0.7.0
STATICCHECK_TOOLCHAIN := go1.25.0

# Explicit browser inventory is shared by the exclusive CI selectors and tested
# against Chromium-backed test functions in scripts/release_workflow_test.go.
PORTAL_BROWSER_TESTS := ^($(shell paste -sd '|' scripts/portal_browser_tests.txt))$$

check: fmt-check build-check vet staticcheck contracts test
	@echo "All checks passed."

fmt:
	@echo "Formatting Go code..."
	gofmt -w .

fmt-check:
	@files="$$(gofmt -l .)" || exit $$?; if [ -n "$$files" ]; then printf "%s\n" "$$files"; exit 1; fi

# Shared, non-mutating PR validation commands. Browser scenarios have one owner.
build-check:
	go build -v ./...

test-ci:
	mkdir -p reports
	go test -race -covermode=atomic -coverprofile=reports/coverage.out -json -skip '$(PORTAL_BROWSER_TESTS)' -timeout=20m ./...

test-browser:
	SANDMAN_REQUIRE_PORTAL_BROWSER=1 go test -tags visual -json ./internal/cmd -run '$(PORTAL_BROWSER_TESTS)' -count=1 -timeout=10m

# Default race coverage still runs every architecture guard; this focused job
# makes contracts independently visible without relying on an exclusion list.
contracts:
	python3 scripts/opencode_canary_test.py
	python3 scripts/publication_test.py
	python3 scripts/release_report_test.py
	go test -tags smoke -json ./internal/cmd -run '^TestReleasePresetBuildsMissingPrewarmImage$$'
	python3 scripts/ci_report_test.py
	python3 scripts/security_policy_test.py
	python3 scripts/security_check_test.py

	python3 scripts/coverage_report_test.py
	go test -json ./scripts ./internal/skill/...
	go test -json ./internal/batch ./internal/events ./internal/review ./internal/sandbox -run 'Contract|AgentSelection|Cancellation|Ownership|Lifecycle|Centralized|ProductionGoDoesNotInvokeRepositoryWideWorktreePrune|WorktreeSandboxExecBlocksGlobalWorktreePrune'
	go run scripts/check_adr_index.go

vet:
	@echo "Running go vet..."
	go vet ./...

staticcheck:
	@echo "Running staticcheck $(STATICCHECK_VERSION) with $(STATICCHECK_TOOLCHAIN)..."
	GOTOOLCHAIN=$(STATICCHECK_TOOLCHAIN) go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

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

.PHONY: security-check workflow-check
security-check:
	bash scripts/security_check.sh

workflow-check:
	bash scripts/workflow_check.sh
