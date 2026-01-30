// Package main provides the entry point for running benchmark servers.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/goceleris/benchmarks/servers/baseline/chi"
	"github.com/goceleris/benchmarks/servers/baseline/echo"
	"github.com/goceleris/benchmarks/servers/baseline/fiber"
	"github.com/goceleris/benchmarks/servers/baseline/gin"
	irisserver "github.com/goceleris/benchmarks/servers/baseline/iris"
	"github.com/goceleris/benchmarks/servers/baseline/stdhttp"
)

func main() {
	mode := flag.String("mode", "direct", "Mode: direct (run single server) or control (run control daemon)")
	serverType := flag.String("server", "stdhttp-h1", "Server type to run (direct mode only)")
	port := flag.String("port", "8080", "Port for benchmark server")
	controlPort := flag.String("control-port", "9999", "Port for control daemon (control mode only)")
	flag.Parse()

	if *mode == "control" {
		runControlDaemon(*port, *controlPort)
	} else {
		runDirectServer(*serverType, *port)
	}
}

// runDirectServer runs a single server directly (original behavior)
func runDirectServer(serverType, port string) {
	log.Printf("Starting benchmark server: %s on port %s", serverType, port)

	var err error

	switch serverType {
	case "stdhttp-h1":
		server := stdhttp.NewHTTP1Server(port)
		err = server.Run()

	case "stdhttp-h2":
		server := stdhttp.NewHTTP2Server(port)
		err = server.Run()

	case "stdhttp-hybrid":
		server := stdhttp.NewHybridServer(port)
		err = server.Run()

	case "fiber-h1":
		server := fiber.NewServer(port)
		err = server.Run()

	case "iris-h1":
		server := irisserver.NewServer(port, false)
		err = server.Run()

	case "iris-h2", "iris-hybrid":
		server := irisserver.NewServer(port, true)
		err = server.Run()

	case "gin-h1":
		server := gin.NewServer(port, false)
		err = server.Run()

	case "gin-h2", "gin-hybrid":
		server := gin.NewServer(port, true)
		err = server.Run()

	case "chi-h1":
		server := chi.NewServer(port, false)
		err = server.Run()

	case "chi-h2", "chi-hybrid":
		server := chi.NewServer(port, true)
		err = server.Run()

	case "echo-h1":
		server := echo.NewServer(port, false)
		err = server.Run()

	case "echo-h2", "echo-hybrid":
		server := echo.NewServer(port, true)
		err = server.Run()

	case "epoll-h1", "epoll-h2", "epoll-hybrid":
		log.Printf("Epoll server %s - Linux only", serverType)
		err = runEpollServer(serverType, port)

	case "iouring-h1", "iouring-h2", "iouring-hybrid":
		log.Printf("io_uring server %s - Linux 5.19+ only", serverType)
		err = runIOUringServer(serverType, port)

	default:
		fmt.Fprintf(os.Stderr, "Unknown server type: %s\n", serverType)
		fmt.Fprintf(os.Stderr, "Available types:\n")
		fmt.Fprintf(os.Stderr, "  Baseline:\n")
		fmt.Fprintf(os.Stderr, "    stdhttp-h1, stdhttp-h2, stdhttp-hybrid\n")
		fmt.Fprintf(os.Stderr, "    fiber-h1\n")
		fmt.Fprintf(os.Stderr, "    gin-h1, gin-h2, gin-hybrid\n")
		fmt.Fprintf(os.Stderr, "    chi-h1, chi-h2, chi-hybrid\n")
		fmt.Fprintf(os.Stderr, "    echo-h1, echo-h2, echo-hybrid\n")
		fmt.Fprintf(os.Stderr, "    iris-h1, iris-h2, iris-hybrid\n")
		fmt.Fprintf(os.Stderr, "  Theoretical: epoll-h1, epoll-h2, epoll-hybrid, iouring-h1, iouring-h2, iouring-hybrid\n")
		os.Exit(1)
	}

	if err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

// ControlDaemon manages benchmark server processes remotely
type ControlDaemon struct {
	mu            sync.Mutex
	serverPort    string
	currentServer string
	currentCmd    *exec.Cmd
	shutdownChan  chan struct{}
}

// ControlStatus is the response from control endpoints
type ControlStatus struct {
	ServerType string `json:"server_type"`
	Status     string `json:"status"`
	Port       string `json:"port"`
	Error      string `json:"error,omitempty"`
}

// runControlDaemon starts the control daemon
func runControlDaemon(serverPort, controlPort string) {
	log.Printf("Starting control daemon on port %s (server port: %s)", controlPort, serverPort)

	daemon := &ControlDaemon{
		serverPort:   serverPort,
		shutdownChan: make(chan struct{}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/start", daemon.handleStart)
	mux.HandleFunc("/stop", daemon.handleStop)
	mux.HandleFunc("/status", daemon.handleStatus)
	mux.HandleFunc("/health", daemon.handleHealth)
	mux.HandleFunc("/shutdown", daemon.handleShutdown)

	server := &http.Server{
		Addr:              ":" + controlPort,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Handle graceful shutdown
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

		select {
		case <-sigChan:
			log.Println("Received shutdown signal")
		case <-daemon.shutdownChan:
			log.Println("Shutdown requested via API")
		}

		daemon.stopCurrentServer()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	log.Printf("Control daemon listening on :%s", controlPort)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Control daemon error: %v", err)
	}
	log.Println("Control daemon stopped")
}

func (d *ControlDaemon) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	serverType := r.URL.Query().Get("type")
	if serverType == "" {
		d.jsonResponse(w, http.StatusBadRequest, ControlStatus{
			Status: "error",
			Error:  "missing 'type' parameter",
		})
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Stop existing server if running
	d.stopCurrentServerLocked()

	// Start new server
	log.Printf("Starting server: %s on port %s", serverType, d.serverPort)

	// Get the path to this binary
	execPath, err := os.Executable()
	if err != nil {
		d.jsonResponse(w, http.StatusInternalServerError, ControlStatus{
			Status: "error",
			Error:  fmt.Sprintf("failed to get executable path: %v", err),
		})
		return
	}

	cmd := exec.Command(execPath, "-mode", "direct", "-server", serverType, "-port", d.serverPort)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		d.jsonResponse(w, http.StatusInternalServerError, ControlStatus{
			Status: "error",
			Error:  fmt.Sprintf("failed to start server: %v", err),
		})
		return
	}

	d.currentServer = serverType
	d.currentCmd = cmd

	// Wait for server to be ready
	if err := d.waitForServer(5 * time.Second); err != nil {
		d.stopCurrentServerLocked()
		d.jsonResponse(w, http.StatusInternalServerError, ControlStatus{
			Status: "error",
			Error:  fmt.Sprintf("server failed to start: %v", err),
		})
		return
	}

	log.Printf("Server %s started successfully", serverType)

	d.jsonResponse(w, http.StatusOK, ControlStatus{
		ServerType: serverType,
		Status:     "running",
		Port:       d.serverPort,
	})
}

func (d *ControlDaemon) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.currentCmd == nil {
		d.jsonResponse(w, http.StatusOK, ControlStatus{
			Status: "stopped",
		})
		return
	}

	serverType := d.currentServer
	d.stopCurrentServerLocked()

	log.Printf("Server %s stopped", serverType)

	d.jsonResponse(w, http.StatusOK, ControlStatus{
		ServerType: serverType,
		Status:     "stopped",
	})
}

func (d *ControlDaemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.currentCmd == nil {
		d.jsonResponse(w, http.StatusOK, ControlStatus{
			Status: "idle",
			Port:   d.serverPort,
		})
		return
	}

	// Check if process is still running
	if d.currentCmd.ProcessState != nil && d.currentCmd.ProcessState.Exited() {
		d.currentCmd = nil
		d.currentServer = ""
		d.jsonResponse(w, http.StatusOK, ControlStatus{
			Status: "idle",
			Port:   d.serverPort,
		})
		return
	}

	d.jsonResponse(w, http.StatusOK, ControlStatus{
		ServerType: d.currentServer,
		Status:     "running",
		Port:       d.serverPort,
	})
}

func (d *ControlDaemon) handleHealth(w http.ResponseWriter, r *http.Request) {
	d.jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (d *ControlDaemon) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	log.Println("Shutdown requested")
	d.jsonResponse(w, http.StatusOK, map[string]string{"status": "shutting_down"})

	// Trigger shutdown in background
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(d.shutdownChan)
	}()
}

func (d *ControlDaemon) stopCurrentServer() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopCurrentServerLocked()
}

func (d *ControlDaemon) stopCurrentServerLocked() {
	if d.currentCmd == nil || d.currentCmd.Process == nil {
		return
	}

	log.Printf("Stopping server %s (PID: %d)", d.currentServer, d.currentCmd.Process.Pid)

	// Send SIGTERM
	_ = d.currentCmd.Process.Signal(syscall.SIGTERM)

	// Wait for process to exit with timeout
	done := make(chan error, 1)
	go func() {
		done <- d.currentCmd.Wait()
	}()

	select {
	case <-done:
		// Process exited
	case <-time.After(5 * time.Second):
		// Force kill
		log.Printf("Force killing server %s", d.currentServer)
		_ = d.currentCmd.Process.Kill()
		<-done
	}

	d.currentCmd = nil
	d.currentServer = ""
}

func (d *ControlDaemon) waitForServer(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("localhost:%s", d.serverPort)

	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("timeout waiting for server on %s", addr)
}

func (d *ControlDaemon) jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
