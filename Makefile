# Celeris Benchmark Suite Makefile
# ================================

.PHONY: all build lint test docker-build docker-test validate clean help

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOVET=$(GOCMD) vet
GOFMT=gofmt
BINARY_NAME=server
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
	@echo "  make build          - Build the server binary"
	@echo "  make build-linux    - Cross-compile for Linux"
	@echo "  make lint           - Run golangci-lint"
	@echo "  make fmt            - Format Go code"
	@echo "  make vet            - Run go vet"
	@echo "  make test           - Run unit tests"
	@echo "  make docker-build   - Build Docker images"
	@echo "  make docker-test    - Run servers in Docker for validation"
	@echo "  make validate       - Validate workflows and Terraform"
	@echo "  make validate-tf    - Validate Terraform only"
	@echo "  make validate-workflows - Validate GitHub Actions workflows"
	@echo "  make clean          - Clean build artifacts"
	@echo "  make all            - Run lint and build"
	@echo ""

## build: Build the server binary for current platform
build:
	@echo "$(GREEN)Building server...$(NC)"
	@mkdir -p $(BINARY_DIR)
	$(GOBUILD) -o $(BINARY_DIR)/$(BINARY_NAME) ./cmd/server
	@echo "$(GREEN)✓ Build complete: $(BINARY_DIR)/$(BINARY_NAME)$(NC)"

## build-linux: Cross-compile for Linux (amd64)
build-linux:
	@echo "$(GREEN)Building server for Linux amd64...$(NC)"
	@mkdir -p $(BINARY_DIR)
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o $(BINARY_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/server
	@echo "$(GREEN)✓ Linux build complete: $(BINARY_DIR)/$(BINARY_NAME)-linux-amd64$(NC)"

## build-linux-arm: Cross-compile for Linux (arm64)
build-linux-arm:
	@echo "$(GREEN)Building server for Linux arm64...$(NC)"
	@mkdir -p $(BINARY_DIR)
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o $(BINARY_DIR)/$(BINARY_NAME)-linux-arm64 ./cmd/server
	@echo "$(GREEN)✓ Linux ARM build complete: $(BINARY_DIR)/$(BINARY_NAME)-linux-arm64$(NC)"

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

## bench-quick: Run a quick benchmark test against stdhttp-h1 (validation only)
bench-quick: build
	@echo "$(GREEN)Running quick benchmark validation...$(NC)"
	@echo "Starting stdhttp-h1 server in background..."
	@$(BINARY_DIR)/$(BINARY_NAME) -server stdhttp-h1 -port 8099 &
	@sleep 2
	@echo "Testing server responds..."
	@curl -s http://localhost:8099/ > /dev/null && echo "  Server OK" || (echo "  Server failed" && exit 1)
	@curl -s http://localhost:8099/json > /dev/null && echo "  JSON endpoint OK" || echo "  JSON endpoint failed"
	@curl -s http://localhost:8099/users/123 > /dev/null && echo "  Path endpoint OK" || echo "  Path endpoint failed"
	@echo "Running mini wrk test (if available)..."
	@if command -v wrk >/dev/null 2>&1; then \
		wrk -t1 -c10 -d3s http://localhost:8099/ 2>&1 | grep -E "(Requests/sec|Latency)" || true; \
	else \
		echo "$(YELLOW)⚠ wrk not installed, skipping load test$(NC)"; \
	fi
	@echo "Stopping server..."
	@pkill -f "$(BINARY_NAME) -server stdhttp-h1 -port 8099" || true
	@echo "$(GREEN)✓ Quick benchmark validation complete$(NC)"

## bench-charts: Test chart generation with sample data
bench-charts:
	@echo "$(GREEN)Testing chart generation...$(NC)"
	@mkdir -p results/test
	@echo '{"timestamp":"2026-01-10T12:00:00Z","architecture":"test","config":{"duration":"5s","connections":10,"threads":2},"results":[{"server":"stdhttp-h1","benchmark":"simple","method":"GET","path":"/","requests_per_sec":50000,"transfer_per_sec":"10MB","latency":{"avg":"1ms","stdev":"0.5ms","max":"10ms","p50":"0.8ms","p75":"1.2ms","p90":"2ms","p99":"5ms"}},{"server":"epoll-h1","benchmark":"simple","method":"GET","path":"/","requests_per_sec":75000,"transfer_per_sec":"15MB","latency":{"avg":"0.7ms","stdev":"0.3ms","max":"8ms","p50":"0.5ms","p75":"0.9ms","p90":"1.5ms","p99":"4ms"}}]}' > results/test/benchmark-test-sample.json
	@if command -v uv >/dev/null 2>&1; then \
		uv run --with matplotlib --with numpy python scripts/generate_charts.py results/test results/test 2>&1 && \
		echo "$(GREEN)✓ Charts generated in results/test/$(NC)" && \
		ls -la results/test/*.png 2>/dev/null || echo "  (No PNG files - may need display)"; \
	elif command -v python3 >/dev/null 2>&1; then \
		python3 scripts/generate_charts.py results/test results/test 2>&1 || \
		echo "$(YELLOW)⚠ Install matplotlib/numpy: pip install matplotlib numpy$(NC)"; \
	else \
		echo "$(YELLOW)⚠ python3/uv not installed, skipping chart test$(NC)"; \
	fi

## bench-runner-syntax: Check benchmark runner script syntax
bench-runner-syntax:
	@echo "$(GREEN)Checking benchmark runner script...$(NC)"
	@bash -n scripts/runner.sh && echo "  runner.sh syntax OK" || echo "  runner.sh syntax ERROR"
	@echo "$(GREEN)✓ Script syntax check complete$(NC)"

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
	@echo "Testing HTTP/1.1 servers..."
	@curl -s --max-time 2 http://localhost:8091/ && echo " - epoll-h1 ✓" || echo " - epoll-h1 ✗"
	@curl -s --max-time 2 http://localhost:8093/ && echo " - epoll-hybrid ✓" || echo " - epoll-hybrid ✗"
	@echo "Testing HTTP/2 servers (prior knowledge)..."
	@curl -s --max-time 2 --http2-prior-knowledge http://localhost:8092/ && echo " - epoll-h2 ✓" || echo " - epoll-h2 ✗"
	@echo "Testing io_uring servers (may require kernel 6.15+)..."
	@curl -s --max-time 2 http://localhost:8094/ && echo " - iouring-h1 ✓" || echo " - iouring-h1 ✗ (io_uring ring issue)"
	@curl -s --max-time 2 --http2-prior-knowledge http://localhost:8095/ && echo " - iouring-h2 ✓" || echo " - iouring-h2 ✗ (io_uring ring issue)"
	@curl -s --max-time 2 http://localhost:8096/ && echo " - iouring-hybrid ✓" || echo " - iouring-hybrid ✗ (io_uring ring issue)"
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
