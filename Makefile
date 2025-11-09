.PHONY: build fmt lint test clean docker-test docker-test-advanced docker-up docker-down help

# Variables
BINARY_NAME=sauron
GO=go
GOFLAGS=-v
DOCKER_DIR=.docker

# Default target
all: fmt lint build

## build: Build the binary
build:
	@echo "Building $(BINARY_NAME)..."
	$(GO) build $(GOFLAGS) -o bin/$(BINARY_NAME) .

## fmt: Format Go code
fmt:
	@echo "Formatting code..."
	$(GO) fmt ./...
	gofmt -s -w .
	@if [ -f ~/go/bin/goimports ]; then \
		~/go/bin/goimports -w .; \
	elif command -v goimports >/dev/null 2>&1; then \
		goimports -w .; \
	else \
		echo "goimports not found, skipping import organization"; \
	fi

## lint: Run linters
lint:
	@echo "Running linters..."
	@if [ -f ~/go/bin/golangci-lint ]; then \
		~/go/bin/golangci-lint run ./...; \
	elif command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not found, running go vet instead..."; \
		$(GO) vet ./...; \
	fi

## test: Run unit tests
test:
	@echo "Running tests..."
	$(GO) test -v ./...

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
