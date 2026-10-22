.PHONY: build fmt fmt-check lint test ci clean docker-test docker-test-advanced docker-up docker-down help

# Variables
BINARY_NAME=sauron
GO=go
GOFLAGS=-v
DOCKER_DIR=.docker

# Tool versions are pinned and run through `go run`, so every machine and CI
# use the same linters, built with the Go toolchain this module requires.
GOLANGCI_LINT_VERSION ?= v2.14.0
GOIMPORTS_VERSION ?= v0.51.0
GOLANGCI_LINT = $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
GOIMPORTS = $(GO) run golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION)

# Default target
all: fmt lint build

## build: Build the binary
build:
	@echo "Building $(BINARY_NAME)..."
	$(GO) build $(GOFLAGS) -o bin/$(BINARY_NAME) .

## fmt: Format Go code (gofmt -s + goimports)
fmt:
	@echo "Formatting code..."
	gofmt -s -w .
	$(GOIMPORTS) -w $$(git ls-files '*.go')

## fmt-check: Fail if gofmt or goimports would change any file
fmt-check:
	@echo "Checking formatting..."
	@out=$$(gofmt -s -l .); if [ -n "$$out" ]; then echo "gofmt would reformat:"; echo "$$out"; exit 1; fi
	@out=$$($(GOIMPORTS) -l $$(git ls-files '*.go')); if [ -n "$$out" ]; then echo "goimports would reformat:"; echo "$$out"; exit 1; fi

## lint: Run golangci-lint (pinned version)
lint:
	@echo "Running linters..."
	$(GOLANGCI_LINT) run --timeout=5m ./...

## test: Run all tests with the race detector
test:
	@echo "Running tests..."
	$(GO) test -race -count=1 ./...

## ci: Run every check CI runs (tests, lint, format)
ci: test lint fmt-check

## clean: Remove built binaries
clean:
	@echo "Cleaning..."
	rm -f $(BINARY_NAME)
	rm -rf bin/
	$(GO) clean

## docker-up: Start Docker Compose environment
docker-up:
	@echo "Starting Docker environment..."
	cd $(DOCKER_DIR) && docker-compose up -d

## docker-down: Stop Docker Compose environment
docker-down:
	@echo "Stopping Docker environment..."
	cd $(DOCKER_DIR) && docker-compose down -v

## docker-test: Run external validation tests
docker-test:
	@echo "Running external validation tests..."
	cd $(DOCKER_DIR) && ./test-external-validation.sh

## docker-test-advanced: Run advanced feature tests
docker-test-advanced:
	@echo "Running advanced feature tests..."
	cd $(DOCKER_DIR) && ./test-advanced-features.sh

## help: Show this help message
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@sed -n 's/^##//p' Makefile | column -t -s ':' | sed -e 's/^/ /'
