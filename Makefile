.PHONY: build test clean run lint lint-install format

# Version of golangci-lint to use
GOLANGCI_LINT_VERSION := v2.12.2

# Path to golangci-lint binary
GOLANGCI_LINT := $(shell if [ -f ./bin/golangci-lint ]; then echo ./bin/golangci-lint || echo ""; fi)

# Build the binaries
build:
	@echo "Formatting the code..."
	@make format
	@echo "Building supavisor..."
	@go build -o bin/supavisor ./cmd/supavisor
	@echo "Building sctl..."
	@go build -o bin/sctl ./cmd/sctl
	@echo "Build complete."
	@ls -l bin/

# Run all tests
test:
	@echo "Running tests..."
	@go test -v ./...

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
	@if [ -z "$(GOLANGCI_LINT)" ]; then \
		echo "Error: could not find golangci-lint in ./bin/"; \
		exit 1; \
	fi; \
	$(GOLANGCI_LINT) run --timeout=5m ./...
	@echo "Linting complete!"

# Format code with gofumpt and goimports
format:
	@echo "Formatting code with gofumpt..."
	@go tool gofumpt -w .
	@echo "Organizing imports with goimports..."
	@go tool goimports -local github.com/ademidoff/supavisor -w .
	@echo "Formatting complete!"

# Clean build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf bin/
	@go clean
	@echo "Clean complete!"

run:
	@echo "Running supavisor..."
	@./bin/supavisor -c supavisor.yml &
	@echo "Running sctl..."
	@./bin/sctl status
