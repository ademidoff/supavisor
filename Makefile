.PHONY: build test clean run lint lint-install

# Version of golangci-lint to use
GOLANGCI_LINT_VERSION := v2.7.2

# Path to golangci-lint binary
GOLANGCI_LINT := $(shell if [ -f ./bin/golangci-lint ]; then echo ./bin/golangci-lint || echo ""; fi)

# Build both supavisor and sctl binaries
build:
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

# Install golangci-lint if not present
lint-install:
	@if [ -z "$(GOLANGCI_LINT)" ]; then \
		echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..."; \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b ./bin $(GOLANGCI_LINT_VERSION); \
		echo "golangci-lint installed successfully"; \
	else \
		echo "golangci-lint is already installed at $(GOLANGCI_LINT)"; \
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

# Clean build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf bin/
	@go clean
	@echo "Clean complete!"

run:
	@echo "Running supavisor..."
	@./bin/supavisor -c supavisor.conf &
	@echo "Running sctl..."
	@./bin/sctl status
