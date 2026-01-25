// Package main provides the benchmark runner CLI.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/goceleris/benchmarks/internal/bench"
)

// Server definitions
var baselineServers = []string{
	"stdhttp-h1",
	"stdhttp-h2",
	"stdhttp-hybrid",
	"fiber-h1",
	"iris-h2",
	"gin-h1",
	"chi-h1",
	"echo-h1",
}

var theoreticalServers = []string{
	"epoll-h1",
	"epoll-h2",
	"epoll-hybrid",
	"iouring-h1",
	"iouring-h2",
	"iouring-hybrid",
}

// Benchmark types
var benchmarkTypes = []struct {
	Name   string
	Method string
	Path   string
	Body   []byte
}{
	{"simple", "GET", "/", nil},
	{"json", "GET", "/json", nil},
	{"path", "GET", "/users/12345", nil},
	{"big-request", "POST", "/upload", make([]byte, 4096)},
}

func main() {
	mode := flag.String("mode", "baseline", "Benchmark mode: baseline, theoretical, all")
	duration := flag.Duration("duration", 30*time.Second, "Benchmark duration")
	connections := flag.Int("connections", 256, "Number of connections")
	workers := flag.Int("workers", 8, "Number of worker goroutines")
	warmup := flag.Duration("warmup", 5*time.Second, "Warmup duration")
	outputDir := flag.String("output", "results", "Output directory for results")
	port := flag.String("port", "8080", "Server port")
	serverBin := flag.String("server-bin", "", "Path to server binary (auto-detect if empty)")

	flag.Parse()

	// Determine architecture
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x86"
	}

	log.Printf("Celeris Benchmark Runner")
	log.Printf("Architecture: %s", arch)
	log.Printf("Duration: %s", *duration)
	log.Printf("Connections: %d", *connections)
	log.Printf("Workers: %d", *workers)

	// Find server binary
	if *serverBin == "" {
		// Try to find it
		candidates := []string{
			"./bin/server",
			"./server",
			"bin/server",
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				*serverBin = c
				break
			}
		}
		if *serverBin == "" {
			log.Fatal("Could not find server binary. Build with 'go build -o bin/server ./cmd/server'")
		}
	}

	// Determine which servers to benchmark
	var servers []string
	switch *mode {
	case "baseline":
		servers = baselineServers
	case "theoretical":
		servers = theoreticalServers
	case "all":
		servers = append(baselineServers, theoreticalServers...)
	default:
		log.Fatalf("Unknown mode: %s", *mode)
	}

	// Create output
	output := &bench.BenchmarkOutput{
		Timestamp:    time.Now().UTC().Format("2006-01-02T15_04_05Z"),
		Architecture: arch,
		Config: bench.BenchmarkConfig{
			Duration:    duration.String(),
			Connections: *connections,
			Workers:     *workers,
		},
		Results: []bench.ServerResult{},
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Run benchmarks
	for _, serverType := range servers {
		log.Printf("=== Benchmarking: %s ===", serverType)

		// Start server
		cmd, err := startServer(*serverBin, serverType, *port)
		if err != nil {
			log.Printf("WARN: Failed to start %s: %v, skipping", serverType, err)
			continue
		}

		// Wait for server to be ready
		if err := waitForServer(ctx, *port, 10*time.Second); err != nil {
			log.Printf("WARN: Server %s not ready: %v, skipping", serverType, err)
			stopServer(cmd)
			continue
		}

		log.Printf("Server ready: %s", serverType)

		// Run each benchmark type
		for _, bt := range benchmarkTypes {
			log.Printf("Running benchmark: %s on %s", bt.Name, serverType)

			cfg := bench.Config{
				URL:         fmt.Sprintf("http://localhost:%s%s", *port, bt.Path),
				Method:      bt.Method,
				Body:        bt.Body,
				Duration:    *duration,
				Connections: *connections,
				Workers:     *workers,
				WarmupTime:  *warmup,
				KeepAlive:   true,
				H2C:         strings.Contains(serverType, "-h2") && (strings.Contains(serverType, "epoll") || strings.Contains(serverType, "iouring")),
			}

			benchmarker := bench.New(cfg)
			result, err := benchmarker.Run(ctx)
			if err != nil {
				log.Printf("ERROR: Benchmark failed: %v", err)
				continue
			}

			log.Printf("Completed: %.2f req/s, avg latency: %s", result.RequestsPerSec, result.Latency.Avg)

			output.Results = append(output.Results, result.ToServerResult(
				serverType, bt.Name, bt.Method, bt.Path,
			))
		}

		// Stop server
		stopServer(cmd)
		time.Sleep(500 * time.Millisecond) // Brief pause between servers
	}

	// Write output
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		log.Fatalf("Failed to create output directory: %v", err)
	}

	outputFile := filepath.Join(*outputDir, fmt.Sprintf("benchmark-%s-%s.json", arch, output.Timestamp))
	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		log.Fatalf("Failed to marshal results: %v", err)
	}

	if err := os.WriteFile(outputFile, data, 0644); err != nil {
		log.Fatalf("Failed to write results: %v", err)
	}

	log.Printf("Results saved to: %s", outputFile)
	log.Printf("Benchmarks complete!")
}

func startServer(binary, serverType, port string) (*exec.Cmd, error) {
	cmd := exec.Command(binary, "-server", serverType, "-port", port)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return cmd, nil
}

func stopServer(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}

	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
	}
}

func waitForServer(ctx context.Context, port string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Try to connect
		resp, err := httpGet(fmt.Sprintf("http://localhost:%s/", port))
		if err == nil && resp {
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for server on port %s", port)
}

func httpGet(url string) (bool, error) {
	// Simple HTTP GET without importing net/http to avoid import cycle
	// This is a workaround - in practice we'd use http.Get
	cmd := exec.Command("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", url)
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "200", nil
}
