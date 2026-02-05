.PHONY: build test test-race testacc clean fmt vet lint deps

# Example binary name (not a usable Packer plugin — compile check only)
BINARY_NAME=psrp-example

# Version info
VERSION?=0.1.0
VERSION_PRERELEASE?=dev

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod

# Build flags
LDFLAGS=-ldflags="-X github.com/smnsjas/packer-psrp-communicator/version.Version=$(VERSION) \
                   -X github.com/smnsjas/packer-psrp-communicator/version.VersionPrerelease=$(VERSION_PRERELEASE)"

all: build

## build: Compile the example binary (verifies the library builds)
build:
	@echo "Building $(BINARY_NAME) (compile check)..."
	$(GOBUILD) $(LDFLAGS) -o $(BINARY_NAME) ./cmd/example

## test: Run unit tests
test:
	@echo "Running unit tests..."
	$(GOTEST) -v ./... -timeout=30s

## test-race: Run tests with race detector
test-race:
	@echo "Running tests with race detector..."
	$(GOTEST) -race -v ./... -timeout=30s

## testacc: Run acceptance tests (requires PACKER_ACC=1)
testacc:
	@echo "Running acceptance tests..."
	@if [ -z "$(PACKER_ACC)" ]; then \
		echo "PACKER_ACC must be set to run acceptance tests"; \
		echo "Usage: PACKER_ACC=1 make testacc"; \
		exit 1; \
	fi
	$(GOTEST) -v ./... -timeout=120m

## clean: Remove build artifacts
clean:
	@echo "Cleaning..."
	$(GOCLEAN)
	@rm -f $(BINARY_NAME)

## fmt: Format Go code
fmt:
	@echo "Formatting code..."
	@$(GOCMD) fmt ./...

## vet: Run go vet
vet:
	@echo "Running go vet..."
	@$(GOCMD) vet ./...

## lint: Run golangci-lint (requires golangci-lint to be installed)
lint:
	@echo "Running golangci-lint..."
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint is not installed. Install it from https://golangci-lint.run/usage/install/"; \
		exit 1; \
	fi

## deps: Download and verify dependencies
deps:
	@echo "Downloading dependencies..."
	$(GOMOD) download
	$(GOMOD) verify
	$(GOMOD) tidy

## help: Display this help message
help:
	@echo "Available targets:"
	@sed -n 's/^## //p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/  /'

.DEFAULT_GOAL := help
