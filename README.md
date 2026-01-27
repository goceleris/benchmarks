# Celeris Benchmarks

Official reproducible benchmark suite for comparing Go HTTP server throughput and latency. Tests production frameworks against theoretical maximum implementations using raw syscalls.

## Purpose

This benchmark suite provides:

- **Reproducible results** on bare-metal AWS instances (no noisy neighbors)
- **Fair comparisons** with identical test conditions for all servers
- **Theoretical baselines** using epoll/io_uring to show maximum achievable performance
- **Automated CI/CD** with results committed to the repository

## Server Implementations

### Baseline (Production Frameworks)

| Server | Protocol | Framework |
|--------|----------|-----------|
| stdhttp-h1 | HTTP/1.1 | Go standard library |
| stdhttp-h2 | HTTP/2 (H2C) | `golang.org/x/net/http2` |
| stdhttp-hybrid | HTTP/1.1 + H2C | Auto-detection |
| fiber-h1 | HTTP/1.1 | [Fiber](https://github.com/gofiber/fiber) v2 |
| gin-h1 | HTTP/1.1 | [Gin](https://github.com/gin-gonic/gin) |
| chi-h1 | HTTP/1.1 | [Chi](https://github.com/go-chi/chi) |
| echo-h1 | HTTP/1.1 | [Echo](https://github.com/labstack/echo) |
| iris-h2 | HTTP/2 (H2C) | [Iris](https://github.com/kataras/iris) |

### Theoretical Maximum (Raw Syscalls)

| Server | Protocol | Implementation |
|--------|----------|----------------|
| epoll-h1 | HTTP/1.1 | Raw `epoll` syscalls |
| epoll-h2 | HTTP/2 (H2C) | Raw `epoll` + HPACK |
| epoll-hybrid | HTTP/1.1 + H2C | Raw `epoll` with auto-detection |
| iouring-h1 | HTTP/1.1 | `io_uring` with multishot |
| iouring-h2 | HTTP/2 (H2C) | `io_uring` + HPACK |
| iouring-hybrid | HTTP/1.1 + H2C | `io_uring` with auto-detection |

> **Note**: io_uring servers require Linux kernel 6.15+ for multishot support.

## Benchmark Types

| Type | Method | Endpoint | Description |
|------|--------|----------|-------------|
| Simple | GET | `/` | Plain text "Hello, World!" response |
| JSON | GET | `/json` | JSON object serialization |
| Path | GET | `/users/:id` | Path parameter extraction |
| Big Request | POST | `/upload` | 4KB request body handling |

## Benchmark Modes

The CI/CD system supports multiple benchmark modes with automatic fallback:

| Mode | ARM64 Instance | x86 Instance | Trigger | Purpose |
|------|----------------|--------------|---------|---------|
| **Fast** | c6g.medium (1 vCPU) | c5.large (2 vCPU) | Pull Requests | Quick trend validation |
| **Metal** | c6g.metal (64 vCPU) | c5.metal (96 vCPU) | Releases | Official results |
| **Provisional** | c6g.2xlarge (8 vCPU) | c5.2xlarge (8 vCPU) | Fallback | When metal quota unavailable |

### Instance Fallback Chain

When running Metal benchmarks, each architecture independently attempts:

1. **Metal Spot** - Cheapest option for bare-metal
2. **Metal On-Demand** - Guaranteed availability at higher cost
3. **Provisional Spot** - Best available within typical quotas
4. **Provisional On-Demand** - Guaranteed fallback

This allows ARM64 to run on metal while x86 falls back to provisional (or vice versa) based on available AWS quota.

## Results Structure

Results are stored with clear official/provisional distinction:

```
results/
├── latest/                       # Most recent results
│   ├── arm64/                    # Official ARM64 results (metal)
│   ├── x86-provisional/          # Provisional x86 results (not metal)
│   └── BENCHMARK_INFO.json       # Metadata about each architecture
├── v0.1.0/                       # Results for specific release
│   ├── arm64/
│   ├── x86/                      # Both official
│   └── BENCHMARK_INFO.json
├── v0.2.0/
│   ├── arm64/
│   └── x86-provisional/          # x86 needs promotion
└── PROVISIONAL_STATUS.json       # Tracks what needs promotion
```

### Promoting Provisional Results

When metal quota becomes available, use the **Promote Provisional** workflow:

1. Select version to promote (e.g., `v0.1.0`)
2. Select architecture (`arm64`, `x86`, or `both`)
3. Workflow checks out the specific tag and runs on metal
4. Replaces `x86-provisional/` with `x86/`
5. Only updates `results/latest/` if promoting the most recent version

## GitHub Actions Workflows

| Workflow | Trigger | Description |
|----------|---------|-------------|
| **Benchmark (Fast)** | PR with `run-benchmarks` label | Quick validation on virtualized instances |
| **Benchmark (Metal)** | Release published, manual | Official results on bare-metal |
| **Promote Provisional** | Manual | Re-run specific version/arch on metal |
| **Lint** | Push, PR | Code quality checks with golangci-lint |

### Security

All benchmark workflows require authorization to protect AWS resources:

- **Fast mode**: Requires `run-benchmarks` label (maintainers only can add)
- **Metal mode**: Requires write/maintain/admin repository permissions
- **Promote**: Requires write/maintain/admin repository permissions

## Local Development

### Prerequisites

- Go 1.25+
- Docker (for container validation)
- Python 3.11+ with matplotlib, numpy (for chart generation)
- Linux kernel 6.15+ (for io_uring servers)
- Terraform 1.6+ (for infrastructure management)

### Quick Start

```bash
# Clone repository
git clone https://github.com/goceleris/benchmarks
cd benchmarks

# Build binaries
make build

# Run quick benchmark validation
make benchmark-quick

# Run full baseline benchmark
make benchmark
```

### Available Make Targets

```bash
make help              # Show all available targets

# Building
make build             # Build server and benchmark binaries
make build-server      # Build server only
make build-bench       # Build benchmark tool only
make build-linux       # Cross-compile for Linux amd64
make build-linux-arm   # Cross-compile for Linux arm64

# Testing
make lint              # Run golangci-lint
make fmt               # Format Go code
make vet               # Run go vet
make test              # Run unit tests
make check             # Run all checks

# Benchmarking
make benchmark         # Run baseline benchmarks (30s)
make benchmark-quick   # Quick validation (5s)
make bench-charts      # Test chart generation

# Docker
make docker-build           # Build all Docker images
make docker-test-baseline   # Test baseline servers
make docker-test-theoretical # Test theoretical servers (Linux)
make docker-stop            # Stop all containers

# Validation
make validate          # Validate workflows and Terraform
make validate-tf       # Validate Terraform only
make validate-workflows # Validate GitHub Actions only

# Cleanup
make clean             # Remove build artifacts
make deps              # Download/tidy dependencies
```

### Running Benchmarks Manually

```bash
# Build everything
make build

# Run baseline servers only
./bin/bench -mode baseline -duration 30s -connections 256 -workers 8

# Run theoretical servers only (Linux)
./bin/bench -mode theoretical -duration 30s -connections 256 -workers 8

# Run all servers
./bin/bench -mode all -duration 60s -connections 512 -workers 16

# Custom configuration
./bin/bench \
  -mode baseline \
  -duration 60s \
  -connections 1024 \
  -workers 16 \
  -warmup 5s
```

### Benchmark Tool Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-mode` | `all` | Servers to test: `baseline`, `theoretical`, `all` |
| `-duration` | `30s` | Duration per benchmark |
| `-connections` | `256` | Concurrent connections |
| `-workers` | `8` | Worker goroutines |
| `-warmup` | `3s` | Warmup duration before measurement |

### Docker Validation

```bash
# Test baseline servers
make docker-test-baseline

# Test theoretical servers (requires Linux with kernel 6.15+)
make docker-test-theoretical

# Manual Docker testing
docker compose -f docker/docker-compose.yml --profile baseline up -d
curl http://localhost:8081/        # stdhttp-h1
curl http://localhost:8084/        # fiber-h1
curl http://localhost:8086/        # gin-h1
docker compose -f docker/docker-compose.yml --profile baseline down
```

## Infrastructure

### AWS Resources

The benchmark infrastructure uses Terraform to provision:

- **EC2 Spot/On-Demand Instances** - Self-hosted GitHub Actions runners
- **IAM Instance Profile** - Minimal permissions for runner registration
- **Security Groups** - Outbound-only access

### Terraform Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `benchmark_mode` | Instance type selection | `fast` |
| `use_on_demand` | Use on-demand instead of spot | `false` |
| `launch_arm64_only` | Only launch ARM64 | `false` |
| `launch_x86_only` | Only launch x86 | `false` |
| `aws_region` | AWS region | `us-east-1` |

### Required AWS Quotas

For metal benchmarks, ensure sufficient vCPU quota:

| Quota | ARM64 | x86 |
|-------|-------|-----|
| Metal Spot | 64 vCPU (c6g.metal) | 96 vCPU (c5.metal) |
| Metal On-Demand | 64 vCPU | 96 vCPU |
| Provisional | 8 vCPU (c6g.2xlarge) | 8 vCPU (c5.2xlarge) |

Request quota increases in AWS Service Quotas for:
- "Running On-Demand Standard (A, C, D, H, I, M, R, T, Z) instances"
- "All Standard (A, C, D, H, I, M, R, T, Z) Spot Instance Requests"
- "Running On-Demand G and VT instances" (for ARM64)
- "All G and VT Spot Instance Requests" (for ARM64)

## Project Structure

```
.
├── cmd/
│   ├── bench/           # Benchmark tool
│   └── server/          # Multi-server binary
├── docker/
│   ├── Dockerfile.baseline
│   ├── Dockerfile.theoretical
│   └── docker-compose.yml
├── internal/
│   └── bench/           # Benchmark library
├── results/             # Benchmark results (committed)
├── scripts/
│   └── generate_charts.py
├── servers/
│   ├── baseline/        # Production frameworks
│   │   ├── chi/
│   │   ├── echo/
│   │   ├── fiber/
│   │   ├── gin/
│   │   ├── iris/
│   │   └── stdhttp/
│   ├── common/          # Shared utilities
│   └── theoretical/     # Raw syscall implementations
│       ├── epoll/
│       └── iouring/
├── terraform/           # AWS infrastructure
├── .github/workflows/   # CI/CD pipelines
├── Makefile
└── go.mod
```

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make changes and run `make check`
4. Submit a pull request
5. Add `run-benchmarks` label to trigger benchmark validation

### Adding a New Server

1. Create server package in `servers/baseline/` or `servers/theoretical/`
2. Implement the server interface with all benchmark endpoints
3. Register in `cmd/server/main.go`
4. Update this README with the new server

### Code Style

- Run `make fmt` before committing
- Run `make lint` to check for issues
- Follow existing patterns in the codebase

## License

Apache 2.0 - See [LICENSE](LICENSE) for details.
