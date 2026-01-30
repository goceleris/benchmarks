// Package orchestrator coordinates benchmark runs across worker instances.
package orchestrator

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/goceleris/benchmarks/internal/c2/cfn"
	"github.com/goceleris/benchmarks/internal/c2/github"
	"github.com/goceleris/benchmarks/internal/c2/spot"
	"github.com/goceleris/benchmarks/internal/c2/store"
)

// Default timeouts
const (
	WorkerStartTimeout   = 10 * time.Minute
	BenchmarkTimeout     = 3 * time.Hour
	HealthCheckInterval  = 30 * time.Second
	WorkerHealthTimeout  = 5 * time.Minute
	CleanupInterval      = 1 * time.Hour
	QueueProcessInterval = 10 * time.Second
)

// Default durations per mode
var DefaultDurations = map[string]string{
	"fast":  "10s",
	"med":   "20s",
	"metal": "30s",
}

// Concurrent run limits per mode
var MaxConcurrentByMode = map[string]int{
	"metal": 1, // Only 1 metal run at a time (instance limits)
	"med":   2, // Up to 2 medium runs
	"fast":  5, // Up to 5 fast runs
}

// Config holds orchestrator dependencies.
type Config struct {
	Store     *store.Store
	Spot      *spot.Client
	CFN       *cfn.Client
	GitHub    *github.Client
	AWSRegion string

	// Infrastructure config (from C2 stack outputs)
	SubnetsByAZ        map[string]string // AZ -> SubnetID mapping for public subnets
	SecurityGroupID    string
	InstanceProfileArn string
	C2Endpoint         string
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

// StartRun queues a new benchmark run.
func (o *Orchestrator) StartRun(ctx context.Context, mode, duration, benchMode string) (*store.Run, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// Validate infrastructure config
	if len(o.config.SubnetsByAZ) == 0 {
		return nil, fmt.Errorf("infrastructure not configured: no subnets available")
	}
	if o.config.SecurityGroupID == "" {
		return nil, fmt.Errorf("infrastructure not configured: SecurityGroupID is missing")
	}
	if o.config.InstanceProfileArn == "" {
		return nil, fmt.Errorf("infrastructure not configured: InstanceProfileArn is missing")
	}
	if o.config.C2Endpoint == "" {
		return nil, fmt.Errorf("infrastructure not configured: C2Endpoint is missing")
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

	// Create run in queued state
	run := &store.Run{
		ID:        runID,
		Mode:      mode,
		Status:    "queued",
		QueuedAt:  time.Now(),
		Duration:  duration,
		BenchMode: benchMode,
		Workers:   make(map[string]*store.Worker),
		Results:   make(map[string]*store.ArchResult),
	}

	if err := o.config.Store.CreateRun(run); err != nil {
		return nil, fmt.Errorf("failed to create run: %w", err)
	}

	log.Printf("Queued benchmark run %s (mode: %s, duration: %s, benchMode: %s)",
		runID, mode, duration, benchMode)

	// Try to start immediately if capacity is available
	go o.tryProcessQueue()

	return run, nil
}

// tryProcessQueue attempts to start queued runs if capacity is available.
func (o *Orchestrator) tryProcessQueue() {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.processQueueLocked()
}

// processQueueLocked processes the queue (must hold mu lock).
func (o *Orchestrator) processQueueLocked() {
	// Get current running counts
	counts, err := o.config.Store.CountRunningByMode()
	if err != nil {
		log.Printf("Failed to count running runs: %v", err)
		return
	}

	// Get queued runs (sorted by priority)
	queued, err := o.config.Store.GetQueuedRuns()
	if err != nil {
		log.Printf("Failed to get queued runs: %v", err)
		return
	}

	for _, run := range queued {
		// Check if we have capacity for this mode
		maxConcurrent := MaxConcurrentByMode[run.Mode]
		if counts[run.Mode] >= maxConcurrent {
			log.Printf("Run %s waiting: %s mode at capacity (%d/%d)",
				run.ID, run.Mode, counts[run.Mode], maxConcurrent)
			continue
		}

		// Start this run
		log.Printf("Starting queued run %s (mode: %s)", run.ID, run.Mode)
		run.Status = "pending"
		run.StartedAt = time.Now()
		if err := o.config.Store.UpdateRun(run); err != nil {
			log.Printf("Failed to update run status: %v", err)
			continue
		}

		// Increment count for this mode
		counts[run.Mode]++

		// Start workers in background
		go o.runBenchmarks(context.Background(), run, run.BenchMode)
	}
}

// runBenchmarks executes the full benchmark flow.
func (o *Orchestrator) runBenchmarks(ctx context.Context, run *store.Run, benchMode string) {
	log.Printf("Starting benchmark run %s (mode: %s, duration: %s)", run.ID, run.Mode, run.Duration)

	// Update status to running
	run.Status = "running"
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now()
	}
	if err := o.config.Store.UpdateRun(run); err != nil {
		log.Printf("Failed to update run status: %v", err)
	}

	// Use stored BenchMode if not provided
	if benchMode == "" {
		benchMode = run.BenchMode
	}
	if benchMode == "" {
		benchMode = "all"
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
	_ = o.config.Store.UpdateRun(run)

	// Cleanup workers
	o.cleanupRun(ctx, run.ID)

	// Generate results and create PR
	if run.Status == "completed" {
		o.finalizeResults(ctx, run)
	}

	log.Printf("Benchmark run %s finished: %s", run.ID, run.Status)

	// Process queue to start next waiting run
	go o.tryProcessQueue()
}

// runArchitecture runs benchmarks for a single architecture.
func (o *Orchestrator) runArchitecture(ctx context.Context, run *store.Run, arch, benchMode string) error {
	log.Printf("Starting %s benchmarks for run %s", arch, run.ID)

	// Get available AZs from our subnet configuration
	var availableAZs []string
	for az := range o.config.SubnetsByAZ {
		availableAZs = append(availableAZs, az)
	}

	if len(availableAZs) == 0 {
		return fmt.Errorf("no subnets available")
	}

	// Find the best AZ considering both capacity AND price
	bestAZ, serverBid, clientBid, err := o.config.Spot.GetBestAZWithCapacity(ctx, run.Mode, arch, availableAZs)
	if err != nil {
		return fmt.Errorf("failed to find AZ with capacity: %w", err)
	}

	subnetID := o.config.SubnetsByAZ[bestAZ]
	if subnetID == "" {
		return fmt.Errorf("no subnet found for selected AZ %s", bestAZ)
	}

	log.Printf("%s: Using subnet %s in AZ %s (server: $%.4f, client: $%.4f)",
		arch, subnetID, bestAZ, serverBid.BidPrice, clientBid.BidPrice)

	// Create worker stack
	stackName, err := o.config.CFN.CreateWorkerStack(ctx, cfn.WorkerStackParams{
		RunID:              run.ID,
		Mode:               run.Mode,
		Architecture:       arch,
		AvailabilityZone:   bestAZ,
		SpotPrice:          fmt.Sprintf("%.4f", serverBid.BidPrice),
		SubnetID:           subnetID,
		SecurityGroupID:    o.config.SecurityGroupID,
		InstanceProfileArn: o.config.InstanceProfileArn,
		C2Endpoint:         o.config.C2Endpoint,
		BenchmarkDuration:  run.Duration,
		BenchmarkMode:      benchMode,
	})
	if err != nil {
		return fmt.Errorf("failed to create worker stack: %w", err)
	}

	// Wait for stack to be ready
	_, err = o.config.CFN.WaitForStack(ctx, stackName, WorkerStartTimeout)
	if err != nil {
		_ = o.config.CFN.DeleteStack(ctx, stackName)
		return fmt.Errorf("worker stack failed: %w", err)
	}

	// Wait for workers to register
	if err := o.waitForWorkers(ctx, run.ID, arch, WorkerStartTimeout); err != nil {
		_ = o.config.CFN.DeleteStack(ctx, stackName)
		return fmt.Errorf("workers did not register: %w", err)
	}

	// Wait for benchmarks to complete (with health monitoring)
	if err := o.waitForCompletion(ctx, run.ID, arch, BenchmarkTimeout); err != nil {
		return fmt.Errorf("benchmark failed: %w", err)
	}

	// Mark architecture as complete
	_ = o.config.Store.CompleteArch(run.ID, arch)

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
			log.Printf("Both workers registered for %s %s", runID, arch)
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
		log.Printf("Terminating %d orphaned instances for run %s", len(instanceIDs), runID)
		if err := o.config.Spot.TerminateInstances(ctx, instanceIDs); err != nil {
			log.Printf("Warning: Failed to terminate instances: %v", err)
		}
	}
}

// finalizeResults creates a PR with benchmark results.
func (o *Orchestrator) finalizeResults(ctx context.Context, run *store.Run) {
	log.Printf("Finalizing results for run %s", run.ID)

	// Create PR with results
	if o.config.GitHub != nil {
		if err := o.config.GitHub.CreateResultsPR(ctx, run); err != nil {
			log.Printf("Warning: Failed to create PR: %v", err)
		} else {
			log.Printf("Successfully created results PR for run %s", run.ID)
		}
	} else {
		log.Printf("GitHub client not configured, skipping PR creation")
	}
}

// CancelRun cancels a running or queued benchmark.
func (o *Orchestrator) CancelRun(ctx context.Context, runID string) error {
	run, err := o.config.Store.GetRun(runID)
	if err != nil {
		return err
	}

	if run.Status != "running" && run.Status != "pending" && run.Status != "queued" {
		return fmt.Errorf("run is not active: %s", run.Status)
	}

	wasRunning := run.Status == "running" || run.Status == "pending"

	run.Status = "cancelled"
	run.EndedAt = time.Now()
	_ = o.config.Store.UpdateRun(run)

	if wasRunning {
		o.cleanupRun(ctx, runID)
	}

	log.Printf("Cancelled run %s", runID)

	// Process queue to start next waiting run
	go o.tryProcessQueue()

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

// RunQueueProcessor periodically processes the queue.
func (o *Orchestrator) RunQueueProcessor(ctx context.Context) {
	ticker := time.NewTicker(QueueProcessInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.tryProcessQueue()
		}
	}
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
			}
		}

		// Check for timeout
		if time.Since(run.StartedAt) > BenchmarkTimeout {
			log.Printf("Run %s exceeded timeout, cancelling", run.ID)
			_ = o.CancelRun(ctx, run.ID)
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
	runs, err := o.config.Store.ListRuns("", 0)
	if err != nil {
		return
	}

	for _, run := range runs {
		// Clean up stale running runs (shouldn't happen normally)
		if run.Status == "running" && time.Since(run.StartedAt) > 24*time.Hour {
			log.Printf("Cleaning up stale run %s", run.ID)
			_ = o.CancelRun(ctx, run.ID)
		}
	}
}

// CleanupOrphaned cleans up orphaned resources (instances, stacks).
func (o *Orchestrator) CleanupOrphaned(ctx context.Context) {
	log.Println("Starting orphaned resource cleanup")

	// Find all instances tagged with celeris-benchmarks
	instances, err := o.config.Spot.DescribeAllWorkerInstances(ctx)
	if err != nil {
		log.Printf("Warning: Failed to describe instances: %v", err)
		return
	}

	// Get active run IDs
	activeRuns := make(map[string]bool)
	runs, err := o.config.Store.GetRunningRuns()
	if err == nil {
		for _, run := range runs {
			activeRuns[run.ID] = true
		}
	}

	// Terminate instances not associated with active runs
	var orphanedIDs []string
	for _, inst := range instances {
		if inst.InstanceId == nil {
			continue
		}

		// Find RunId tag
		var runID string
		for _, tag := range inst.Tags {
			if tag.Key != nil && *tag.Key == "RunId" && tag.Value != nil {
				runID = *tag.Value
				break
			}
		}

		if runID == "" || !activeRuns[runID] {
			orphanedIDs = append(orphanedIDs, *inst.InstanceId)
		}
	}

	if len(orphanedIDs) > 0 {
		log.Printf("Terminating %d orphaned instances", len(orphanedIDs))
		if err := o.config.Spot.TerminateInstances(ctx, orphanedIDs); err != nil {
			log.Printf("Warning: Failed to terminate orphaned instances: %v", err)
		}
	}

	log.Println("Orphaned resource cleanup complete")
}
