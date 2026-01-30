// Package main provides the C2 (Command & Control) server for benchmark orchestration.
// The C2 server is a persistent, always-on service that:
// - Exposes a REST API for triggering and monitoring benchmarks
// - Manages spot instance lifecycle for workers
// - Collects and stores benchmark results
// - Creates PRs with results
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/goceleris/benchmarks/internal/c2/api"
	"github.com/goceleris/benchmarks/internal/c2/cfn"
	"github.com/goceleris/benchmarks/internal/c2/config"
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
	defer func() { _ = logFile.Close() }()

	// Log to both file and stdout
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multiWriter)
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
	log.Printf("Using AWS region: %s", awsRegion)

	// Load configuration from SSM and CloudFormation
	ctx := context.Background()
	configLoader, err := config.NewLoader(awsRegion)
	if err != nil {
		log.Fatalf("Failed to create config loader: %v", err)
	}

	cfg, err := configLoader.Load(ctx)
	if err != nil {
		log.Printf("Warning: Config loading had errors: %v", err)
	}
	cfg.Region = awsRegion

	log.Printf("Config loaded - VpcID: %s, Subnets: %v, SecurityGroupID: %s, C2Endpoint: %s",
		cfg.VpcID, cfg.SubnetsByAZ, cfg.SecurityGroupID, cfg.C2Endpoint)

	// Initialize store
	db, err := store.New(*dataDir)
	if err != nil {
		log.Fatalf("Failed to initialize store: %v", err)
	}
	defer func() { _ = db.Close() }()
	log.Println("BadgerDB initialized")

	// Initialize AWS clients
	spotClient, err := spot.NewClient(awsRegion)
	if err != nil {
		log.Fatalf("Failed to initialize spot client: %v", err)
	}
	log.Println("Spot client initialized")

	cfnClient, err := cfn.NewClient(awsRegion)
	if err != nil {
		log.Fatalf("Failed to initialize CloudFormation client: %v", err)
	}
	log.Println("CloudFormation client initialized")

	// Initialize GitHub client (optional - only needed for PR creation)
	var ghClient *github.Client
	if cfg.GitHubToken != "" {
		ghClient, err = github.NewClientWithToken(cfg.GitHubToken)
		if err != nil {
			log.Printf("Warning: GitHub client not initialized: %v", err)
		} else {
			log.Println("GitHub client initialized")
		}
	} else {
		log.Println("Warning: GitHub token not set, PR creation disabled")
	}

	// Initialize orchestrator with full config
	orch := orchestrator.New(orchestrator.Config{
		Store:              db,
		Spot:               spotClient,
		CFN:                cfnClient,
		GitHub:             ghClient,
		AWSRegion:          awsRegion,
		SubnetsByAZ:        cfg.SubnetsByAZ,
		SecurityGroupID:    cfg.SecurityGroupID,
		InstanceProfileArn: cfg.InstanceProfileArn,
		C2Endpoint:         cfg.C2Endpoint,
	})
	log.Println("Orchestrator initialized")

	// Start background workers
	bgCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go orch.RunQueueProcessor(bgCtx)
	go orch.RunHealthChecker(bgCtx)
	go orch.RunCleanupWorker(bgCtx)
	go db.RunGC(bgCtx)
	log.Println("Background workers started (queue processor, health checker, cleanup, GC)")

	// Initialize API with config
	apiHandler := api.NewWithConfig(api.Config{
		Store:        db,
		Orchestrator: orch,
		APIKey:       cfg.APIKey,
	})
	log.Println("API handler initialized")

	// HTTP server (health checks, ACME)
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})
	// Also serve API on HTTP for internal access
	httpMux.Handle("/api/", apiHandler)

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
		if *certFile != "" && *keyFile != "" {
			log.Printf("HTTPS server listening on %s", *addr)
			if err := httpsServer.ListenAndServeTLS(*certFile, *keyFile); err != http.ErrServerClosed {
				log.Printf("HTTPS server error: %v", err)
			}
		} else {
			log.Printf("Warning: TLS not configured, HTTPS server disabled")
		}
	}()

	log.Println("=== C2 Server Ready ===")

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
