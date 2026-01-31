// Package orchestrator coordinates benchmark runs across worker instances.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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

	// Optional S3 config for custom binaries (PR testing)
	BinaryS3Bucket string
	BinaryS3Prefix string
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

	slog.Info("benchmark run queued",
		"run_id", runID,
		"mode", mode,
		"duration", duration,
		"bench_mode", benchMode)

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
//
// LOCKING STRATEGY: This function must be called while holding the orchestrator's mu lock
// to prevent race conditions when multiple goroutines try to process the queue concurrently.
// The critical section is the check-update-start cycle:
//  1. Check current running counts from database
//  2. Update run status to "pending" in database (reserves capacity)
//  3. Re-read counts to ensure consistency for next iteration
//  4. Start the run in a background goroutine
//
// By holding the lock during the entire operation and updating the database BEFORE starting
// the run, we ensure that concurrent calls cannot both see the same old count and exceed
// the MaxConcurrentByMode limits (especially critical for metal=1).
func (o *Orchestrator) processQueueLocked() {
	// Get queued runs (sorted by priority)
	queued, err := o.config.Store.GetQueuedRuns()
	if err != nil {
		slog.Error("failed to get queued runs", "error", err)
		return
	}

	for _, run := range queued {
		// Re-read counts from database for each run to ensure we have the latest state.
		// This is necessary because we update the database status below, and we need to
		// see those changes reflected in the count for subsequent runs in this loop.
		counts, err := o.config.Store.CountRunningByMode()
		if err != nil {
			slog.Error("failed to count running runs", "error", err)
			return
		}

		// Check if we have capacity for this mode
		maxConcurrent := MaxConcurrentByMode[run.Mode]
		if counts[run.Mode] >= maxConcurrent {
			slog.Info("run waiting for capacity",
				"run_id", run.ID,
				"mode", run.Mode,
				"current", counts[run.Mode],
				"max", maxConcurrent)
			continue
		}

		// CRITICAL: Update run status to "pending" in the database BEFORE starting the run.
		// This reserves capacity and prevents other concurrent processQueueLocked calls
		// from also starting this run or exceeding the concurrent limit.
		// The CountRunningByMode() function counts both "pending" and "running" statuses,
		// so this update will be reflected in subsequent capacity checks.
		slog.Info("starting queued run",
			"run_id", run.ID,
			"mode", run.Mode)
		run.Status = "pending"
		run.StartedAt = time.Now()
		if err := o.config.Store.UpdateRun(run); err != nil {
			slog.Error("failed to update run status",
				"error", err,
				"run_id", run.ID)
			continue
		}

		// Start workers in background
		// Note: Once we release the lock, the run is in "pending" state and will be
		// counted by CountRunningByMode() in subsequent calls to processQueueLocked.
		go o.runBenchmarks(context.Background(), run, run.BenchMode)
	}
}

// runBenchmarks executes the full benchmark flow.
func (o *Orchestrator) runBenchmarks(ctx context.Context, run *store.Run, benchMode string) {
	start := time.Now()
	slog.Info("benchmark run starting",
		"run_id", run.ID,
		"mode", run.Mode,
		"duration", run.Duration)

	// Update status to running
	run.Status = "running"
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now()
	}
	if err := o.config.Store.UpdateRun(run); err != nil {
		slog.Error("failed to update run status",
			"error", err,
			"run_id", run.ID)
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
				slog.Error("architecture benchmarks failed",
					"error", err,
					"run_id", run.ID,
					"arch", arch)
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
	run, err := o.config.Store.GetRun(run.ID)
	if err != nil {
		slog.Error("failed to get run for final status update",
			"error", err,
			"run_id", run.ID,
			"operation", "GetRun")
		return
	}
	if len(errMsgs) > 0 {
		run.Status = "failed"
		run.Error = fmt.Sprintf("errors: %v", errMsgs)
	} else {
		run.Status = "completed"
	}
	run.EndedAt = time.Now()
	if err := o.config.Store.UpdateRun(run); err != nil {
		slog.Error("failed to update run with final status",
			"error", err,
			"run_id", run.ID,
			"operation", "UpdateRun",
			"status", run.Status)
	}

	// Cleanup workers
	o.cleanupRun(ctx, run.ID)

	// Generate results and create PR
	if run.Status == "completed" {
		o.finalizeResults(ctx, run)
	}

	duration := time.Since(start)
	slog.Info("benchmark run finished",
		"run_id", run.ID,
		"status", run.Status,
		"duration_seconds", duration.Seconds())

	// Process queue to start next waiting run
	go o.tryProcessQueue()
}

// runArchitecture runs benchmarks for a single architecture.
func (o *Orchestrator) runArchitecture(ctx context.Context, run *store.Run, arch, benchMode string) error {
	slog.Info("architecture benchmarks starting",
		"run_id", run.ID,
		"arch", arch,
		"mode", run.Mode)

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

	slog.Info("worker placement determined",
		"run_id", run.ID,
		"arch", arch,
		"subnet_id", subnetID,
		"availability_zone", bestAZ,
		"server_bid_price", serverBid.BidPrice,
		"client_bid_price", clientBid.BidPrice)

	// Generate presigned URLs for custom binaries if S3 config is set
	var serverBinaryUrl, benchBinaryUrl string
	if o.config.BinaryS3Bucket != "" && o.config.BinaryS3Prefix != "" {
		archSuffix := arch
		if archSuffix == "x86" {
			archSuffix = "amd64"
		}
		serverBinaryUrl = o.generateBinaryURL(ctx, fmt.Sprintf("server-linux-%s", archSuffix))
		benchBinaryUrl = o.generateBinaryURL(ctx, fmt.Sprintf("bench-linux-%s", archSuffix))
	}

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
		ServerBinaryUrl:    serverBinaryUrl,
		BenchBinaryUrl:     benchBinaryUrl,
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
	if err := o.config.Store.CompleteArch(run.ID, arch); err != nil {
		slog.Error("failed to mark architecture as complete",
			"error", err,
			"run_id", run.ID,
			"arch", arch,
			"operation", "CompleteArch")
	}

	slog.Info("architecture benchmarks completed",
		"run_id", run.ID,
		"arch", arch)
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
			slog.Info("workers registered",
				"run_id", runID,
				"arch", arch)
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
	slog.Info("cleaning up workers", "run_id", runID)

	// Delete CloudFormation stacks
	if err := o.config.CFN.DeleteWorkerStacks(ctx, runID); err != nil {
		slog.Warn("failed to delete worker stacks",
			"error", err,
			"run_id", runID)
	}

	// Terminate any remaining instances
	instances, err := o.config.Spot.DescribeInstances(ctx, runID)
	if err != nil {
		slog.Warn("failed to describe instances",
			"error", err,
			"run_id", runID)
		return
	}

	var instanceIDs []string
	for _, inst := range instances {
		if inst.InstanceId != nil {
			instanceIDs = append(instanceIDs, *inst.InstanceId)
		}
	}

	if len(instanceIDs) > 0 {
		slog.Info("terminating orphaned instances",
			"run_id", runID,
			"instance_count", len(instanceIDs))
		if err := o.config.Spot.TerminateInstances(ctx, instanceIDs); err != nil {
			slog.Warn("failed to terminate instances",
				"error", err,
				"run_id", runID)
		}
	}
}

// finalizeResults creates a PR with benchmark results.
func (o *Orchestrator) finalizeResults(ctx context.Context, run *store.Run) {
	slog.Info("finalizing results", "run_id", run.ID)

	// Create PR with results
	if o.config.GitHub != nil {
		if err := o.config.GitHub.CreateResultsPR(ctx, run); err != nil {
			slog.Warn("failed to create PR",
				"error", err,
				"run_id", run.ID)
		} else {
			slog.Info("results PR created",
				"run_id", run.ID)
		}
	} else {
		slog.Info("GitHub client not configured, skipping PR creation",
			"run_id", run.ID)
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
	if err := o.config.Store.UpdateRun(run); err != nil {
		slog.Error("failed to update run status to cancelled",
			"error", err,
			"run_id", run.ID,
			"operation", "UpdateRun")
	}

	if wasRunning {
		o.cleanupRun(ctx, runID)
	}

	slog.Info("run cancelled", "run_id", runID)

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
		slog.Error("failed to get running runs", "error", err)
		return
	}

	for _, run := range runs {
		// Check for stale workers
		for key, worker := range run.Workers {
			if worker.Status == "running" && time.Since(worker.LastSeen) > WorkerHealthTimeout {
				slog.Warn("worker unhealthy",
					"run_id", run.ID,
					"worker_id", key,
					"last_seen", worker.LastSeen,
					"timeout_seconds", WorkerHealthTimeout.Seconds())
			}
		}

		// Check for timeout
		if time.Since(run.StartedAt) > BenchmarkTimeout {
			slog.Warn("run exceeded timeout, cancelling",
				"run_id", run.ID,
				"duration_seconds", time.Since(run.StartedAt).Seconds())
			if err := o.CancelRun(ctx, run.ID); err != nil {
				slog.Error("failed to cancel timed out run",
					"error", err,
					"run_id", run.ID,
					"operation", "CancelRun")
			}
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
			slog.Warn("cleaning up stale run",
				"run_id", run.ID,
				"age_hours", time.Since(run.StartedAt).Hours())
			if err := o.CancelRun(ctx, run.ID); err != nil {
				slog.Error("failed to cancel stale run",
					"error", err,
					"run_id", run.ID,
					"operation", "CancelRun")
			}
		}
	}
}

// generateBinaryURL generates a presigned S3 URL for a binary if S3 config is set.
// Returns empty string if S3 config is not set (workers will fall back to GitHub releases).
func (o *Orchestrator) generateBinaryURL(ctx context.Context, binaryName string) string {
	if o.config.BinaryS3Bucket == "" || o.config.BinaryS3Prefix == "" {
		return ""
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(o.config.AWSRegion))
	if err != nil {
		slog.Error("failed to load AWS config for S3 presign",
			"error", err,
			"operation", "generateBinaryURL")
		return ""
	}

	client := s3.NewFromConfig(cfg)
	presignClient := s3.NewPresignClient(client)

	key := fmt.Sprintf("%s/%s", o.config.BinaryS3Prefix, binaryName)
	presignResult, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &o.config.BinaryS3Bucket,
		Key:    &key,
	}, s3.WithPresignExpires(4*time.Hour))
	if err != nil {
		slog.Error("failed to generate presigned URL",
			"error", err,
			"bucket", o.config.BinaryS3Bucket,
			"key", key,
			"operation", "generateBinaryURL")
		return ""
	}

	slog.Debug("generated presigned URL for binary",
		"binary", binaryName,
		"bucket", o.config.BinaryS3Bucket,
		"key", key)

	return presignResult.URL
}

// CleanupOrphaned cleans up orphaned resources (instances, stacks).
func (o *Orchestrator) CleanupOrphaned(ctx context.Context) {
	slog.Info("starting orphaned resource cleanup")

	// Find all instances tagged with celeris-benchmarks
	instances, err := o.config.Spot.DescribeAllWorkerInstances(ctx)
	if err != nil {
		slog.Warn("failed to describe instances", "error", err)
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
		slog.Info("terminating orphaned instances",
			"instance_count", len(orphanedIDs))
		if err := o.config.Spot.TerminateInstances(ctx, orphanedIDs); err != nil {
			slog.Warn("failed to terminate orphaned instances", "error", err)
		}
	}

	slog.Info("orphaned resource cleanup complete")
}
