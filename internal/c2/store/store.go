// Package store provides a BadgerDB-based storage layer for the C2 server.
// It handles benchmark run state, results, and worker registration with automatic TTL-based cleanup.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/dgraph-io/badger/v4"
)

const (
	// TTL for benchmark data (30 days)
	DataTTL = 30 * 24 * time.Hour

	// Key prefixes
	prefixRun     = "run:"
	prefixWorker  = "worker:"
	prefixResult  = "result:"
	prefixMetrics = "metrics:"
)

// Store wraps BadgerDB with benchmark-specific operations.
type Store struct {
	db *badger.DB
}

// Run represents a benchmark run.
type Run struct {
	ID        string    `json:"id"`
	Mode      string    `json:"mode"`   // fast, med, metal
	Status    string    `json:"status"` // queued, pending, running, completed, failed, cancelled
	QueuedAt  time.Time `json:"queued_at"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	Duration  string    `json:"duration"`   // benchmark duration per test
	BenchMode string    `json:"bench_mode"` // all, baseline, theoretical
	Error     string    `json:"error,omitempty"`

	// Worker tracking
	Workers map[string]*Worker `json:"workers"` // key: arch-role (e.g., "arm64-server")

	// Results per architecture
	Results map[string]*ArchResult `json:"results"` // key: arch
}

// Worker represents a registered worker instance.
type Worker struct {
	RunID        string    `json:"run_id"`
	Role         string    `json:"role"` // server, client
	Arch         string    `json:"arch"` // arm64, x86
	InstanceID   string    `json:"instance_id"`
	PrivateIP    string    `json:"private_ip"`
	Status       string    `json:"status"` // pending, running, completed, failed, terminated
	RegisteredAt time.Time `json:"registered_at"`
	LastSeen     time.Time `json:"last_seen"`
}

// ArchResult holds benchmark results for one architecture.
type ArchResult struct {
	Arch         string        `json:"arch"`
	Status       string        `json:"status"` // pending, running, completed, failed
	StartedAt    time.Time     `json:"started_at"`
	CompletedAt  time.Time     `json:"completed_at,omitempty"`
	Benchmarks   []BenchResult `json:"benchmarks"`
	InstanceType string        `json:"instance_type"`
	WasSpot      bool          `json:"was_spot"`
}

// BenchResult holds results for a single benchmark.
type BenchResult struct {
	ServerType     string        `json:"server_type"`
	BenchmarkType  string        `json:"benchmark_type"`
	RequestsPerSec float64       `json:"requests_per_sec"`
	LatencyAvg     time.Duration `json:"latency_avg"`
	LatencyP50     time.Duration `json:"latency_p50"`
	LatencyP99     time.Duration `json:"latency_p99"`
	LatencyMax     time.Duration `json:"latency_max"`
	TotalRequests  int64         `json:"total_requests"`
	Errors         int64         `json:"errors"`
}

// New creates a new Store with the given data directory.
func New(dataDir string) (*Store, error) {
	opts := badger.DefaultOptions(dataDir)
	opts.Logger = nil // Disable badger's internal logging
	opts.SyncWrites = true

	db, err := badger.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to open badger: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// RunGC runs garbage collection periodically.
func (s *Store) RunGC(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := s.db.RunValueLogGC(0.5)
			if err != nil && err != badger.ErrNoRewrite {
				log.Printf("BadgerDB GC error: %v", err)
			}
		}
	}
}

// CreateRun creates a new benchmark run.
func (s *Store) CreateRun(run *Run) error {
	// Set defaults only if not already set
	if run.Status == "" {
		run.Status = "pending"
	}
	if run.QueuedAt.IsZero() {
		run.QueuedAt = time.Now()
	}
	if run.Workers == nil {
		run.Workers = make(map[string]*Worker)
	}
	if run.Results == nil {
		run.Results = make(map[string]*ArchResult)
	}

	return s.saveRun(run)
}

// GetRun retrieves a run by ID.
func (s *Store) GetRun(id string) (*Run, error) {
	var run Run
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(prefixRun + id))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &run)
		})
	})
	if err == badger.ErrKeyNotFound {
		return nil, fmt.Errorf("run not found: %s", id)
	}
	return &run, err
}

// UpdateRun updates an existing run.
func (s *Store) UpdateRun(run *Run) error {
	return s.saveRun(run)
}

// ListRuns returns all runs, optionally filtered by status.
func (s *Store) ListRuns(status string, limit int) ([]*Run, error) {
	var runs []*Run

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(prefixRun)
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			var run Run
			err := item.Value(func(val []byte) error {
				return json.Unmarshal(val, &run)
			})
			if err != nil {
				continue
			}

			if status == "" || run.Status == status {
				runs = append(runs, &run)
				if limit > 0 && len(runs) >= limit {
					break
				}
			}
		}
		return nil
	})

	return runs, err
}

// RegisterWorker registers a worker for a run.
func (s *Store) RegisterWorker(worker *Worker) error {
	run, err := s.GetRun(worker.RunID)
	if err != nil {
		return err
	}

	worker.RegisteredAt = time.Now()
	worker.LastSeen = time.Now()
	worker.Status = "running"

	key := fmt.Sprintf("%s-%s", worker.Arch, worker.Role)
	run.Workers[key] = worker

	// Initialize arch result if needed
	if _, ok := run.Results[worker.Arch]; !ok {
		run.Results[worker.Arch] = &ArchResult{
			Arch:      worker.Arch,
			Status:    "pending",
			StartedAt: time.Now(),
		}
	}

	return s.saveRun(run)
}

// UpdateWorkerStatus updates a worker's status and last seen time.
func (s *Store) UpdateWorkerStatus(runID, arch, role, status string) error {
	run, err := s.GetRun(runID)
	if err != nil {
		return err
	}

	key := fmt.Sprintf("%s-%s", arch, role)
	if worker, ok := run.Workers[key]; ok {
		worker.Status = status
		worker.LastSeen = time.Now()
	}

	return s.saveRun(run)
}

// GetServerIP returns the server IP for a given run and architecture.
func (s *Store) GetServerIP(runID, arch string) (string, error) {
	run, err := s.GetRun(runID)
	if err != nil {
		return "", err
	}

	key := fmt.Sprintf("%s-server", arch)
	if worker, ok := run.Workers[key]; ok && worker.Status == "running" {
		return worker.PrivateIP, nil
	}

	return "", fmt.Errorf("server not found for run %s arch %s", runID, arch)
}

// SaveBenchmarkResults saves benchmark results for an architecture.
func (s *Store) SaveBenchmarkResults(runID, arch string, results []BenchResult) error {
	run, err := s.GetRun(runID)
	if err != nil {
		return err
	}

	if archResult, ok := run.Results[arch]; ok {
		archResult.Benchmarks = append(archResult.Benchmarks, results...)
		archResult.Status = "running"
	}

	return s.saveRun(run)
}

// CompleteArch marks an architecture's benchmarks as complete.
func (s *Store) CompleteArch(runID, arch string) error {
	run, err := s.GetRun(runID)
	if err != nil {
		return err
	}

	if archResult, ok := run.Results[arch]; ok {
		archResult.Status = "completed"
		archResult.CompletedAt = time.Now()
	}

	// Check if all architectures are complete
	allComplete := true
	for _, result := range run.Results {
		if result.Status != "completed" && result.Status != "failed" {
			allComplete = false
			break
		}
	}

	if allComplete {
		run.Status = "completed"
		run.EndedAt = time.Now()
	}

	return s.saveRun(run)
}

// FailRun marks a run as failed.
func (s *Store) FailRun(runID, errorMsg string) error {
	run, err := s.GetRun(runID)
	if err != nil {
		return err
	}

	run.Status = "failed"
	run.Error = errorMsg
	run.EndedAt = time.Now()

	return s.saveRun(run)
}

// saveRun saves a run with TTL.
func (s *Store) saveRun(run *Run) error {
	data, err := json.Marshal(run)
	if err != nil {
		return err
	}

	return s.db.Update(func(txn *badger.Txn) error {
		entry := badger.NewEntry([]byte(prefixRun+run.ID), data).WithTTL(DataTTL)
		return txn.SetEntry(entry)
	})
}

// DeleteRun deletes a run.
func (s *Store) DeleteRun(id string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Delete([]byte(prefixRun + id))
	})
}

// GetRunningRuns returns all runs that are currently in progress.
func (s *Store) GetRunningRuns() ([]*Run, error) {
	return s.ListRuns("running", 0)
}

// GetQueuedRuns returns all queued runs sorted by priority (metal > med > fast) then by queue time.
func (s *Store) GetQueuedRuns() ([]*Run, error) {
	runs, err := s.ListRuns("queued", 0)
	if err != nil {
		return nil, err
	}

	// Sort by priority then queue time
	priorityOrder := map[string]int{"metal": 0, "med": 1, "fast": 2}

	// Simple bubble sort (runs list is typically small)
	for i := 0; i < len(runs); i++ {
		for j := i + 1; j < len(runs); j++ {
			pi := priorityOrder[runs[i].Mode]
			pj := priorityOrder[runs[j].Mode]
			// Lower priority number = higher priority
			// If same priority, earlier queue time wins
			if pi > pj || (pi == pj && runs[i].QueuedAt.After(runs[j].QueuedAt)) {
				runs[i], runs[j] = runs[j], runs[i]
			}
		}
	}

	return runs, nil
}

// GetActiveRuns returns all runs that are queued, pending, or running.
func (s *Store) GetActiveRuns() ([]*Run, error) {
	var runs []*Run

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(prefixRun)
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			var run Run
			err := item.Value(func(val []byte) error {
				return json.Unmarshal(val, &run)
			})
			if err != nil {
				continue
			}

			if run.Status == "queued" || run.Status == "pending" || run.Status == "running" {
				runs = append(runs, &run)
			}
		}
		return nil
	})

	return runs, err
}

// CountRunningByMode returns the count of running/pending runs for each mode.
func (s *Store) CountRunningByMode() (map[string]int, error) {
	counts := map[string]int{"metal": 0, "med": 0, "fast": 0}

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(prefixRun)
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			var run Run
			err := item.Value(func(val []byte) error {
				return json.Unmarshal(val, &run)
			})
			if err != nil {
				continue
			}

			// Count pending and running (not queued)
			if run.Status == "pending" || run.Status == "running" {
				counts[run.Mode]++
			}
		}
		return nil
	})

	return counts, err
}

// GetQueuePosition returns the position of a run in the queue (1-based), or 0 if not queued.
func (s *Store) GetQueuePosition(runID string) (int, error) {
	queued, err := s.GetQueuedRuns()
	if err != nil {
		return 0, err
	}

	for i, run := range queued {
		if run.ID == runID {
			return i + 1, nil
		}
	}

	return 0, nil
}
