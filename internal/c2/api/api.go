// Package api provides the REST API for the C2 server.
package api

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/goceleris/benchmarks/internal/c2/orchestrator"
	"github.com/goceleris/benchmarks/internal/c2/store"
)

// Config holds API dependencies.
type Config struct {
	Store        *store.Store
	Orchestrator *orchestrator.Orchestrator
}

// Handler is the main API handler.
type Handler struct {
	config Config
	apiKey string
	mux    *http.ServeMux
}

// New creates a new API handler.
func New(config Config) *Handler {
	h := &Handler{
		config: config,
		apiKey: os.Getenv("C2_API_KEY"),
		mux:    http.NewServeMux(),
	}

	// If no API key set, try SSM
	if h.apiKey == "" {
		// TODO: Fetch from SSM Parameter Store
		log.Println("Warning: C2_API_KEY not set, API authentication disabled")
	}

	// Register routes
	h.mux.HandleFunc("/api/benchmark/start", h.authMiddleware(h.handleStartBenchmark))
	h.mux.HandleFunc("/api/benchmark/", h.authMiddleware(h.handleBenchmark))
	h.mux.HandleFunc("/api/results", h.authMiddleware(h.handleListResults))
	h.mux.HandleFunc("/api/results/", h.authMiddleware(h.handleGetResult))
	h.mux.HandleFunc("/api/worker/register", h.handleWorkerRegister)
	h.mux.HandleFunc("/api/worker/assignment", h.handleWorkerAssignment)
	h.mux.HandleFunc("/api/worker/heartbeat", h.handleWorkerHeartbeat)
	h.mux.HandleFunc("/api/worker/complete", h.handleWorkerComplete)
	h.mux.HandleFunc("/api/worker/results", h.handleWorkerResults)
	h.mux.HandleFunc("/api/health", h.handleHealth)
	h.mux.HandleFunc("/api/admin/", h.authMiddleware(h.handleAdmin))

	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	h.mux.ServeHTTP(w, r)
}

// authMiddleware checks for valid API key.
func (h *Handler) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.apiKey != "" {
			key := r.Header.Get("X-API-Key")
			if key != h.apiKey {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

// handleHealth returns health status.
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleStartBenchmark starts a new benchmark run.
func (h *Handler) handleStartBenchmark(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "fast"
	}
	if mode != "fast" && mode != "med" && mode != "metal" {
		http.Error(w, "Invalid mode", http.StatusBadRequest)
		return
	}

	duration := r.URL.Query().Get("duration")
	benchMode := r.URL.Query().Get("bench_mode")

	run, err := h.config.Orchestrator.StartRun(r.Context(), mode, duration, benchMode)
	if err != nil {
		log.Printf("Failed to start benchmark: %v", err)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"run_id": run.ID,
		"status": run.Status,
	})
}

// handleBenchmark handles operations on a specific benchmark run.
func (h *Handler) handleBenchmark(w http.ResponseWriter, r *http.Request) {
	// Extract run ID from path: /api/benchmark/{run_id}/...
	path := strings.TrimPrefix(r.URL.Path, "/api/benchmark/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Missing run ID", http.StatusBadRequest)
		return
	}
	runID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "status", "":
		h.handleBenchmarkStatus(w, r, runID)
	case "results":
		h.handleBenchmarkResults(w, r, runID)
	case "cancel":
		h.handleBenchmarkCancel(w, r, runID)
	default:
		http.Error(w, "Unknown action", http.StatusNotFound)
	}
}

// handleBenchmarkStatus returns the status of a run.
func (h *Handler) handleBenchmarkStatus(w http.ResponseWriter, r *http.Request, runID string) {
	run, err := h.config.Orchestrator.GetRun(runID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"run_id":     run.ID,
		"status":     run.Status,
		"mode":       run.Mode,
		"started_at": run.StartedAt,
		"ended_at":   run.EndedAt,
		"error":      run.Error,
		"workers":    run.Workers,
		"results":    run.Results,
	})
}

// handleBenchmarkResults returns the results of a run.
func (h *Handler) handleBenchmarkResults(w http.ResponseWriter, r *http.Request, runID string) {
	run, err := h.config.Orchestrator.GetRun(runID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(run.Results)
}

// handleBenchmarkCancel cancels a running benchmark.
func (h *Handler) handleBenchmarkCancel(w http.ResponseWriter, r *http.Request, runID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := h.config.Orchestrator.CancelRun(r.Context(), runID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "cancelled"})
}

// handleListResults lists all benchmark runs.
func (h *Handler) handleListResults(w http.ResponseWriter, r *http.Request) {
	runs, err := h.config.Orchestrator.ListRuns(50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(runs)
}

// handleGetResult returns a specific result.
func (h *Handler) handleGetResult(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimPrefix(r.URL.Path, "/api/results/")
	if runID == "" {
		http.Error(w, "Missing run ID", http.StatusBadRequest)
		return
	}

	run, err := h.config.Orchestrator.GetRun(runID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(run)
}

// Worker API endpoints (called by worker instances)

type workerRegisterRequest struct {
	RunID      string `json:"run_id"`
	Role       string `json:"role"`
	Arch       string `json:"arch"`
	InstanceID string `json:"instance_id"`
	PrivateIP  string `json:"private_ip"`
}

// handleWorkerRegister registers a worker with the C2.
func (h *Handler) handleWorkerRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req workerRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	worker := &store.Worker{
		RunID:      req.RunID,
		Role:       req.Role,
		Arch:       req.Arch,
		InstanceID: req.InstanceID,
		PrivateIP:  req.PrivateIP,
	}

	if err := h.config.Store.RegisterWorker(worker); err != nil {
		log.Printf("Failed to register worker: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	log.Printf("Worker registered: %s %s-%s (%s)", req.RunID, req.Arch, req.Role, req.PrivateIP)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "registered"})
}

// handleWorkerAssignment returns the server IP for a client worker.
func (h *Handler) handleWorkerAssignment(w http.ResponseWriter, r *http.Request) {
	runID := r.URL.Query().Get("run_id")
	arch := r.URL.Query().Get("arch")

	if runID == "" || arch == "" {
		http.Error(w, "Missing parameters", http.StatusBadRequest)
		return
	}

	serverIP, err := h.config.Store.GetServerIP(runID, arch)
	if err != nil {
		// Server not ready yet
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"server_ip": serverIP})
}

// handleWorkerHeartbeat updates worker health status.
func (h *Handler) handleWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		RunID string `json:"run_id"`
		Arch  string `json:"arch"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	_ = h.config.Store.UpdateWorkerStatus(req.RunID, req.Arch, req.Role, "running")

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleWorkerComplete marks a worker as complete.
func (h *Handler) handleWorkerComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		RunID      string `json:"run_id"`
		Arch       string `json:"arch"`
		Role       string `json:"role"`
		InstanceID string `json:"instance_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	_ = h.config.Store.UpdateWorkerStatus(req.RunID, req.Arch, req.Role, "completed")

	if req.Role == "client" {
		// Client completion triggers arch completion
		_ = h.config.Store.CompleteArch(req.RunID, req.Arch)
	}

	log.Printf("Worker completed: %s %s-%s", req.RunID, req.Arch, req.Role)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleWorkerResults receives benchmark results from a worker.
func (h *Handler) handleWorkerResults(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		RunID   string              `json:"run_id"`
		Arch    string              `json:"arch"`
		Results []store.BenchResult `json:"results"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	if err := h.config.Store.SaveBenchmarkResults(req.RunID, req.Arch, req.Results); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	log.Printf("Received %d results for %s %s", len(req.Results), req.RunID, req.Arch)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleAdmin handles admin operations.
func (h *Handler) handleAdmin(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimPrefix(r.URL.Path, "/api/admin/")

	switch action {
	case "logs":
		// TODO: Return recent logs
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("Logs endpoint not implemented yet"))
	case "update":
		// TODO: Trigger self-update
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "not implemented"})
	case "restart":
		// TODO: Trigger restart
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "not implemented"})
	default:
		http.Error(w, "Unknown action", http.StatusNotFound)
	}
}
