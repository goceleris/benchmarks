#!/bin/bash
# Local benchmark testing using multipass VM
# This script creates an Ubuntu VM for testing the benchmark suite locally

set -e

VM_NAME="celeris-bench"
VM_CPUS=2
VM_MEM="2G"
VM_DISK="10G"

echo "=== Celeris Local Benchmark Testing ==="
echo ""

# Check if multipass is available
if ! command -v multipass &> /dev/null; then
    echo "[ERROR] multipass is not installed. Install with: brew install multipass"
    exit 1
fi

# Function to cleanup
cleanup() {
    echo ""
    echo "=== Cleanup ==="
    echo "Stopping VM..."
    multipass stop $VM_NAME 2>/dev/null || true
    echo "Deleting VM..."
    multipass delete $VM_NAME 2>/dev/null || true
    multipass purge 2>/dev/null || true
    echo "[OK] Cleanup complete"
}

# Parse arguments
ACTION="${1:-run}"

case "$ACTION" in
    cleanup)
        cleanup
        exit 0
        ;;
    shell)
        echo "Opening shell in VM..."
        multipass shell $VM_NAME
        exit 0
        ;;
    run)
        # Continue with normal execution
        ;;
    *)
        echo "Usage: $0 [run|cleanup|shell]"
        echo "  run     - Create VM and run benchmarks (default)"
        echo "  cleanup - Delete the VM"
        echo "  shell   - Open a shell in the VM"
        exit 1
        ;;
esac

# Check if VM already exists
if multipass list | grep -q "$VM_NAME"; then
    echo "[INFO] VM '$VM_NAME' already exists, reusing..."
    multipass start $VM_NAME 2>/dev/null || true
else
    echo "[INFO] Creating Ubuntu VM: $VM_NAME (CPUs: $VM_CPUS, Memory: $VM_MEM)..."
    multipass launch -n $VM_NAME -c $VM_CPUS -m $VM_MEM -d $VM_DISK 22.04
fi

echo "[INFO] Waiting for VM to be ready..."
sleep 5

# Install Go and build tools
echo ""
echo "=== Installing Go and build tools ==="
multipass exec $VM_NAME -- bash -c '
    set -e
    export DEBIAN_FRONTEND=noninteractive
    
    # Check if Go is already installed
    if command -v go &> /dev/null; then
        echo "[INFO] Go already installed: $(go version)"
    else
        echo "[INFO] Installing Go..."
        sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq
        sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq golang-go git make
        echo "[OK] Go installed: $(go version)"
    fi
'

# Get the current directory (benchmarks repo)
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"

echo ""
echo "=== Copying benchmark code to VM ==="
echo "[INFO] Source: $REPO_DIR"

# Create a tarball of the repo (excluding .git)
TARBALL="/tmp/celeris-bench.tar.gz"
tar -czf "$TARBALL" -C "$REPO_DIR" --exclude='.git' --exclude='bin' --exclude='results' .

# Copy to VM
multipass transfer "$TARBALL" $VM_NAME:/tmp/celeris-bench.tar.gz

# Extract in VM
multipass exec $VM_NAME -- bash -c '
    rm -rf ~/benchmarks
    mkdir -p ~/benchmarks
    tar -xzf /tmp/celeris-bench.tar.gz -C ~/benchmarks
    rm /tmp/celeris-bench.tar.gz
'
rm "$TARBALL"
echo "[OK] Code copied to VM"

# Build the benchmark tools
echo ""
echo "=== Building server and benchmark tool ==="
multipass exec $VM_NAME -- bash -c '
    set -e
    cd ~/benchmarks
    
    echo "[INFO] Downloading Go modules..."
    go mod download
    
    echo "[INFO] Building server..."
    go build -o bin/server ./cmd/server
    
    echo "[INFO] Building benchmark tool..."
    go build -o bin/bench ./cmd/bench
    
    echo "[OK] Build complete"
    ls -la bin/
'

# Run benchmarks
echo ""
echo "=== Running Benchmarks ==="

# Check which mode to run
BENCHMARK_MODE="${BENCHMARK_MODE:-all}"
BENCHMARK_DURATION="${BENCHMARK_DURATION:-5s}"

echo "[INFO] Mode: $BENCHMARK_MODE"
echo "[INFO] Duration: $BENCHMARK_DURATION"
echo ""

multipass exec $VM_NAME -- bash -c "
    set -e
    cd ~/benchmarks
    
    echo '[INFO] Starting benchmark run...'
    echo ''
    
    ./bin/bench -mode '$BENCHMARK_MODE' -duration '$BENCHMARK_DURATION' -connections 64 -workers 4
    
    echo ''
    echo '[OK] Benchmark complete!'
    
    # Show results
    if ls results/*.json 1>/dev/null 2>&1; then
        echo ''
        echo '=== Results ==='
        cat results/*.json
    fi
"

echo ""
echo "=== Local Benchmark Complete ==="
echo "To clean up the VM, run: $0 cleanup"
echo "To open a shell in the VM, run: $0 shell"
