#!/bin/bash
set -euo pipefail

# Benchmark Runner for Celeris
# This script runs wrk/wrk2 benchmarks against all server implementations

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
RESULTS_DIR="${PROJECT_ROOT}/results"
SERVER_BIN="${PROJECT_ROOT}/bin/server"

# Configuration
PORT="${PORT:-8080}"
WARMUP_DURATION="${WARMUP_DURATION:-5s}"
BENCHMARK_DURATION="${BENCHMARK_DURATION:-30s}"
CONNECTIONS="${CONNECTIONS:-256}"
THREADS="${THREADS:-8}"
LATENCY_RATE="${LATENCY_RATE:-50000}"  # Requests per second for wrk2

# Server types to benchmark
BASELINE_SERVERS=(
    "stdhttp-h1"
    "stdhttp-h2"
    "stdhttp-hybrid"
    "fiber-h1"
    "iris-h2"
)

THEORETICAL_SERVERS=(
    "epoll-h1"
    "epoll-h2"
    "epoll-hybrid"
    "iouring-h1"
    "iouring-h2"
    "iouring-hybrid"
)

# Benchmark types
BENCHMARK_TYPES=(
    "simple:GET:/"
    "json:GET:/json"
    "path:GET:/users/12345"
    "big-request:POST:/upload"
)

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[OK]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Create results directory
mkdir -p "$RESULTS_DIR"

# Get architecture
ARCH=$(uname -m)
case "$ARCH" in
    x86_64) ARCH_NAME="x86" ;;
    aarch64) ARCH_NAME="arm64" ;;
    arm64) ARCH_NAME="arm64" ;;
    *) ARCH_NAME="$ARCH" ;;
esac

# Initialize results JSON
TIMESTAMP=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
RESULTS_FILE="${RESULTS_DIR}/benchmark-${ARCH_NAME}-${TIMESTAMP}.json"

cat > "$RESULTS_FILE" << EOF
{
    "timestamp": "$TIMESTAMP",
    "architecture": "$ARCH_NAME",
    "config": {
        "duration": "$BENCHMARK_DURATION",
        "connections": $CONNECTIONS,
        "threads": $THREADS
    },
    "results": []
}
EOF

# Function to start server and wait for it to be ready
start_server() {
    local server_type=$1
    log_info "Starting server: $server_type"
    
    "$SERVER_BIN" -server "$server_type" -port "$PORT" &
    SERVER_PID=$!
    
    # Wait for server to be ready
    local max_attempts=30
    local attempt=0
    while ! curl -s "http://localhost:${PORT}/" > /dev/null 2>&1; do
        sleep 0.1
        attempt=$((attempt + 1))
        if [ $attempt -ge $max_attempts ]; then
            log_error "Server failed to start: $server_type"
            kill $SERVER_PID 2>/dev/null || true
            return 1
        fi
    done
    
    log_success "Server ready: $server_type (PID: $SERVER_PID)"
    return 0
}

# Function to stop server
stop_server() {
    if [ -n "${SERVER_PID:-}" ]; then
        kill $SERVER_PID 2>/dev/null || true
        wait $SERVER_PID 2>/dev/null || true
        unset SERVER_PID
    fi
}

# Function to run wrk benchmark
run_wrk_benchmark() {
    local server_type=$1
    local bench_name=$2
    local method=$3
    local path=$4
    
    log_info "Running benchmark: $bench_name on $server_type"
    
    local wrk_script=""
    local wrk_args="-t$THREADS -c$CONNECTIONS -d$BENCHMARK_DURATION"
    
    if [ "$method" = "POST" ]; then
        # Use a Lua script for POST requests
        wrk_args="$wrk_args -s ${SCRIPT_DIR}/post.lua"
    fi
    
    # Warmup
    log_info "Warming up..."
    wrk -t2 -c16 -d"$WARMUP_DURATION" "http://localhost:${PORT}${path}" > /dev/null 2>&1 || true
    
    # Run benchmark
    local output
    output=$(wrk $wrk_args --latency "http://localhost:${PORT}${path}" 2>&1)
    
    # Parse wrk output
    local reqs_sec=$(echo "$output" | grep "Requests/sec:" | awk '{print $2}')
    local transfer_sec=$(echo "$output" | grep "Transfer/sec:" | awk '{print $2}')
    local latency_avg=$(echo "$output" | grep "Latency" | head -1 | awk '{print $2}')
    local latency_stdev=$(echo "$output" | grep "Latency" | head -1 | awk '{print $3}')
    local latency_max=$(echo "$output" | grep "Latency" | head -1 | awk '{print $4}')
    
    # Parse latency distribution
    local p50=$(echo "$output" | grep "50%" | awk '{print $2}')
    local p75=$(echo "$output" | grep "75%" | awk '{print $2}')
    local p90=$(echo "$output" | grep "90%" | awk '{print $2}')
    local p99=$(echo "$output" | grep "99%" | awk '{print $2}')
    
    log_success "Completed: $reqs_sec req/s, avg latency: $latency_avg"
    
    # Convert to JSON-safe values (remove units if present)
    reqs_sec=$(echo "$reqs_sec" | sed 's/[^0-9.]//g')
    
    # Append to results
    local result=$(cat << EOF
{
    "server": "$server_type",
    "benchmark": "$bench_name",
    "method": "$method",
    "path": "$path",
    "requests_per_sec": ${reqs_sec:-0},
    "transfer_per_sec": "$transfer_sec",
    "latency": {
        "avg": "$latency_avg",
        "stdev": "$latency_stdev",
        "max": "$latency_max",
        "p50": "$p50",
        "p75": "$p75",
        "p90": "$p90",
        "p99": "$p99"
    }
}
EOF
    )
    
    # Add to results file using jq
    jq ".results += [$result]" "$RESULTS_FILE" > "${RESULTS_FILE}.tmp" && mv "${RESULTS_FILE}.tmp" "$RESULTS_FILE"
}

# Function to run wrk2 latency benchmark
run_wrk2_latency() {
    local server_type=$1
    local bench_name=$2
    local path=$3
    
    log_info "Running latency benchmark: $bench_name on $server_type (${LATENCY_RATE} req/s)"
    
    # Run wrk2 with fixed rate
    local output
    output=$(wrk2 -t$THREADS -c$CONNECTIONS -d$BENCHMARK_DURATION -R$LATENCY_RATE --latency "http://localhost:${PORT}${path}" 2>&1)
    
    # Parse detailed latency
    local p50=$(echo "$output" | grep "50.000%" | awk '{print $2}')
    local p90=$(echo "$output" | grep "90.000%" | awk '{print $2}')
    local p99=$(echo "$output" | grep "99.000%" | awk '{print $2}')
    local p999=$(echo "$output" | grep "99.900%" | awk '{print $2}')
    local p9999=$(echo "$output" | grep "99.990%" | awk '{print $2}')
    
    log_success "Latency p50=$p50 p99=$p99 p99.9=$p999"
    
    local result=$(cat << EOF
{
    "server": "$server_type",
    "benchmark": "${bench_name}_latency",
    "target_rate": $LATENCY_RATE,
    "latency": {
        "p50": "$p50",
        "p90": "$p90",
        "p99": "$p99",
        "p99.9": "$p999",
        "p99.99": "$p9999"
    }
}
EOF
    )
    
    jq ".results += [$result]" "$RESULTS_FILE" > "${RESULTS_FILE}.tmp" && mv "${RESULTS_FILE}.tmp" "$RESULTS_FILE"
}

# Main benchmark loop
run_benchmarks() {
    local servers=("$@")
    
    for server_type in "${servers[@]}"; do
        log_info "=== Benchmarking: $server_type ==="
        
        # Try to start server
        if ! start_server "$server_type"; then
            log_warn "Skipping $server_type (failed to start)"
            continue
        fi
        
        # Run all benchmark types
        for bench_spec in "${BENCHMARK_TYPES[@]}"; do
            IFS=':' read -r bench_name method path <<< "$bench_spec"
            run_wrk_benchmark "$server_type" "$bench_name" "$method" "$path"
        done
        
        # Run latency benchmark (simple only)
        run_wrk2_latency "$server_type" "simple" "/"
        
        # Stop server
        stop_server
        
        # Brief pause between servers
        sleep 1
    done
}

# Cleanup on exit
trap 'stop_server; exit' INT TERM EXIT

# Check dependencies
check_dependencies() {
    local missing=0
    
    for cmd in wrk wrk2 jq curl; do
        if ! command -v "$cmd" &> /dev/null; then
            log_error "Missing dependency: $cmd"
            missing=1
        fi
    done
    
    if [ ! -x "$SERVER_BIN" ]; then
        log_error "Server binary not found: $SERVER_BIN"
        log_info "Building server..."
        cd "$PROJECT_ROOT"
        go build -o bin/server ./cmd/server
    fi
    
    return $missing
}

# Parse arguments
MODE="${1:-all}"

log_info "Celeris Benchmark Runner"
log_info "Architecture: $ARCH_NAME"
log_info "Duration: $BENCHMARK_DURATION"
log_info "Connections: $CONNECTIONS"
log_info "Threads: $THREADS"

check_dependencies

case "$MODE" in
    baseline)
        log_info "Running baseline benchmarks only"
        run_benchmarks "${BASELINE_SERVERS[@]}"
        ;;
    theoretical)
        log_info "Running theoretical benchmarks only"
        run_benchmarks "${THEORETICAL_SERVERS[@]}"
        ;;
    all)
        log_info "Running all benchmarks"
        run_benchmarks "${BASELINE_SERVERS[@]}"
        run_benchmarks "${THEORETICAL_SERVERS[@]}"
        ;;
    *)
        log_error "Unknown mode: $MODE"
        echo "Usage: $0 [baseline|theoretical|all]"
        exit 1
        ;;
esac

log_success "Benchmarks complete!"
log_info "Results saved to: $RESULTS_FILE"
