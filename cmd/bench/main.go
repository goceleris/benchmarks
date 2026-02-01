// Package main provides the benchmark runner CLI.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

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

// ControlStatus represents the response from the control daemon
type ControlStatus struct {
	ServerType string `json:"server_type"`
	Status     string `json:"status"`
	Port       string `json:"port"`
	Error      string `json:"error,omitempty"`
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

	// Remote mode flags
	serverIP := flag.String("server-ip", "", "Remote server IP (enables remote mode, can be empty if using SSM)")
	controlPort := flag.String("control-port", "9999", "Control daemon port on remote server")
	serverRetryTimeout := flag.Duration("server-retry-timeout", 10*time.Minute, "Max time to wait for server to become available")
	serverRetryInterval := flag.Duration("server-retry-interval", 5*time.Second, "Interval between server availability checks")
	maxUnreachable := flag.Duration("max-unreachable", 3*time.Minute, "Max time control daemon can be unreachable before failing (0 = disabled)")
	useSSM := flag.Bool("use-ssm", false, "Use AWS SSM Parameter Store for dynamic server IP discovery")
	ssmParamName := flag.String("ssm-param", "", "SSM parameter name for server IP (default: /celeris-benchmark/server-ip/<arch>)")
	awsRegion := flag.String("aws-region", "us-east-1", "AWS region for SSM")

	// C2 integration flags
	c2Endpoint := flag.String("c2-endpoint", "", "C2 server endpoint for reporting results (enables C2 mode)")
	runID := flag.String("run-id", "", "Benchmark run ID (required in C2 mode)")
	archOverride := flag.String("arch", "", "Architecture override (default: auto-detect)")

	// Infrastructure mode (affects connection/worker scaling)
	infraMode := flag.String("infra-mode", "", "Infrastructure mode: fast, med, metal (affects scaling defaults)")

	flag.Parse()

	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x86"
	}

	// Override arch if specified
	if *archOverride != "" {
		arch = *archOverride
	}

	// Check for environment variable override
	if *serverIP == "" {
		*serverIP = os.Getenv("BENCHMARK_SERVER_IP")
	}

	// Enable SSM if specified or if no server IP is provided but we're in a remote context
	if os.Getenv("USE_SSM_DISCOVERY") == "true" {
		*useSSM = true
	}

	// Set default SSM parameter name based on architecture
	if *ssmParamName == "" {
		*ssmParamName = fmt.Sprintf("/celeris-benchmark/server-ip/%s", arch)
	}

	remoteMode := *serverIP != "" || *useSSM

	// Get CPU count for scaling
	numCPU := runtime.NumCPU()

	// Determine scaling multipliers based on infrastructure mode
	// Metal mode needs much higher values to saturate 64-96 core servers
	effectiveWorkersPerCPU := *workersPerCPU
	effectiveConnsPerWorker := *connectionsPerWorker
	maxWorkers := 1024
	maxConnections := 4096

	switch *infraMode {
	case "metal":
		// Metal: saturate 64-96 core servers from 36-72 core clients
		if *workersPerCPU == 4 { // default, not explicitly set
			effectiveWorkersPerCPU = 16
		}
		if *connectionsPerWorker == 2 { // default, not explicitly set
			effectiveConnsPerWorker = 8
		}
		maxWorkers = 2048
		maxConnections = 16384
		log.Printf("Infrastructure mode: METAL (high-throughput scaling)")
	case "med":
		// Med: moderate scaling for larger instances
		if *workersPerCPU == 4 {
			effectiveWorkersPerCPU = 8
		}
		if *connectionsPerWorker == 2 {
			effectiveConnsPerWorker = 4
		}
		maxWorkers = 1024
		maxConnections = 8192
		log.Printf("Infrastructure mode: MED")
	case "fast", "":
		// Fast/default: conservative scaling for small instances
		log.Printf("Infrastructure mode: FAST (default)")
	default:
		log.Printf("Infrastructure mode: %s (using defaults)", *infraMode)
	}

	// Auto-scale workers based on CPU count if not explicitly set
	actualWorkers := *workers
	if actualWorkers == 0 {
		actualWorkers = numCPU * effectiveWorkersPerCPU
		if actualWorkers < 8 {
			actualWorkers = 8
		}
		if actualWorkers > maxWorkers {
			actualWorkers = maxWorkers
		}
	}

	// Auto-scale connections based on workers if not explicitly set
	actualConnections := *connections
	if actualConnections == 0 {
		actualConnections = actualWorkers * effectiveConnsPerWorker
		if actualConnections < 64 {
			actualConnections = 64
		}
		if actualConnections > maxConnections {
			actualConnections = maxConnections
		}
	}

	log.Printf("Celeris Benchmark Runner")
	log.Printf("Architecture: %s", arch)
	log.Printf("Available CPUs: %d", numCPU)
	log.Printf("Duration: %s", *duration)
	if remoteMode {
		log.Printf("Mode: REMOTE (server: %s:%s, control: %s)", *serverIP, *port, *controlPort)
	} else {
		log.Printf("Mode: LOCAL")
	}
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

	// In local mode, we need the server binary
	if !remoteMode {
		if *serverBin == "" {
			candidates := []string{"./bin/server", "./server", "bin/server"}
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

	if *checkpointFile == "" {
		*checkpointFile = filepath.Join(*outputDir, fmt.Sprintf("checkpoint-%s.json", arch))
	}

	var checkpoint *bench.Checkpoint
	if *resume {
		if cp, err := bench.LoadCheckpoint(*checkpointFile); err == nil {
			checkpoint = cp
			log.Printf("Resuming from checkpoint: %s (%d results completed)", *checkpointFile, len(cp.Results))
		} else {
			log.Printf("No existing checkpoint found, starting fresh")
		}
	}

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

	// Channel to signal spot interruption
	spotInterruptChan := make(chan struct{})

	// Track if we're exiting due to spot interruption (need non-zero exit code)
	// Use atomic to avoid race between goroutine and main
	var exitDueToSpotInterrupt atomic.Bool

	go func() {
		select {
		case sig := <-sigChan:
			log.Printf("Received signal %v, saving checkpoint and shutting down...", sig)
		case <-spotInterruptChan:
			log.Printf("Spot interruption detected, saving checkpoint and shutting down...")
			exitDueToSpotInterrupt.Store(true)
		}
		if err := checkpoint.Save(*checkpointFile); err != nil {
			log.Printf("ERROR: Failed to save checkpoint: %v", err)
		} else {
			log.Printf("Checkpoint saved to: %s", *checkpointFile)
		}
		cancel()
	}()

	// Start spot interruption monitor (checks AWS metadata for termination notice)
	go monitorSpotInterruption(ctx, spotInterruptChan)

	// Defer exit code handling - must be after checkpoint save
	defer func() {
		if exitDueToSpotInterrupt.Load() {
			log.Printf("Exiting with code 1 due to spot interruption (triggers retry job)")
			os.Exit(1)
		}
	}()

	// Create remote controller if in remote mode
	var rc *RemoteController
	if remoteMode {
		rc = &RemoteController{
			serverIP:       *serverIP,
			serverPort:     *port,
			controlPort:    *controlPort,
			retryTimeout:   *serverRetryTimeout,
			retryInterval:  *serverRetryInterval,
			maxUnreachable: *maxUnreachable,
			useSSM:         *useSSM,
			ssmParamName:   *ssmParamName,
			awsRegion:      *awsRegion,
		}

		// If using SSM and no initial IP, fetch it now
		if *useSSM && *serverIP == "" {
			log.Printf("Fetching server IP from SSM parameter: %s", *ssmParamName)
			if err := rc.refreshServerIP(ctx); err != nil {
				log.Printf("Warning: Could not fetch server IP from SSM: %v", err)
				log.Printf("Will retry during benchmark execution...")
			} else {
				log.Printf("Server IP from SSM: %s", rc.serverIP)
			}
		}
	}

	// Create C2 client if endpoint is provided
	var c2 *C2Client
	if *c2Endpoint != "" {
		if *runID == "" {
			log.Fatal("C2 mode requires -run-id flag")
		}
		c2 = &C2Client{
			endpoint: *c2Endpoint,
			runID:    *runID,
			arch:     arch,
		}
		log.Printf("C2 mode enabled: endpoint=%s, run_id=%s, arch=%s", *c2Endpoint, *runID, arch)

		// Start heartbeat goroutine
		go c2.runHeartbeat(ctx)
	}

	totalBenchmarks := len(servers) * len(benchmarkTypes)
	completedBefore := len(checkpoint.Results)
	skipped := 0

	// Initialize C2 client total count
	if c2 != nil {
		c2.progressMu.Lock()
		c2.totalCount = totalBenchmarks
		c2.progressMu.Unlock()
	}

	for _, serverType := range servers {
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

		var cmd *exec.Cmd
		var serverHost string

		if remoteMode {
			// Remote mode: use control daemon to start server
			if err := rc.StartServer(ctx, serverType); err != nil {
				log.Printf("WARN: Failed to start %s on remote: %v, skipping", serverType, err)
				continue
			}
			rc.mu.RLock()
			serverHost = rc.serverIP
			rc.mu.RUnlock()
		} else {
			// Local mode: start server locally
			var err error
			cmd, err = startServer(*serverBin, serverType, *port)
			if err != nil {
				log.Printf("WARN: Failed to start %s: %v, skipping", serverType, err)
				continue
			}
			serverHost = "localhost"
		}

		// Wait for server to be ready
		if err := waitForServer(ctx, serverHost, *port, 10*time.Second); err != nil {
			log.Printf("WARN: Server %s not ready: %v, skipping", serverType, err)
			if remoteMode {
				_ = rc.StopServer(ctx)
			} else {
				stopServer(cmd)
			}
			continue
		}

		log.Printf("Server ready: %s", serverType)

		for _, bt := range benchmarkTypes {
			select {
			case <-ctx.Done():
				log.Printf("Benchmark interrupted during %s:%s", serverType, bt.Name)
				if remoteMode {
					_ = rc.StopServer(ctx)
				} else {
					stopServer(cmd)
				}
				goto saveAndExit
			default:
			}

			if checkpoint.IsCompleted(serverType, bt.Name) {
				log.Printf("Skipping (completed): %s on %s", bt.Name, serverType)
				skipped++
				continue
			}

			log.Printf("Running benchmark: %s on %s", bt.Name, serverType)

			// Update C2 client progress
			if c2 != nil {
				currentBenchNum := len(checkpoint.Results) + 1
				c2.progressMu.Lock()
				c2.currentServer = serverType
				c2.currentBench = bt.Name
				c2.currentCount = currentBenchNum
				c2.progressMu.Unlock()
			}

			cfg := bench.Config{
				URL:         fmt.Sprintf("http://%s:%s%s", serverHost, *port, bt.Path),
				Method:      bt.Method,
				Body:        bt.Body,
				Duration:    *duration,
				Connections: actualConnections,
				Workers:     actualWorkers,
				WarmupTime:  *warmup,
				KeepAlive:   true,
				H2C:         strings.Contains(serverType, "-h2") || strings.Contains(serverType, "-hybrid"),
			}

			// In remote mode, wrap the benchmark with retry logic
			var result *bench.Result
			var err error

			if remoteMode {
				result, err = runBenchmarkWithRetry(ctx, cfg, rc, serverType, *serverRetryTimeout, *serverRetryInterval)
			} else {
				benchmarker := bench.New(cfg)
				result, err = benchmarker.Run(ctx)
			}

			if err != nil {
				if ctx.Err() != nil {
					log.Printf("Benchmark interrupted: %v", err)
					if remoteMode {
						_ = rc.StopServer(ctx)
					} else {
						stopServer(cmd)
					}
					goto saveAndExit
				}
				log.Printf("ERROR: Benchmark failed: %v", err)
				continue
			}

			log.Printf("Completed: %.2f req/s, avg latency: %s", result.RequestsPerSec, result.Latency.Avg)

			serverResult := result.ToServerResult(serverType, bt.Name, bt.Method, bt.Path)
			checkpoint.AddResult(serverResult)

			// Send result to C2 if enabled
			if c2 != nil {
				if err := c2.sendResult(ctx, serverType, bt.Name, result); err != nil {
					log.Printf("WARN: Failed to send result to C2: %v", err)
				}
			}

			if err := checkpoint.Save(*checkpointFile); err != nil {
				log.Printf("WARN: Failed to save checkpoint: %v", err)
			}
		}

		if remoteMode {
			_ = rc.StopServer(ctx)
		} else {
			stopServer(cmd)
		}
		time.Sleep(500 * time.Millisecond)
	}

saveAndExit:
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

	if err := checkpoint.Save(*checkpointFile); err != nil {
		log.Printf("ERROR: Failed to save final checkpoint: %v", err)
	}

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

		// Notify C2 of completion
		if c2 != nil {
			if err := c2.sendCompletion(context.Background()); err != nil {
				log.Printf("WARN: Failed to send completion to C2: %v", err)
			} else {
				log.Printf("Completion notification sent to C2")
			}
		}
	}

	// Shutdown the control daemon if in remote mode and benchmarks completed successfully
	if remoteMode && rc != nil && len(checkpoint.Results) >= totalBenchmarks {
		log.Printf("Shutting down control daemon...")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = rc.ShutdownDaemon(shutdownCtx)
		shutdownCancel()
	}
}

// RemoteController manages communication with the control daemon
type RemoteController struct {
	mu               sync.RWMutex // Protects serverIP access
	serverIP         string
	serverPort       string
	controlPort      string
	retryTimeout     time.Duration
	retryInterval    time.Duration
	maxUnreachable   time.Duration // Max time control daemon can be unreachable before hard fail
	useSSM           bool          // Whether to use SSM for dynamic IP discovery
	ssmParamName     string        // SSM parameter name for server IP
	awsRegion        string
	unreachableSince time.Time // When control daemon first became unreachable (zero if reachable)
}

// getServerIPFromSSM fetches the current server IP from AWS SSM Parameter Store
func (rc *RemoteController) getServerIPFromSSM(ctx context.Context) (string, error) {
	if !rc.useSSM {
		rc.mu.RLock()
		ip := rc.serverIP
		rc.mu.RUnlock()
		return ip, nil
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(rc.awsRegion))
	if err != nil {
		return "", fmt.Errorf("failed to load AWS config: %w", err)
	}

	client := ssm.NewFromConfig(cfg)
	output, err := client.GetParameter(ctx, &ssm.GetParameterInput{
		Name: &rc.ssmParamName,
	})
	if err != nil {
		return "", fmt.Errorf("failed to get SSM parameter %s: %w", rc.ssmParamName, err)
	}

	if output.Parameter == nil || output.Parameter.Value == nil {
		return "", fmt.Errorf("SSM parameter %s has no value", rc.ssmParamName)
	}

	newIP := *output.Parameter.Value

	rc.mu.Lock()
	if newIP != rc.serverIP {
		log.Printf("Server IP changed: %s -> %s (discovered via SSM)", rc.serverIP, newIP)
		rc.serverIP = newIP
	}
	rc.mu.Unlock()

	return newIP, nil
}

// refreshServerIP updates the server IP from SSM if using dynamic discovery
func (rc *RemoteController) refreshServerIP(ctx context.Context) error {
	if !rc.useSSM {
		return nil
	}
	_, err := rc.getServerIPFromSSM(ctx)
	return err
}

// StartServer starts a server on the remote machine
func (rc *RemoteController) StartServer(ctx context.Context, serverType string) error {
	rc.mu.RLock()
	ip := rc.serverIP
	rc.mu.RUnlock()

	url := fmt.Sprintf("http://%s:%s/start?type=%s", ip, rc.controlPort, serverType)

	// Retry starting server with timeout
	deadline := time.Now().Add(rc.retryTimeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			return err
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			// Control daemon is unreachable - track how long
			if rc.unreachableSince.IsZero() {
				rc.unreachableSince = time.Now()
				log.Printf("Control daemon not reachable (started tracking unreachable time)")
			}

			unreachableDuration := time.Since(rc.unreachableSince)

			// Fail fast if unreachable for too long (server likely terminated)
			if rc.maxUnreachable > 0 && unreachableDuration > rc.maxUnreachable {
				return fmt.Errorf("control daemon unreachable for %s (max %s) - server likely terminated, failing fast",
					unreachableDuration.Round(time.Second), rc.maxUnreachable)
			}

			log.Printf("Control daemon not reachable (%s), retrying in %s...",
				unreachableDuration.Round(time.Second), rc.retryInterval)
			time.Sleep(rc.retryInterval)
			continue
		}

		// Control daemon is reachable - reset unreachable tracking
		rc.unreachableSince = time.Time{}

		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var status ControlStatus
			if err := json.Unmarshal(body, &status); err == nil {
				if status.Status == "running" {
					return nil
				}
			}
		}

		log.Printf("Server start response: %d - %s, retrying...", resp.StatusCode, string(body))
		time.Sleep(rc.retryInterval)
	}

	return fmt.Errorf("timeout waiting for server to start")
}

// StopServer stops the current server on the remote machine
func (rc *RemoteController) StopServer(ctx context.Context) error {
	rc.mu.RLock()
	ip := rc.serverIP
	rc.mu.RUnlock()

	url := fmt.Sprintf("http://%s:%s/stop", ip, rc.controlPort)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()

	return nil
}

// ShutdownDaemon tells the control daemon to shut down completely
func (rc *RemoteController) ShutdownDaemon(ctx context.Context) error {
	rc.mu.RLock()
	ip := rc.serverIP
	rc.mu.RUnlock()

	if ip == "" {
		return nil // No server to shutdown
	}

	url := fmt.Sprintf("http://%s:%s/shutdown", ip, rc.controlPort)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Server might already be shutting down, that's ok
		log.Printf("Note: Control daemon may already be shutting down: %v", err)
		return nil
	}
	_ = resp.Body.Close()

	log.Printf("Shutdown signal sent to control daemon")
	return nil
}

// WaitForServer waits for the server to become available
func (rc *RemoteController) WaitForServer(ctx context.Context) error {
	deadline := time.Now().Add(rc.retryTimeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Refresh IP if using SSM
		if rc.useSSM {
			_ = rc.refreshServerIP(ctx)
		}

		rc.mu.RLock()
		ip := rc.serverIP
		rc.mu.RUnlock()

		if ip == "" {
			log.Printf("No server IP available, waiting %s...", rc.retryInterval)
			time.Sleep(rc.retryInterval)
			continue
		}

		// First check if control daemon is reachable (quick health check)
		controlURL := fmt.Sprintf("http://%s:%s/health", ip, rc.controlPort)
		healthReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, controlURL, nil)
		healthResp, healthErr := http.DefaultClient.Do(healthReq)
		if healthErr != nil {
			// Control daemon unreachable - track how long
			if rc.unreachableSince.IsZero() {
				rc.unreachableSince = time.Now()
			}
			unreachableDuration := time.Since(rc.unreachableSince)

			// Fail fast if unreachable for too long
			if rc.maxUnreachable > 0 && unreachableDuration > rc.maxUnreachable {
				return fmt.Errorf("control daemon unreachable for %s - server likely terminated",
					unreachableDuration.Round(time.Second))
			}

			log.Printf("Control daemon unreachable (%s), waiting %s...",
				unreachableDuration.Round(time.Second), rc.retryInterval)
			time.Sleep(rc.retryInterval)
			continue
		}
		_ = healthResp.Body.Close()

		// Control daemon is reachable - reset unreachable tracking
		rc.unreachableSince = time.Time{}

		// Check if server port is reachable
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%s", ip, rc.serverPort), 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}

		log.Printf("Server %s not available (will retry)...", ip)
		time.Sleep(rc.retryInterval)
	}

	return fmt.Errorf("timeout waiting for server")
}

// runBenchmarkWithRetry runs a benchmark with automatic retry on server failure
func runBenchmarkWithRetry(ctx context.Context, cfg bench.Config, rc *RemoteController, serverType string, timeout, interval time.Duration) (*bench.Result, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// Refresh server IP from SSM if using dynamic discovery AND we don't have a working IP
		// This handles the case where a retry server has a different IP
		// Skip SSM refresh if we already have a valid server IP to avoid warning spam
		if rc.useSSM && rc.serverIP == "" {
			if err := rc.refreshServerIP(ctx); err != nil {
				log.Printf("Warning: Could not refresh server IP from SSM: %v", err)
			}
		}

		// Update the benchmark URL with current server IP
		cfg.URL = updateURLHost(cfg.URL, rc.serverIP, rc.serverPort)

		// Check server health first
		if err := rc.WaitForServer(ctx); err != nil {
			lastErr = err
			log.Printf("Server unavailable during benchmark, pausing and waiting...")

			// Server might have been terminated (spot instance)
			// Wait for it to come back (retry will bring up new instance with new IP)
			pauseStart := time.Now()
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				default:
				}

				// Refresh IP from SSM - retry server will have registered its new IP
				if rc.useSSM {
					if err := rc.refreshServerIP(ctx); err != nil {
						log.Printf("Waiting for server IP in SSM...")
						time.Sleep(interval)
						continue
					}
				}

				// Try to restart the server via control daemon
				if err := rc.StartServer(ctx, serverType); err == nil {
					log.Printf("Server recovered after %s pause (IP: %s)", time.Since(pauseStart).Round(time.Second), rc.serverIP)
					break
				}

				time.Sleep(interval)
			}
			continue
		}

		// Run the benchmark
		benchmarker := bench.New(cfg)
		result, err := benchmarker.Run(ctx)
		if err != nil {
			// Check if it's a connection error (server might have died)
			if isConnectionError(err) {
				lastErr = err
				log.Printf("Connection error during benchmark: %v, pausing...", err)
				continue
			}
			return nil, err
		}

		return result, nil
	}

	if lastErr != nil {
		return nil, fmt.Errorf("benchmark failed after retries: %w", lastErr)
	}
	return nil, fmt.Errorf("benchmark timed out")
}

// updateURLHost replaces the host:port in a URL with new values
func updateURLHost(urlStr, newHost, newPort string) string {
	// Simple replacement - assumes URL format http://host:port/path
	parts := strings.SplitN(urlStr, "/", 4)
	if len(parts) >= 4 {
		return fmt.Sprintf("%s//%s:%s/%s", parts[0], newHost, newPort, parts[3])
	} else if len(parts) == 3 {
		return fmt.Sprintf("%s//%s:%s/", parts[0], newHost, newPort)
	}
	return urlStr
}

// isConnectionError checks if the error is a connection-related error
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "no such host") ||
		strings.Contains(errStr, "network is unreachable") ||
		strings.Contains(errStr, "i/o timeout")
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

func waitForServer(ctx context.Context, host, port string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%s", host, port), 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for server on %s:%s", host, port)
}

// SpotInterruptionAction represents the AWS spot interruption notice
type SpotInterruptionAction struct {
	Action string `json:"action"`
	Time   string `json:"time"`
}

// monitorSpotInterruption polls AWS metadata for spot instance interruption notices.
// AWS provides a 2-minute warning before spot instance termination.
// When detected, it signals the interrupt channel to trigger graceful shutdown.
//
// For testing, set SIMULATE_SPOT_INTERRUPT environment variable to a duration (e.g., "30s")
// to simulate a spot interruption after that delay.
func monitorSpotInterruption(ctx context.Context, interruptChan chan<- struct{}) {
	// Check for simulation mode (for testing)
	if simDelay := os.Getenv("SIMULATE_SPOT_INTERRUPT"); simDelay != "" {
		delay, err := time.ParseDuration(simDelay)
		if err != nil {
			log.Printf("Invalid SIMULATE_SPOT_INTERRUPT value %q: %v", simDelay, err)
		} else {
			log.Printf("TESTING MODE: Will simulate spot interruption in %s", delay)
			go func() {
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
					log.Printf("SIMULATED SPOT INTERRUPTION: Triggering graceful shutdown for testing")
					close(interruptChan)
				}
			}()
			return
		}
	}

	// AWS instance metadata endpoint for spot interruption
	const metadataURL = "http://169.254.169.254/latest/meta-data/spot/instance-action"
	const pollInterval = 5 * time.Second

	// Create HTTP client with short timeout (metadata service should be fast)
	client := &http.Client{
		Timeout: 2 * time.Second,
	}

	// Check if we're even on an EC2 instance by doing an initial probe
	// If not on EC2, the metadata endpoint will timeout/fail - that's fine, just exit quietly
	initialReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	initialResp, initialErr := client.Do(initialReq)
	if initialErr != nil {
		// Not on EC2 or metadata service unavailable - silently stop monitoring
		return
	}
	_ = initialResp.Body.Close()

	log.Printf("Spot interruption monitor started (checking every %s)", pollInterval)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
			if err != nil {
				continue
			}

			resp, err := client.Do(req)
			if err != nil {
				// Network error - might not be on EC2, continue polling
				continue
			}

			if resp.StatusCode == http.StatusOK {
				// Interruption notice received!
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()

				var action SpotInterruptionAction
				if err := json.Unmarshal(body, &action); err == nil {
					log.Printf("SPOT INTERRUPTION NOTICE: action=%s, time=%s", action.Action, action.Time)
					log.Printf("Instance will be %s at %s - initiating graceful shutdown", action.Action, action.Time)
				} else {
					log.Printf("SPOT INTERRUPTION NOTICE received (raw: %s)", string(body))
				}

				// Signal the main goroutine to save checkpoint and shutdown
				close(interruptChan)
				return
			}

			// 404 = no interruption scheduled (normal case)
			_ = resp.Body.Close()
		}
	}
}

// C2Client handles communication with the C2 orchestration server
type C2Client struct {
	endpoint      string
	runID         string
	arch          string
	currentServer string // Currently benchmarking server (e.g., "gin-h1")
	currentBench  string // Currently benchmarking type (e.g., "simple")
	currentCount  int    // Current benchmark number (1-based)
	totalCount    int    // Total number of benchmarks
	progressMu    sync.RWMutex
}

// C2BenchResult is the result format expected by C2 (must match store.BenchResult)
type C2BenchResult struct {
	ServerType     string  `json:"server_type"`
	BenchmarkType  string  `json:"benchmark_type"`
	RequestsPerSec float64 `json:"requests_per_sec"`
	LatencyAvg     int64   `json:"latency_avg"` // nanoseconds
	LatencyP50     int64   `json:"latency_p50"` // nanoseconds
	LatencyP99     int64   `json:"latency_p99"` // nanoseconds
	LatencyMax     int64   `json:"latency_max"` // nanoseconds
	TotalRequests  int64   `json:"total_requests"`
	Errors         int64   `json:"errors"`
}

// runHeartbeat sends periodic heartbeats to C2
func (c *C2Client) runHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sendHeartbeat(ctx)
		}
	}
}

func (c *C2Client) sendHeartbeat(ctx context.Context) {
	url := fmt.Sprintf("%s/api/worker/heartbeat", c.endpoint)

	c.progressMu.RLock()
	currentServer := c.currentServer
	currentBench := c.currentBench
	currentCount := c.currentCount
	totalCount := c.totalCount
	c.progressMu.RUnlock()

	payload := map[string]interface{}{
		"run_id": c.runID,
		"arch":   c.arch,
		"role":   "client",
	}

	// Include progress info if available
	if currentServer != "" && currentBench != "" {
		payload["current_server"] = currentServer
		payload["current_benchmark"] = currentBench
		payload["current_count"] = currentCount
		payload["total_count"] = totalCount
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(data)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("Heartbeat failed: %v", err)
		return
	}
	_ = resp.Body.Close()
}

// sendResult sends a single benchmark result to C2
func (c *C2Client) sendResult(ctx context.Context, serverType, benchmarkType string, result *bench.Result) error {
	url := fmt.Sprintf("%s/api/worker/results", c.endpoint)

	// Convert to C2 format with proper duration values
	c2Result := C2BenchResult{
		ServerType:     serverType,
		BenchmarkType:  benchmarkType,
		RequestsPerSec: result.RequestsPerSec,
		LatencyAvg:     int64(result.Latency.Avg),
		LatencyP50:     int64(result.Latency.P50),
		LatencyP99:     int64(result.Latency.P99),
		LatencyMax:     int64(result.Latency.Max),
		TotalRequests:  result.Requests,
		Errors:         result.Errors,
	}

	payload := map[string]interface{}{
		"run_id":  c.runID,
		"arch":    c.arch,
		"results": []C2BenchResult{c2Result},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal result: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send result: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("C2 returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// sendCompletion notifies C2 that benchmarks are complete
func (c *C2Client) sendCompletion(ctx context.Context) error {
	url := fmt.Sprintf("%s/api/worker/complete", c.endpoint)

	payload := map[string]string{
		"run_id": c.runID,
		"arch":   c.arch,
		"role":   "client",
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()

	return nil
}
