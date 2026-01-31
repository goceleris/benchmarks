# Celeris Benchmarks

Reproducible HTTP server benchmarks on bare-metal AWS instances. Compare production Go frameworks against theoretical maximum performance using raw syscalls.

## Why This Exists

Most benchmarks run on shared VMs with noisy neighbors, making results unreliable. This suite runs on dedicated bare-metal instances (c6g.metal, c5.metal) with automated CI/CD, so every release gets consistent, comparable numbers.

We test two categories:
- **Baseline**: Production frameworks (Gin, Fiber, Echo, Chi, Iris, stdlib)
- **Theoretical**: Raw epoll/io_uring implementations showing the performance ceiling

## Latest Results

Results are committed to [`results/`](results/) on each release. See benchmark charts and raw data for:
- ARM64 (Graviton) on c6g.metal
- x86-64 (Intel) on c5.metal

## Servers Tested

### Production Frameworks

| Server | Protocol | Framework |
|--------|----------|-----------|
| stdhttp | HTTP/1.1, H2C | Go stdlib |
| fiber | HTTP/1.1 | [Fiber](https://github.com/gofiber/fiber) |
| gin | HTTP/1.1, H2C | [Gin](https://github.com/gin-gonic/gin) |
| chi | HTTP/1.1, H2C | [Chi](https://github.com/go-chi/chi) |
| echo | HTTP/1.1, H2C | [Echo](https://github.com/labstack/echo) |
| iris | HTTP/1.1, H2C | [Iris](https://github.com/kataras/iris) |

### Theoretical Maximum

| Server | Protocol | Implementation |
|--------|----------|----------------|
| epoll | HTTP/1.1, H2C | Raw epoll syscalls |
| iouring | HTTP/1.1, H2C | io_uring with multishot (kernel 6.15+) |

## Quick Start

```bash
# Clone and build
git clone https://github.com/goceleris/benchmarks
cd benchmarks
make build

# Run a quick local benchmark
make benchmark-quick

# Run full benchmark (30s per server)
make benchmark
```

### Benchmark Tool

```bash
./bin/bench -mode baseline -duration 30s -connections 256
./bin/bench -mode theoretical -duration 30s -connections 256
./bin/bench -mode all -duration 60s -connections 512
```

## Benchmark Types

| Type | Endpoint | Description |
|------|----------|-------------|
| Simple | `GET /` | Plain text response |
| JSON | `GET /json` | JSON serialization |
| Path | `GET /users/:id` | Path parameter extraction |
| Upload | `POST /upload` | 4KB request body |

## CI/CD

Benchmarks run automatically:
- **On Release**: Full metal benchmark on bare-metal instances
- **On PR** (with label): Quick validation on smaller instances

The C2 orchestration system manages AWS spot instances, handles capacity fallbacks, and commits results automatically.

## Infrastructure

Benchmarks run on AWS using a C2 (command and control) server that:
- Provisions spot instances with on-demand fallback across multiple regions
- Selects regions dynamically based on spot pricing and vCPU quota availability
- Coordinates server/client workers across availability zones
- Logs region/AZ placement for each worker in workflow output
- Collects results and generates charts
- Cleans up resources automatically

PRs can deploy their own C2 server by adding the `deploy-c2` label, enabling testing of C2 code changes in isolation.

Required AWS quotas for metal benchmarks:
- ARM64: 64 vCPU (c6g.metal)
- x86: 96 vCPU (c5.metal)

## Contributing

1. Fork and create a feature branch
2. Run `make check` before submitting
3. Add `run-benchmarks` label to PRs for benchmark validation

### Adding a Server

1. Create package in `servers/baseline/` or `servers/theoretical/`
2. Implement all benchmark endpoints
3. Register in `cmd/server/main.go`

## License

Apache 2.0
