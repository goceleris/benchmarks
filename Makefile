# Celeris Benchmark Suite Makefile
# ================================

.PHONY: all build build-bench build-control lint test docker-build docker-test validate clean help

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOVET=$(GOCMD) vet
GOFMT=gofmt
SERVER_BINARY=server
BENCH_BINARY=bench
CONTROL_BINARY=control
BINARY_DIR=bin

# Docker parameters
DOCKER=docker
DOCKER_COMPOSE=docker compose

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
	@echo "  make build          - Build server and benchmark binaries"
	@echo "  make build-server   - Build the server binary only"
	@echo "  make build-bench    - Build the benchmark tool only"
	@echo "  make build-linux    - Cross-compile for Linux"
	@echo "  make lint           - Run golangci-lint"
	@echo "  make fmt            - Format Go code"
	@echo "  make vet            - Run go vet"
	@echo "  make test           - Run unit tests"
	@echo "  make benchmark      - Run benchmarks (requires server running)"
	@echo "  make docker-build   - Build Docker images"
	@echo "  make docker-test    - Run servers in Docker for validation"
	@echo "  make validate       - Validate workflows and Terraform"
	@echo "  make clean          - Clean build artifacts"
	@echo ""

## build: Build server, benchmark, and control daemon binaries
build: build-server build-bench build-control

## build-server: Build the server binary for current platform
build-server:
	@echo "$(GREEN)Building server...$(NC)"
	@mkdir -p $(BINARY_DIR)
	$(GOBUILD) -o $(BINARY_DIR)/$(SERVER_BINARY) ./cmd/server
	@echo "$(GREEN)✓ Server build complete: $(BINARY_DIR)/$(SERVER_BINARY)$(NC)"

## build-bench: Build the benchmark tool for current platform
build-bench:
	@echo "$(GREEN)Building benchmark tool...$(NC)"
	@mkdir -p $(BINARY_DIR)
	$(GOBUILD) -o $(BINARY_DIR)/$(BENCH_BINARY) ./cmd/bench
	@echo "$(GREEN)✓ Benchmark tool build complete: $(BINARY_DIR)/$(BENCH_BINARY)$(NC)"

## build-control: Build the control daemon for current platform
build-control:
	@echo "$(GREEN)Building control daemon...$(NC)"
	@mkdir -p $(BINARY_DIR)
	$(GOBUILD) -o $(BINARY_DIR)/$(CONTROL_BINARY) ./cmd/control
	@echo "$(GREEN)✓ Control daemon build complete: $(BINARY_DIR)/$(CONTROL_BINARY)$(NC)"

## build-linux: Cross-compile for Linux (amd64)
build-linux:
	@echo "$(GREEN)Building for Linux amd64...$(NC)"
	@mkdir -p $(BINARY_DIR)
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o $(BINARY_DIR)/$(SERVER_BINARY)-linux-amd64 ./cmd/server
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o $(BINARY_DIR)/$(BENCH_BINARY)-linux-amd64 ./cmd/bench
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o $(BINARY_DIR)/$(CONTROL_BINARY)-linux-amd64 ./cmd/control
	@echo "$(GREEN)✓ Linux build complete$(NC)"

## build-linux-arm: Cross-compile for Linux (arm64)
build-linux-arm:
	@echo "$(GREEN)Building for Linux arm64...$(NC)"
	@mkdir -p $(BINARY_DIR)
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o $(BINARY_DIR)/$(SERVER_BINARY)-linux-arm64 ./cmd/server
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o $(BINARY_DIR)/$(BENCH_BINARY)-linux-arm64 ./cmd/bench
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o $(BINARY_DIR)/$(CONTROL_BINARY)-linux-arm64 ./cmd/control
	@echo "$(GREEN)✓ Linux ARM build complete$(NC)"

## lint: Run golangci-lint
lint:
	@echo "$(GREEN)Running golangci-lint...$(NC)"
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run --timeout=5m ./...; \
		echo "$(GREEN)✓ Linting complete$(NC)"; \
	else \
		echo "$(YELLOW)golangci-lint not installed. Installing...$(NC)"; \
		go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest; \
		golangci-lint run --timeout=5m ./...; \
		echo "$(GREEN)✓ Linting complete$(NC)"; \
	fi

## fmt: Format Go code
fmt:
	@echo "$(GREEN)Formatting Go code...$(NC)"
	$(GOFMT) -s -w .
	@echo "$(GREEN)✓ Formatting complete$(NC)"

## vet: Run go vet
vet:
	@echo "$(GREEN)Running go vet...$(NC)"
	$(GOVET) ./...
	@echo "$(GREEN)✓ Vet complete$(NC)"

## test: Run unit tests
test:
	@echo "$(GREEN)Running tests...$(NC)"
	$(GOTEST) -v ./...
	@echo "$(GREEN)✓ Tests complete$(NC)"

## benchmark: Run benchmarks using Go benchmark tool (auto-scales based on CPU count)
benchmark: build
	@echo "$(GREEN)Running benchmarks (auto-scaled to $$(nproc 2>/dev/null || sysctl -n hw.ncpu) CPUs)...$(NC)"
	./$(BINARY_DIR)/$(BENCH_BINARY) -mode baseline -duration 30s

## benchmark-quick: Quick benchmark for validation (auto-scales based on CPU count)
benchmark-quick: build
	@echo "$(GREEN)Running quick benchmark validation (auto-scaled to $$(nproc 2>/dev/null || sysctl -n hw.ncpu) CPUs)...$(NC)"
	./$(BINARY_DIR)/$(BENCH_BINARY) -mode baseline -duration 5s

## bench-charts: Test chart generation with sample data
bench-charts:
	@echo "$(GREEN)Testing chart generation...$(NC)"
	@mkdir -p results/test
	@echo '{"timestamp":"2026-01-10T12_00_00Z","architecture":"test","config":{"duration":"5s","connections":10,"workers":2},"results":[{"server":"stdhttp-h1","benchmark":"simple","method":"GET","path":"/","requests_per_sec":50000,"transfer_per_sec":"10MB","latency":{"avg":"1ms","max":"10ms","p50":"0.8ms","p75":"1.2ms","p90":"2ms","p99":"5ms"}},{"server":"gin-h1","benchmark":"simple","method":"GET","path":"/","requests_per_sec":55000,"transfer_per_sec":"11MB","latency":{"avg":"0.9ms","max":"9ms","p50":"0.7ms","p75":"1.1ms","p90":"1.8ms","p99":"4.5ms"}}]}' > results/test/benchmark-test-sample.json
	@if command -v uv >/dev/null 2>&1; then \
		uv run --with matplotlib --with numpy python scripts/generate_charts.py results/test results/test 2>&1 && \
		echo "$(GREEN)✓ Charts generated in results/test/$(NC)"; \
	elif command -v python3 >/dev/null 2>&1; then \
		python3 scripts/generate_charts.py results/test results/test 2>&1 || \
		echo "$(YELLOW)⚠ Install matplotlib/numpy: pip install matplotlib numpy$(NC)"; \
	else \
		echo "$(YELLOW)⚠ python3/uv not installed, skipping chart test$(NC)"; \
	fi

## docker-build: Build Docker images
docker-build:
	@echo "$(GREEN)Building Docker images...$(NC)"
	$(DOCKER) build -f docker/Dockerfile.baseline -t celeris-bench-baseline .
	@echo "$(GREEN)✓ Baseline image built$(NC)"
	@echo "$(GREEN)Building theoretical image (Linux only)...$(NC)"
	$(DOCKER) build -f docker/Dockerfile.theoretical -t celeris-bench-theoretical . || \
		echo "$(YELLOW)⚠ Theoretical image may require Linux host$(NC)"
	@echo "$(GREEN)✓ Docker build complete$(NC)"

## docker-test: Run all servers in Docker for validation
docker-test: docker-test-baseline docker-test-theoretical
	@echo "$(GREEN)✓ All Docker tests complete$(NC)"

## docker-test-baseline: Run baseline servers in Docker
docker-test-baseline:
	@echo "$(GREEN)Starting baseline servers in Docker...$(NC)"
	$(DOCKER_COMPOSE) -f docker/docker-compose.yml --profile baseline up -d --build
	@echo "Waiting for servers to start..."
	@sleep 3
	@echo "$(GREEN)Testing baseline endpoints...$(NC)"
	@curl -s http://localhost:8081/ && echo " - stdhttp-h1 ✓" || echo " - stdhttp-h1 ✗"
	@curl -s http://localhost:8082/ && echo " - stdhttp-h2 ✓" || echo " - stdhttp-h2 ✗"
	@curl -s http://localhost:8083/ && echo " - stdhttp-hybrid ✓" || echo " - stdhttp-hybrid ✗"
	@curl -s http://localhost:8084/ && echo " - fiber-h1 ✓" || echo " - fiber-h1 ✗"
	@curl -s http://localhost:8085/ && echo " - iris-h2 ✓" || echo " - iris-h2 ✗"
	@echo "$(GREEN)Stopping baseline containers...$(NC)"
	$(DOCKER_COMPOSE) -f docker/docker-compose.yml --profile baseline down
	@echo "$(GREEN)✓ Baseline Docker test complete$(NC)"

## docker-test-theoretical: Run theoretical servers in Docker (Linux only)
docker-test-theoretical:
	@echo "$(GREEN)Starting theoretical servers in Docker...$(NC)"
	@echo "$(YELLOW)Note: io_uring servers require Linux kernel 6.15+$(NC)"
	$(DOCKER_COMPOSE) -f docker/docker-compose.yml --profile theoretical up -d --build || \
		(echo "$(YELLOW)⚠ Theoretical servers may not work on this platform$(NC)" && exit 0)
	@echo "Waiting for servers to start..."
	@sleep 5
	@echo "$(GREEN)Testing theoretical endpoints...$(NC)"
	@curl -s --max-time 2 http://localhost:8091/ && echo " - epoll-h1 ✓" || echo " - epoll-h1 ✗"
	@curl -s --max-time 2 http://localhost:8093/ && echo " - epoll-hybrid ✓" || echo " - epoll-hybrid ✗"
	@curl -s --max-time 2 --http2-prior-knowledge http://localhost:8092/ && echo " - epoll-h2 ✓" || echo " - epoll-h2 ✗"
	@echo "$(GREEN)Stopping theoretical containers...$(NC)"
	$(DOCKER_COMPOSE) -f docker/docker-compose.yml --profile theoretical down
	@echo "$(GREEN)✓ Theoretical Docker test complete$(NC)"

## docker-stop: Stop all Docker containers
docker-stop:
	@echo "$(GREEN)Stopping all containers...$(NC)"
	$(DOCKER_COMPOSE) -f docker/docker-compose.yml --profile baseline --profile theoretical down
	@echo "$(GREEN)✓ Containers stopped$(NC)"

## validate: Validate workflows and Terraform
validate: validate-workflows validate-tf
	@echo "$(GREEN)✓ All validations passed$(NC)"

## validate-tf: Validate Terraform configuration
validate-tf:
	@echo "$(GREEN)Validating Terraform...$(NC)"
	@if command -v terraform >/dev/null 2>&1; then \
		cd terraform && terraform init -backend=false >/dev/null 2>&1 && terraform validate; \
		echo "$(GREEN)✓ Terraform validation complete$(NC)"; \
	else \
		echo "$(YELLOW)⚠ Terraform not installed, skipping validation$(NC)"; \
	fi

## validate-workflows: Validate GitHub Actions workflows
validate-workflows:
	@echo "$(GREEN)Validating GitHub Actions workflows...$(NC)"
	@if command -v actionlint >/dev/null 2>&1; then \
		actionlint .github/workflows/*.yml; \
		echo "$(GREEN)✓ Workflow validation complete$(NC)"; \
	elif command -v uv >/dev/null 2>&1; then \
		for f in .github/workflows/*.yml; do \
			uv run --with pyyaml python -c "import yaml; yaml.safe_load(open('$$f'))" && echo "  $$f: valid YAML" || echo "  $$f: INVALID"; \
		done; \
		echo "$(GREEN)✓ YAML validation complete$(NC)"; \
	else \
		echo "$(YELLOW)⚠ actionlint/uv not installed, skipping validation$(NC)"; \
	fi

## clean: Clean build artifacts
clean:
	@echo "$(GREEN)Cleaning...$(NC)"
	rm -rf $(BINARY_DIR)
	rm -rf results/*.json results/*.png results/charts/
	$(DOCKER_COMPOSE) -f docker/docker-compose.yml --profile baseline --profile theoretical down --rmi local 2>/dev/null || true
	@echo "$(GREEN)✓ Clean complete$(NC)"

## deps: Download Go dependencies
deps:
	@echo "$(GREEN)Downloading dependencies...$(NC)"
	$(GOCMD) mod download
	$(GOCMD) mod tidy
	@echo "$(GREEN)✓ Dependencies ready$(NC)"

## check: Run all checks (lint, vet, build, validate)
check: deps lint vet build validate
	@echo "$(GREEN)✓ All checks passed$(NC)"
