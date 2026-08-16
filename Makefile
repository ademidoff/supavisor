.PHONY: build test cover cover-html clean run lint lint-install format

# Version of golangci-lint to use
GOLANGCI_LINT_VERSION := v2.12.2

# Coverage profile produced by the cover target
COVERAGE_FILE := coverage.out

# Extra flags for the test targets
# CI passes -count=1 to bypass Go's test result cache
GOTESTFLAGS ?=

# Path to golangci-lint binary
GOLANGCI_LINT := $(shell if [ -f ./bin/golangci-lint ]; then echo ./bin/golangci-lint || echo ""; fi)

# Build the binaries
build:
	@echo "Formatting the code..."
	$(MAKE) format
	@echo "Building supavisor..."
	@go build -o bin/supavisor ./cmd/supavisor
	@echo "Building sctl..."
	@go build -o bin/sctl ./cmd/sctl
	@echo "Build complete."
	@ls -l bin/

# Run all tests
test:
	@echo "Running tests..."
	@go test -race -v $(GOTESTFLAGS) ./...

# Run tests with the race detector and coverage, then print the total
# -coverpkg=./... credits code exercised across package boundaries
cover:
	@echo "Running tests with coverage..."
	@go test -race -covermode=atomic $(GOTESTFLAGS) -coverpkg=./... -coverprofile=$(COVERAGE_FILE) ./...
	@go tool cover -func=$(COVERAGE_FILE) | tail -1

# Open the annotated HTML coverage report in a browser
cover-html: cover
	@go tool cover -html=$(COVERAGE_FILE)

# Install golangci-lint if not present or version doesn't match
lint-install:
	@if [ ! -f ./bin/golangci-lint ] || ! ./bin/golangci-lint --version 2>/dev/null | grep -qF "$(patsubst v%,%,$(GOLANGCI_LINT_VERSION))"; then \
		echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..."; \
		GOBIN=$(CURDIR)/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
		echo "golangci-lint installed successfully"; \
	fi

# Run static analysis with golangci-lint
lint: lint-install
	@echo "Running golangci-lint..."
	$(GOLANGCI_LINT) run --timeout=5m ./...
	@echo "Linting complete!"

# Format code with gofumpt and goimports
format:
	@echo "Organizing imports with goimports..."
	@go tool goimports -local github.com/ademidoff/supavisor -w .
	@echo "Formatting code with gofumpt..."
	@go tool gofumpt -w .
	@echo "Formatting complete!"

# Clean build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf bin/
	@rm -f $(COVERAGE_FILE)
	@go clean
	@echo "Clean complete!"

run:
	@echo "Running supavisor..."
	@./bin/supavisor -c supavisor.yml &
	@echo "Running sctl..."
	@./bin/sctl status
