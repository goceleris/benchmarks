// Package main provides the C2 (Command & Control) server for benchmark orchestration.
// The C2 server is a persistent, always-on service that:
// - Exposes a REST API for triggering and monitoring benchmarks
// - Manages spot instance lifecycle for workers
// - Collects and stores benchmark results
// - Generates charts and creates PRs with results
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/goceleris/benchmarks/internal/c2/api"
	"github.com/goceleris/benchmarks/internal/c2/cfn"
	"github.com/goceleris/benchmarks/internal/c2/github"
	"github.com/goceleris/benchmarks/internal/c2/orchestrator"
	"github.com/goceleris/benchmarks/internal/c2/spot"
	"github.com/goceleris/benchmarks/internal/c2/store"
)

func main() {
	// Flags
	addr := flag.String("addr", ":8443", "HTTPS listen address")
	httpAddr := flag.String("http-addr", ":8080", "HTTP listen address (for health checks and ACME)")
	dataDir := flag.String("data-dir", "/data/badger", "BadgerDB data directory")
	logDir := flag.String("log-dir", "/data/logs", "Log directory")
	certFile := flag.String("cert", "", "TLS certificate file (auto-generated if empty)")
	keyFile := flag.String("key", "", "TLS key file (auto-generated if empty)")
	region := flag.String("region", "", "AWS region (defaults to AWS_REGION env)")
	flag.Parse()

	// Setup logging
	if err := os.MkdirAll(*logDir, 0755); err != nil {
		log.Fatalf("Failed to create log directory: %v", err)
	}
	logFile, err := os.OpenFile(*logDir+"/c2.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("Failed to open log file: %v", err)
	}
	defer logFile.Close()
	log.SetOutput(logFile)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	log.Println("=== C2 Server Starting ===")

	// AWS Region
	awsRegion := *region
	if awsRegion == "" {
		awsRegion = os.Getenv("AWS_REGION")
	}
	if awsRegion == "" {
		awsRegion = "us-east-1"
	}

	// Initialize store
	db, err := store.New(*dataDir)
	if err != nil {
		log.Fatalf("Failed to initialize store: %v", err)
	}
	defer db.Close()

	// Initialize AWS clients
	spotClient, err := spot.NewClient(awsRegion)
	if err != nil {
		log.Fatalf("Failed to initialize spot client: %v", err)
	}

	cfnClient, err := cfn.NewClient(awsRegion)
	if err != nil {
		log.Fatalf("Failed to initialize CloudFormation client: %v", err)
	}

	// Initialize GitHub client
	ghClient, err := github.NewClient()
	if err != nil {
		log.Printf("Warning: GitHub client not initialized: %v", err)
	}

	// Initialize orchestrator
	orch := orchestrator.New(orchestrator.Config{
		Store:     db,
		Spot:      spotClient,
		CFN:       cfnClient,
		GitHub:    ghClient,
		AWSRegion: awsRegion,
	})

	// Start background workers
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go orch.RunHealthChecker(ctx)
	go orch.RunCleanupWorker(ctx)
	go db.RunGC(ctx)

	// Initialize API
	apiHandler := api.New(api.Config{
		Store:        db,
		Orchestrator: orch,
	})

	// HTTP server (health checks, ACME)
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	httpServer := &http.Server{
		Addr:    *httpAddr,
		Handler: httpMux,
	}

	// HTTPS server (API)
	httpsServer := &http.Server{
		Addr:         *addr,
		Handler:      apiHandler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("HTTP server listening on %s", *httpAddr)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	go func() {
		log.Printf("HTTPS server listening on %s", *addr)
		if *certFile != "" && *keyFile != "" {
			if err := httpsServer.ListenAndServeTLS(*certFile, *keyFile); err != http.ErrServerClosed {
				log.Printf("HTTPS server error: %v", err)
			}
		} else {
			// For development/initial setup, run without TLS
			log.Println("Warning: Running without TLS (use -cert and -key for production)")
			if err := httpsServer.ListenAndServe(); err != http.ErrServerClosed {
				log.Printf("Server error: %v", err)
			}
		}
	}()

	<-sigChan
	log.Println("Shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	cancel() // Stop background workers

	if err := httpsServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTPS server shutdown error: %v", err)
	}
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	log.Println("=== C2 Server Stopped ===")
}
