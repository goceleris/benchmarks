// Package main provides a control daemon for managing benchmark servers.
// It runs on the server machine and accepts commands from the client.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// ServerStatus represents the current state of the server
type ServerStatus struct {
	ServerType string    `json:"server_type"`
	Status     string    `json:"status"` // "stopped", "starting", "running", "stopping"
	Port       string    `json:"port"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// ControlDaemon manages server processes
type ControlDaemon struct {
	mu         sync.Mutex
	serverBin  string
	serverPort string
	status     ServerStatus
	cmd        *exec.Cmd
	cancelFunc context.CancelFunc
}

func main() {
	controlPort := flag.String("control-port", "9999", "Control API port")
	serverBin := flag.String("server-bin", "", "Path to server binary")
	serverPort := flag.String("server-port", "8080", "Port for benchmark servers")
	flag.Parse()

	if *serverBin == "" {
		candidates := []string{"./bin/server", "./server", "bin/server"}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				*serverBin = c
				break
			}
		}
		if *serverBin == "" {
			log.Fatal("Could not find server binary")
		}
	}

	daemon := &ControlDaemon{
		serverBin:  *serverBin,
		serverPort: *serverPort,
		status: ServerStatus{
			Status: "stopped",
			Port:   *serverPort,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/status", daemon.handleStatus)
	mux.HandleFunc("/start", daemon.handleStart)
	mux.HandleFunc("/stop", daemon.handleStop)
	mux.HandleFunc("/health", daemon.handleHealth)

	server := &http.Server{
		Addr:    ":" + *controlPort,
		Handler: mux,
	}

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down control daemon...")
		daemon.stopServer()
		_ = server.Shutdown(context.Background())
	}()

	log.Printf("Control daemon listening on :%s (server binary: %s)", *controlPort, *serverBin)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func (d *ControlDaemon) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func (d *ControlDaemon) handleStatus(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	status := d.status
	d.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (d *ControlDaemon) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	serverType := r.URL.Query().Get("type")
	if serverType == "" {
		http.Error(w, "Missing 'type' parameter", http.StatusBadRequest)
		return
	}

	d.mu.Lock()
	if d.status.Status == "running" || d.status.Status == "starting" {
		d.mu.Unlock()
		// Stop existing server first
		d.stopServer()
		d.mu.Lock()
	}
	d.mu.Unlock()

	if err := d.startServer(serverType); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	d.mu.Lock()
	status := d.status
	d.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (d *ControlDaemon) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	d.stopServer()

	d.mu.Lock()
	status := d.status
	d.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (d *ControlDaemon) startServer(serverType string) error {
	d.mu.Lock()
	d.status = ServerStatus{
		ServerType: serverType,
		Status:     "starting",
		Port:       d.serverPort,
	}
	d.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, d.serverBin, "-server", serverType, "-port", d.serverPort)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		cancel()
		d.mu.Lock()
		d.status.Status = "stopped"
		d.status.Error = err.Error()
		d.mu.Unlock()
		return fmt.Errorf("failed to start server: %w", err)
	}

	d.mu.Lock()
	d.cmd = cmd
	d.cancelFunc = cancel
	d.mu.Unlock()

	// Wait for server to be ready
	if err := d.waitForReady(5 * time.Second); err != nil {
		d.stopServer()
		return fmt.Errorf("server not ready: %w", err)
	}

	d.mu.Lock()
	d.status.Status = "running"
	d.status.StartedAt = time.Now()
	d.status.Error = ""
	d.mu.Unlock()

	// Monitor process in background
	go func() {
		err := cmd.Wait()
		d.mu.Lock()
		if d.cmd == cmd {
			d.status.Status = "stopped"
			if err != nil {
				d.status.Error = err.Error()
			}
			d.cmd = nil
			d.cancelFunc = nil
		}
		d.mu.Unlock()
	}()

	return nil
}

func (d *ControlDaemon) stopServer() {
	d.mu.Lock()
	cmd := d.cmd
	cancel := d.cancelFunc
	d.status.Status = "stopping"
	d.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		d.mu.Lock()
		d.status.Status = "stopped"
		d.mu.Unlock()
		return
	}

	// Send SIGTERM
	_ = cmd.Process.Signal(syscall.SIGTERM)

	// Wait with timeout
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		if cancel != nil {
			cancel()
		}
		_ = cmd.Process.Kill()
		<-done
	}

	d.mu.Lock()
	d.status.Status = "stopped"
	d.cmd = nil
	d.cancelFunc = nil
	d.mu.Unlock()
}

func (d *ControlDaemon) waitForReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("localhost:%s", d.serverPort)

	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/")
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for server")
}
