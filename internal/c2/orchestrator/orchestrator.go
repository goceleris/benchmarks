// Package orchestrator coordinates benchmark runs across worker instances.
package orchestrator

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/goceleris/benchmarks/internal/c2/cfn"
	"github.com/goceleris/benchmarks/internal/c2/github"
	"github.com/goceleris/benchmarks/internal/c2/spot"
	"github.com/goceleris/benchmarks/internal/c2/store"
	"github.com/google/uuid"
)

// Default timeouts
const (
	WorkerStartTimeout   = 10 * time.Minute
	BenchmarkTimeout     = 3 * time.Hour
	HealthCheckInterval  = 30 * time.Second
	WorkerHealthTimeout  = 5 * time.Minute
	CleanupInterval      = 1 * time.Hour
)

// Default durations per mode
var DefaultDurations = map[string]string{
	"fast":  "10s",
	"med":   "30s",
	"metal": "60s",
}

// Config holds orchestrator dependencies.
type Config struct {
	Store     *store.Store
	Spot      *spot.Client
	CFN       *cfn.Client
	GitHub    *github.Client
	AWSRegion string

	// Infrastructure config (should come from C2 stack outputs)
	SubnetID          string
	SecurityGroupID   string
	InstanceProfileArn string
	C2Endpoint        string
}

// Orchestrator coordinates benchmark runs.
type Orchestrator struct {
	config Config
	mu     sync.Mutex
}

// New creates a new orchestrator.
func New(config Config) *Orchestrator {
	return &Orchestrator{
		config: config,
	}
}

// StartRun initiates a new benchmark run.
func (o *Orchestrator) StartRun(ctx context.Context, mode, duration, benchMode string) (*store.Run, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// Check for existing running runs
	running, err := o.config.Store.GetRunningRuns()
	if err != nil {
		return nil, fmt.Errorf("failed to check running runs: %w", err)
	}
	if len(running) > 0 {
		return nil, fmt.Errorf("benchmark already in progress: %s", running[0].ID)
	}

	// Generate run ID
	runID := uuid.New().String()[:8]

	// Default duration based on mode
	if duration == "" {
		duration = DefaultDurations[mode]
	}
	if benchMode == "" {
		benchMode = "all"
	}

	// Create run
	run := &store.Run{
		ID:       runID,
		Mode:     mode,
		Status:   "pending",
		Duration: duration,
		Workers:  make(map[string]*store.Worker),
		Results:  make(map[string]*store.ArchResult),
	}

	if err := o.config.Store.CreateRun(run); err != nil {
		return nil, fmt.Errorf("failed to create run: %w", err)
	}

	// Start workers in background
	go o.runBenchmarks(context.Background(), run, benchMode)

	return run, nil
}

// runBenchmarks executes the full benchmark flow.
func (o *Orchestrator) runBenchmarks(ctx context.Context, run *store.Run, benchMode string) {
	log.Printf("Starting benchmark run %s (mode: %s, duration: %s)", run.ID, run.Mode, run.Duration)

	// Update status
	run.Status = "running"
	run.StartedAt = time.Now()
	if err := o.config.Store.UpdateRun(run); err != nil {
		log.Printf("Failed to update run status: %v", err)
	}

	// Launch workers for each architecture
	archs := []string{"arm64", "x86"}
	var wg sync.WaitGroup
	errors := make(chan error, len(archs))

	for _, arch := range archs {
		wg.Add(1)
		go func(arch string) {
			defer wg.Done()
			if err := o.runArchitecture(ctx, run, arch, benchMode); err != nil {
				log.Printf("Architecture %s failed: %v", arch, err)
				errors <- fmt.Errorf("%s: %w", arch, err)
			}
		}(arch)
	}

	wg.Wait()
	close(errors)

	// Collect errors
	var errMsgs []string
	for err := range errors {
		errMsgs = append(errMsgs, err.Error())
	}

	// Update final status
	run, _ = o.config.Store.GetRun(run.ID)
	if len(errMsgs) > 0 {
		run.Status = "failed"
		run.Error = fmt.Sprintf("errors: %v", errMsgs)
	} else {
		run.Status = "completed"
	}
	run.EndedAt = time.Now()
	o.config.Store.UpdateRun(run)

	// Cleanup workers
	o.cleanupRun(ctx, run.ID)

	// Generate results and create PR
	if run.Status == "completed" {
		o.finalizeResults(ctx, run)
	}

	log.Printf("Benchmark run %s finished: %s", run.ID, run.Status)
}

// runArchitecture runs benchmarks for a single architecture.
func (o *Orchestrator) runArchitecture(ctx context.Context, run *store.Run, arch, benchMode string) error {
	log.Printf("Starting %s benchmarks for run %s", arch, run.ID)

	// Get instance types for this mode and arch
	serverType, clientType, err := spot.GetInstancesForMode(run.Mode, arch)
	if err != nil {
		return err
	}

	// Find best AZ with pricing
	serverBid, clientBid, err := o.config.Spot.GetBestAZForPair(ctx, serverType, clientType)
	if err != nil {
		return fmt.Errorf("failed to get spot pricing: %w", err)
	}

	log.Printf("%s: Using AZ %s (server: $%.4f, client: $%.4f)",
		arch, serverBid.AZ, serverBid.BidPrice, clientBid.BidPrice)

	// Create worker stack
	stackName, err := o.config.CFN.CreateWorkerStack(ctx, cfn.WorkerStackParams{
		RunID:             run.ID,
		Mode:              run.Mode,
		Architecture:      arch,
		AvailabilityZone:  serverBid.AZ,
		SpotPrice:         fmt.Sprintf("%.4f", serverBid.BidPrice),
		SubnetID:          o.config.SubnetID,
		SecurityGroupID:   o.config.SecurityGroupID,
		InstanceProfileArn: o.config.InstanceProfileArn,
		C2Endpoint:        o.config.C2Endpoint,
		BenchmarkDuration: run.Duration,
		BenchmarkMode:     benchMode,
	})
	if err != nil {
		return fmt.Errorf("failed to create worker stack: %w", err)
	}

	// Wait for stack to be ready
	_, err = o.config.CFN.WaitForStack(ctx, stackName, WorkerStartTimeout)
	if err != nil {
		o.config.CFN.DeleteStack(ctx, stackName)
		return fmt.Errorf("worker stack failed: %w", err)
	}

	// Wait for workers to register
	if err := o.waitForWorkers(ctx, run.ID, arch, WorkerStartTimeout); err != nil {
		o.config.CFN.DeleteStack(ctx, stackName)
		return fmt.Errorf("workers did not register: %w", err)
	}

	// Wait for benchmarks to complete (with health monitoring)
	if err := o.waitForCompletion(ctx, run.ID, arch, BenchmarkTimeout); err != nil {
		return fmt.Errorf("benchmark failed: %w", err)
	}

	// Mark architecture as complete
	o.config.Store.CompleteArch(run.ID, arch)

	log.Printf("%s benchmarks completed for run %s", arch, run.ID)
	return nil
}

// waitForWorkers waits for both server and client to register.
func (o *Orchestrator) waitForWorkers(ctx context.Context, runID, arch string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		run, err := o.config.Store.GetRun(runID)
		if err != nil {
			return err
		}

		serverKey := fmt.Sprintf("%s-server", arch)
		clientKey := fmt.Sprintf("%s-client", arch)

		serverOK := run.Workers[serverKey] != nil && run.Workers[serverKey].Status == "running"
		clientOK := run.Workers[clientKey] != nil && run.Workers[clientKey].Status == "running"

		if serverOK && clientOK {
			return nil
		}

		time.Sleep(5 * time.Second)
	}

	return fmt.Errorf("timeout waiting for workers")
}

// waitForCompletion waits for benchmarks to complete with health monitoring.
func (o *Orchestrator) waitForCompletion(ctx context.Context, runID, arch string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	lastHealthy := time.Now()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		run, err := o.config.Store.GetRun(runID)
		if err != nil {
			return err
		}

		// Check if arch is complete
		if result, ok := run.Results[arch]; ok {
			if result.Status == "completed" {
				return nil
			}
			if result.Status == "failed" {
				return fmt.Errorf("benchmarks failed")
			}
		}

		// Check worker health
		clientKey := fmt.Sprintf("%s-client", arch)
		if worker, ok := run.Workers[clientKey]; ok {
			if time.Since(worker.LastSeen) < WorkerHealthTimeout {
				lastHealthy = time.Now()
			}
		}

		// Fail if no health update for too long
		if time.Since(lastHealthy) > WorkerHealthTimeout {
			return fmt.Errorf("worker health timeout")
		}

		time.Sleep(HealthCheckInterval)
	}

	return fmt.Errorf("benchmark timeout")
}

// cleanupRun terminates all workers for a run.
func (o *Orchestrator) cleanupRun(ctx context.Context, runID string) {
	log.Printf("Cleaning up workers for run %s", runID)

	// Delete CloudFormation stacks
	if err := o.config.CFN.DeleteWorkerStacks(ctx, runID); err != nil {
		log.Printf("Warning: Failed to delete worker stacks: %v", err)
	}

	// Terminate any remaining instances
	instances, err := o.config.Spot.DescribeInstances(ctx, runID)
	if err != nil {
		log.Printf("Warning: Failed to describe instances: %v", err)
		return
	}

	var instanceIDs []string
	for _, inst := range instances {
		if inst.InstanceId != nil {
			instanceIDs = append(instanceIDs, *inst.InstanceId)
		}
	}

	if len(instanceIDs) > 0 {
		if err := o.config.Spot.TerminateInstances(ctx, instanceIDs); err != nil {
			log.Printf("Warning: Failed to terminate instances: %v", err)
		}
	}
}

// finalizeResults generates charts and creates a PR.
func (o *Orchestrator) finalizeResults(ctx context.Context, run *store.Run) {
	log.Printf("Finalizing results for run %s", run.ID)

	// TODO: Generate charts
	// TODO: Create PR with results

	if o.config.GitHub != nil {
		if err := o.config.GitHub.CreateResultsPR(ctx, run); err != nil {
			log.Printf("Warning: Failed to create PR: %v", err)
		}
	}
}

// CancelRun cancels a running benchmark.
func (o *Orchestrator) CancelRun(ctx context.Context, runID string) error {
	run, err := o.config.Store.GetRun(runID)
	if err != nil {
		return err
	}

	if run.Status != "running" && run.Status != "pending" {
		return fmt.Errorf("run is not active: %s", run.Status)
	}

	run.Status = "cancelled"
	run.EndedAt = time.Now()
	o.config.Store.UpdateRun(run)

	o.cleanupRun(ctx, runID)

	return nil
}

// GetRun returns a run by ID.
func (o *Orchestrator) GetRun(runID string) (*store.Run, error) {
	return o.config.Store.GetRun(runID)
}

// ListRuns returns recent runs.
func (o *Orchestrator) ListRuns(limit int) ([]*store.Run, error) {
	return o.config.Store.ListRuns("", limit)
}

// RunHealthChecker periodically checks worker health.
func (o *Orchestrator) RunHealthChecker(ctx context.Context) {
	ticker := time.NewTicker(HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.checkRunningRuns(ctx)
		}
	}
}

// checkRunningRuns checks health of all running benchmarks.
func (o *Orchestrator) checkRunningRuns(ctx context.Context) {
	runs, err := o.config.Store.GetRunningRuns()
	if err != nil {
		log.Printf("Failed to get running runs: %v", err)
		return
	}

	for _, run := range runs {
		// Check for stale workers
		for key, worker := range run.Workers {
			if worker.Status == "running" && time.Since(worker.LastSeen) > WorkerHealthTimeout {
				log.Printf("Worker %s for run %s appears unhealthy (last seen: %s)",
					key, run.ID, worker.LastSeen)
				// Could trigger recovery here
			}
		}

		// Check for timeout
		if time.Since(run.StartedAt) > BenchmarkTimeout {
			log.Printf("Run %s exceeded timeout, cancelling", run.ID)
			o.CancelRun(ctx, run.ID)
		}
	}
}

// RunCleanupWorker periodically cleans up old data.
func (o *Orchestrator) RunCleanupWorker(ctx context.Context) {
	ticker := time.NewTicker(CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.cleanupOldRuns(ctx)
		}
	}
}

// cleanupOldRuns removes data older than TTL.
func (o *Orchestrator) cleanupOldRuns(ctx context.Context) {
	// BadgerDB handles TTL automatically, but we can also clean up orphaned resources
	runs, err := o.config.Store.ListRuns("", 0)
	if err != nil {
		return
	}

	for _, run := range runs {
		// Clean up stale running runs (shouldn't happen normally)
		if run.Status == "running" && time.Since(run.StartedAt) > 24*time.Hour {
			log.Printf("Cleaning up stale run %s", run.ID)
			o.CancelRun(ctx, run.ID)
		}
	}
}
