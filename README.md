# Celeris Benchmarks

Official reproducible benchmark suite comparing Celeris throughput and latency against alternatives.

## Overview

This repository contains comprehensive benchmarks for testing HTTP server performance on AWS instances. It compares:

### Baseline Implementations
- **std/http HTTP/1.1** - Go standard library
- **std/http HTTP/2** - H2C with `x/net/http2`
- **std/http Hybrid** - HTTP/1.1 + H2C auto-detection
- **Fiber** - HTTP/1.1 only
- **Iris** - H2C only
- **Gin** - HTTP/1.1
- **Chi** - HTTP/1.1
- **Echo** - HTTP/1.1

### Theoretical Maximum Implementations
- **epoll** - Barebones servers using raw `epoll` syscalls
- **io_uring** - Barebones servers using `io_uring` with multishot

## Benchmark Tool

This suite includes a custom Go benchmarking tool (`cmd/bench`) designed for maximum throughput testing:

- High-concurrency workers using goroutines
- Accurate latency percentiles (p50, p90, p99, p99.9)
- Connection pooling with keep-alive
- Progressive load testing to find max RPS
- JSON output compatible with chart generation

## Benchmark Modes

| Mode | Instances | Trigger | Purpose |
|------|-----------|---------|---------|
| **Fast** | c5.large, c6g.medium | PRs | Quick trend validation |
| **Metal** | c5.metal, c6g.metal | Release / Manual | Official results |

### Fast Mode
- Runs automatically on Pull Requests
- Uses cheaper virtualized instances (same CPU family as metal)
- ~15 second benchmark duration
- Results shown in PR summary for trend validation

### Metal Mode
- Runs on new releases or manual trigger by maintainers
- Uses bare-metal instances for accurate results
- ~30 second benchmark duration
- Official results committed to repository

## Benchmark Types

| Type | Endpoint | Description |
|------|----------|-------------|
| Simple | `GET /` | Plain text response |
| JSON | `GET /json` | JSON serialization |
| Path | `GET /users/:id` | Path parameter extraction |
| Big Request | `POST /upload` | 4KB body handling |

## Running Locally

### Build and Test
```bash
make build           # Build server and benchmark binaries
make lint            # Run golangci-lint
make benchmark-quick # Quick local benchmark test
make bench-charts    # Test chart generation
```

### Run Benchmarks
```bash
# Build everything
make build

# Run baseline benchmarks (30s duration)
./bin/bench -mode baseline -duration 30s -connections 256 -workers 8

# Run all benchmarks with custom settings
./bin/bench -mode all -duration 60s -connections 512 -workers 16
```

### Docker Validation
```bash
make docker-test-baseline     # Test baseline servers
make docker-test-theoretical  # Test theoretical servers (Linux)
```

## Requirements

- Go 1.22+
- Linux kernel 6.15+ (for io_uring multishot)
- AWS credentials (for cloud benchmarks)
- Docker (for local validation)
- Python with matplotlib/numpy (for chart generation)

## Infrastructure

The benchmark infrastructure uses:
- **Terraform** for AWS Spot Instance provisioning
- **GitHub Actions** self-hosted runners (ephemeral)
- **Custom Go benchmark tool** for throughput and latency testing

### Instance Types

| Mode | ARM64 | x86 |
|------|-------|-----|
| Fast | c6g.medium (1 vCPU) | c5.large (2 vCPU) |
| Metal | c6g.metal (Graviton2) | c5.metal (Intel) |

> **Note**: Do not use t3/t4g (burstable) instances - CPU credit throttling will invalidate io_uring benchmarks.

## License

Apache 2.0
