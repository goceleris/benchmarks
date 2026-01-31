# Celeris Benchmark Suite Makefile
# ================================

.PHONY: all build build-server build-bench build-c2 build-linux build-linux-arm lint fmt vet test benchmark benchmark-quick clean deps check help

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOVET=$(GOCMD) vet
GOFMT=gofmt
SERVER_BINARY=server
BENCH_BINARY=bench
C2_BINARY=c2
BINARY_DIR=bin

# Colors for output
GREEN=\033[0;32m
YELLOW=\033[1;33m
NC=\033[0m # No Color

# Default target
all: lint build

## help: Show this help message
help:
	@echo "Celeris Benchmark Suite - Available targets:"
	@echo ""
	@echo "  make build           - Build server, bench, and c2 binaries"
	@echo "  make build-server    - Build the server binary only"
	@echo "  make build-bench     - Build the benchmark tool only"
	@echo "  make build-c2        - Build the C2 server only"
	@echo "  make build-linux    - Cross-compile for Linux amd64"
	@echo "  make build-linux-arm - Cross-compile for Linux arm64"
	@echo "  make lint           - Run golangci-lint"
	@echo "  make fmt            - Format Go code"
	@echo "  make vet            - Run go vet"
	@echo "  make test           - Run unit tests"
	@echo "  make benchmark      - Run benchmarks (30s per server)"
	@echo "  make benchmark-quick - Quick benchmark validation (5s)"
	@echo "  make deps           - Download Go dependencies"
	@echo "  make check          - Run all checks (lint, vet, build)"
	@echo "  make clean          - Clean build artifacts"
	@echo ""

## build: Build server, benchmark, and C2 server binaries
build: build-server build-bench build-c2

## build-server: Build the server binary for current platform
build-server:
	@echo "$(GREEN)Building server...$(NC)"
	@mkdir -p $(BINARY_DIR)
	$(GOBUILD) -o $(BINARY_DIR)/$(SERVER_BINARY) ./cmd/server
	@echo "$(GREEN)Server build complete: $(BINARY_DIR)/$(SERVER_BINARY)$(NC)"

## build-bench: Build the benchmark tool for current platform
build-bench:
	@echo "$(GREEN)Building benchmark tool...$(NC)"
	@mkdir -p $(BINARY_DIR)
	$(GOBUILD) -o $(BINARY_DIR)/$(BENCH_BINARY) ./cmd/bench
	@echo "$(GREEN)Benchmark tool build complete: $(BINARY_DIR)/$(BENCH_BINARY)$(NC)"

## build-c2: Build the C2 server for current platform
build-c2:
	@echo "$(GREEN)Building C2 server...$(NC)"
	@mkdir -p $(BINARY_DIR)
	$(GOBUILD) -o $(BINARY_DIR)/$(C2_BINARY) ./cmd/c2
	@echo "$(GREEN)C2 server build complete: $(BINARY_DIR)/$(C2_BINARY)$(NC)"

## build-linux: Cross-compile for Linux (amd64)
build-linux:
	@echo "$(GREEN)Building for Linux amd64...$(NC)"
	@mkdir -p $(BINARY_DIR)
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o $(BINARY_DIR)/$(SERVER_BINARY)-linux-amd64 ./cmd/server
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o $(BINARY_DIR)/$(BENCH_BINARY)-linux-amd64 ./cmd/bench
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o $(BINARY_DIR)/$(C2_BINARY)-linux-amd64 ./cmd/c2
	@echo "$(GREEN)Linux amd64 build complete$(NC)"

## build-linux-arm: Cross-compile for Linux (arm64)
build-linux-arm:
	@echo "$(GREEN)Building for Linux arm64...$(NC)"
	@mkdir -p $(BINARY_DIR)
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o $(BINARY_DIR)/$(SERVER_BINARY)-linux-arm64 ./cmd/server
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o $(BINARY_DIR)/$(BENCH_BINARY)-linux-arm64 ./cmd/bench
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o $(BINARY_DIR)/$(C2_BINARY)-linux-arm64 ./cmd/c2
	@echo "$(GREEN)Linux arm64 build complete$(NC)"

## lint: Run golangci-lint
lint:
	@echo "$(GREEN)Running golangci-lint...$(NC)"
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run --timeout=5m ./...; \
		echo "$(GREEN)Linting complete$(NC)"; \
	else \
		echo "$(YELLOW)golangci-lint not installed. Installing...$(NC)"; \
		go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest; \
		golangci-lint run --timeout=5m ./...; \
		echo "$(GREEN)Linting complete$(NC)"; \
	fi

## fmt: Format Go code
fmt:
	@echo "$(GREEN)Formatting Go code...$(NC)"
	$(GOFMT) -s -w .
	@echo "$(GREEN)Formatting complete$(NC)"

## vet: Run go vet
vet:
	@echo "$(GREEN)Running go vet...$(NC)"
	$(GOVET) ./...
	@echo "$(GREEN)Vet complete$(NC)"

## test: Run unit tests
test:
	@echo "$(GREEN)Running tests...$(NC)"
	$(GOTEST) -v ./...
	@echo "$(GREEN)Tests complete$(NC)"

## benchmark: Run benchmarks using Go benchmark tool
benchmark: build
	@echo "$(GREEN)Running benchmarks...$(NC)"
	./$(BINARY_DIR)/$(BENCH_BINARY) -mode baseline -duration 30s

## benchmark-quick: Quick benchmark for validation
benchmark-quick: build
	@echo "$(GREEN)Running quick benchmark validation...$(NC)"
	./$(BINARY_DIR)/$(BENCH_BINARY) -mode baseline -duration 5s

## deps: Download Go dependencies
deps:
	@echo "$(GREEN)Downloading dependencies...$(NC)"
	$(GOCMD) mod download
	$(GOCMD) mod tidy
	@echo "$(GREEN)Dependencies ready$(NC)"

## check: Run all checks (lint, vet, build)
check: deps lint vet build
	@echo "$(GREEN)All checks passed$(NC)"

## clean: Clean build artifacts
clean:
	@echo "$(GREEN)Cleaning...$(NC)"
	rm -rf $(BINARY_DIR)
	rm -rf results/*.json results/*.png results/charts/
	@echo "$(GREEN)Clean complete$(NC)"
