// Package main provides the benchmark runner CLI.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
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

var baselineServers = []string{
	"stdhttp-h1",
	"stdhttp-h2",
	"stdhttp-hybrid",
	"fiber-h1",
	"iris-h1",
	"iris-h2",
	"iris-hybrid",
	"gin-h1",
	"gin-h2",
	"gin-hybrid",
	"chi-h1",
	"chi-h2",
	"chi-hybrid",
	"echo-h1",
	"echo-h2",
	"echo-hybrid",
}

var theoreticalServers = []string{
	"epoll-h1",
	"epoll-h2",
	"epoll-hybrid",
	"iouring-h1",
	"iouring-h2",
	"iouring-hybrid",
}

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
	connections := flag.Int("connections", 0, "Number of connections (0 = auto-scale based on workers)")
	workers := flag.Int("workers", 0, "Number of worker goroutines (0 = auto-scale based on CPU)")
	workersPerCPU := flag.Int("workers-per-cpu", 4, "Workers per CPU core when auto-scaling (for I/O-bound workloads)")
	connectionsPerWorker := flag.Int("connections-per-worker", 2, "Connections per worker when auto-scaling")
	warmup := flag.Duration("warmup", 5*time.Second, "Warmup duration")
	outputDir := flag.String("output", "results", "Output directory for results")
	port := flag.String("port", "8080", "Server port")
	serverBin := flag.String("server-bin", "", "Path to server binary (auto-detect if empty)")
	checkpointFile := flag.String("checkpoint", "", "Checkpoint file for incremental execution (auto-generated if empty)")
	resume := flag.Bool("resume", false, "Resume from existing checkpoint")
	mergeFile := flag.String("merge", "", "Merge results from another checkpoint file into output")

	flag.Parse()

	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x86"
	}

	// Get CPU count for scaling
	numCPU := runtime.NumCPU()

	// Auto-scale workers based on CPU count if not explicitly set
	actualWorkers := *workers
	if actualWorkers == 0 {
		// For I/O-bound HTTP benchmarks, use multiple workers per CPU
		// This ensures we can saturate the server even with network latency
		actualWorkers = numCPU * *workersPerCPU
		// Ensure minimum of 8 workers for small machines
		if actualWorkers < 8 {
			actualWorkers = 8
		}
		// Cap at reasonable maximum to avoid overwhelming the system
		if actualWorkers > 1024 {
			actualWorkers = 1024
		}
	}

	// Auto-scale connections based on workers if not explicitly set
	actualConnections := *connections
	if actualConnections == 0 {
		// Multiple connections per worker for better connection pool utilization
		actualConnections = actualWorkers * *connectionsPerWorker
		// Ensure minimum of 64 connections
		if actualConnections < 64 {
			actualConnections = 64
		}
		// Cap at reasonable maximum
		if actualConnections > 4096 {
			actualConnections = 4096
		}
	}

	log.Printf("Celeris Benchmark Runner")
	log.Printf("Architecture: %s", arch)
	log.Printf("Available CPUs: %d", numCPU)
	log.Printf("Duration: %s", *duration)
	if *workers == 0 {
		log.Printf("Workers: %d (auto-scaled: %d CPUs x %d workers/CPU)", actualWorkers, numCPU, *workersPerCPU)
	} else {
		log.Printf("Workers: %d (manually set)", actualWorkers)
	}
	if *connections == 0 {
		log.Printf("Connections: %d (auto-scaled: %d workers x %d conn/worker)", actualConnections, actualWorkers, *connectionsPerWorker)
	} else {
		log.Printf("Connections: %d (manually set)", actualConnections)
	}

	if *serverBin == "" {
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

	// Setup checkpoint
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		log.Fatalf("Failed to create output directory: %v", err)
	}

	// Auto-generate checkpoint filename if not specified
	if *checkpointFile == "" {
		*checkpointFile = filepath.Join(*outputDir, fmt.Sprintf("checkpoint-%s.json", arch))
	}

	var checkpoint *bench.Checkpoint
	if *resume {
		// Try to load existing checkpoint
		if cp, err := bench.LoadCheckpoint(*checkpointFile); err == nil {
			checkpoint = cp
			log.Printf("Resuming from checkpoint: %s (%d results completed)", *checkpointFile, len(cp.Results))
		} else {
			log.Printf("No existing checkpoint found, starting fresh")
		}
	}

	// Create new checkpoint if not resuming or no checkpoint found
	if checkpoint == nil {
		output := &bench.BenchmarkOutput{
			Timestamp:    time.Now().UTC().Format("2006-01-02T15_04_05Z"),
			Architecture: arch,
			Config: bench.BenchmarkConfig{
				Duration:    duration.String(),
				Connections: actualConnections,
				Workers:     actualWorkers,
				CPUs:        numCPU,
			},
			Results: []bench.ServerResult{},
		}
		checkpoint = bench.NewCheckpoint(output)
	}

	// Setup signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Save checkpoint on shutdown
	go func() {
		sig := <-sigChan
		log.Printf("Received signal %v, saving checkpoint and shutting down...", sig)
		if err := checkpoint.Save(*checkpointFile); err != nil {
			log.Printf("ERROR: Failed to save checkpoint: %v", err)
		} else {
			log.Printf("Checkpoint saved to: %s", *checkpointFile)
		}
		cancel()
	}()

	// Count how many benchmarks are already completed
	totalBenchmarks := len(servers) * len(benchmarkTypes)
	completedBefore := len(checkpoint.Results)
	skipped := 0

	for _, serverType := range servers {
		// Check if context is cancelled
		select {
		case <-ctx.Done():
			log.Printf("Benchmark interrupted, progress saved")
			goto saveAndExit
		default:
		}

		// Check if all benchmarks for this server are already completed
		allCompleted := true
		for _, bt := range benchmarkTypes {
			if !checkpoint.IsCompleted(serverType, bt.Name) {
				allCompleted = false
				break
			}
		}
		if allCompleted {
			log.Printf("=== Skipping (completed): %s ===", serverType)
			skipped += len(benchmarkTypes)
			continue
		}

		log.Printf("=== Benchmarking: %s ===", serverType)

		cmd, err := startServer(*serverBin, serverType, *port)
		if err != nil {
			log.Printf("WARN: Failed to start %s: %v, skipping", serverType, err)
			continue
		}

		if err := waitForServer(ctx, *port, 10*time.Second); err != nil {
			log.Printf("WARN: Server %s not ready: %v, skipping", serverType, err)
			stopServer(cmd)
			continue
		}

		log.Printf("Server ready: %s", serverType)

		for _, bt := range benchmarkTypes {
			// Check if context is cancelled
			select {
			case <-ctx.Done():
				log.Printf("Benchmark interrupted during %s:%s", serverType, bt.Name)
				stopServer(cmd)
				goto saveAndExit
			default:
			}

			// Skip if already completed
			if checkpoint.IsCompleted(serverType, bt.Name) {
				log.Printf("Skipping (completed): %s on %s", bt.Name, serverType)
				skipped++
				continue
			}

			log.Printf("Running benchmark: %s on %s", bt.Name, serverType)

			cfg := bench.Config{
				URL:         fmt.Sprintf("http://localhost:%s%s", *port, bt.Path),
				Method:      bt.Method,
				Body:        bt.Body,
				Duration:    *duration,
				Connections: actualConnections,
				Workers:     actualWorkers,
				WarmupTime:  *warmup,
				KeepAlive:   true,
				H2C:         strings.Contains(serverType, "-h2") || strings.Contains(serverType, "-hybrid"),
			}

			benchmarker := bench.New(cfg)
			result, err := benchmarker.Run(ctx)
			if err != nil {
				if ctx.Err() != nil {
					// Context cancelled, save and exit
					log.Printf("Benchmark interrupted: %v", err)
					stopServer(cmd)
					goto saveAndExit
				}
				log.Printf("ERROR: Benchmark failed: %v", err)
				continue
			}

			log.Printf("Completed: %.2f req/s, avg latency: %s", result.RequestsPerSec, result.Latency.Avg)

			// Add result and save checkpoint immediately
			checkpoint.AddResult(result.ToServerResult(
				serverType, bt.Name, bt.Method, bt.Path,
			))

			// Save checkpoint after each benchmark
			if err := checkpoint.Save(*checkpointFile); err != nil {
				log.Printf("WARN: Failed to save checkpoint: %v", err)
			}
		}

		stopServer(cmd)
		time.Sleep(500 * time.Millisecond) // Brief pause between servers
	}

saveAndExit:
	// Merge results from another file if specified
	if *mergeFile != "" {
		if otherCP, err := bench.LoadCheckpoint(*mergeFile); err == nil {
			beforeMerge := len(checkpoint.Results)
			checkpoint.MergeResults(otherCP)
			afterMerge := len(checkpoint.Results)
			log.Printf("Merged %d results from %s", afterMerge-beforeMerge, *mergeFile)
		} else {
			log.Printf("WARN: Failed to load merge file %s: %v", *mergeFile, err)
		}
	}

	// Save final checkpoint
	if err := checkpoint.Save(*checkpointFile); err != nil {
		log.Printf("ERROR: Failed to save final checkpoint: %v", err)
	}

	// Write final output file (without checkpoint metadata)
	output := checkpoint.ToBenchmarkOutput()
	outputFile := filepath.Join(*outputDir, fmt.Sprintf("benchmark-%s-%s.json", arch, output.Timestamp))
	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		log.Fatalf("Failed to marshal results: %v", err)
	}

	if err := os.WriteFile(outputFile, data, 0644); err != nil {
		log.Fatalf("Failed to write results: %v", err)
	}

	completedNow := len(checkpoint.Results) - completedBefore
	log.Printf("Results saved to: %s", outputFile)
	log.Printf("Checkpoint saved to: %s", *checkpointFile)
	log.Printf("Summary: %d total benchmarks, %d completed this run, %d skipped (already done), %d total completed",
		totalBenchmarks, completedNow, skipped, len(checkpoint.Results))

	if len(checkpoint.Results) < totalBenchmarks {
		log.Printf("NOTE: %d benchmarks remaining. Run with -resume to continue.", totalBenchmarks-len(checkpoint.Results))
	} else {
		log.Printf("Benchmarks complete!")
	}
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

		conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%s", port), 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for server on port %s", port)
}
