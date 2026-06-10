.DEFAULT_GOAL := check

BINARY := sandman
CMD := ./cmd/sandman

.PHONY: check build install fmt test vet test-scripts clean

check: fmt vet test test-scripts
	@echo "All checks passed."

fmt:
	@echo "Formatting Go code..."
	gofmt -w .

vet:
	@echo "Running go vet..."
	go vet ./...

test:
	@echo "Running tests..."
	go test -v ./...

test-scripts:
	@echo "Running script smoke tests..."
	@bash tests/scripts/reconcile-stranded-worktrees_test.sh

build:
	@echo "Building $(BINARY)..."
	go build -o $(BINARY) $(CMD)

install:
	@echo "Installing $(BINARY)..."
	go install $(CMD)

clean:
	@echo "Cleaning build artifacts..."
	rm -f $(BINARY)
