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
	"log/slog"
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

	// Setup logging with slog
	if err := os.MkdirAll(*logDir, 0755); err != nil {
		log.Fatalf("Failed to create log directory: %v", err)
	}
	logFile, err := os.OpenFile(*logDir+"/c2.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("Failed to open log file: %v", err)
	}
	defer func() { _ = logFile.Close() }()

	// Log to both file and stdout with JSON formatting
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	logger := slog.New(slog.NewJSONHandler(multiWriter, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// Keep old log package for compatibility during transition
	log.SetOutput(multiWriter)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	slog.Info("C2 server starting")

	// AWS Region
	awsRegion := *region
	if awsRegion == "" {
		awsRegion = os.Getenv("AWS_REGION")
	}
	if awsRegion == "" {
		awsRegion = "us-east-1"
	}
	slog.Info("AWS region configured", "region", awsRegion)

	// Load configuration from SSM and CloudFormation
	ctx := context.Background()
	configLoader, err := config.NewLoader(awsRegion)
	if err != nil {
		slog.Error("failed to create config loader", "error", err)
		os.Exit(1)
	}

	cfg, err := configLoader.Load(ctx)
	if err != nil {
		slog.Warn("config loading had errors", "error", err)
	}
	cfg.Region = awsRegion

	slog.Info("configuration loaded",
		"vpc_id", cfg.VpcID,
		"subnets", cfg.SubnetsByAZ,
		"security_group_id", cfg.SecurityGroupID,
		"c2_endpoint", cfg.C2Endpoint)

	// Initialize store
	db, err := store.New(*dataDir)
	if err != nil {
		slog.Error("failed to initialize store", "error", err, "data_dir", *dataDir)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()
	slog.Info("BadgerDB initialized", "data_dir", *dataDir)

	// Initialize AWS clients
	spotClient, err := spot.NewClient(awsRegion)
	if err != nil {
		slog.Error("failed to initialize spot client", "error", err, "region", awsRegion)
		os.Exit(1)
	}
	slog.Info("spot client initialized", "region", awsRegion)

	cfnClient, err := cfn.NewClient(awsRegion)
	if err != nil {
		slog.Error("failed to initialize CloudFormation client", "error", err, "region", awsRegion)
		os.Exit(1)
	}
	slog.Info("CloudFormation client initialized", "region", awsRegion)

	// Initialize GitHub client (optional - only needed for PR creation)
	var ghClient *github.Client
	if cfg.GitHubToken != "" {
		ghClient, err = github.NewClientWithToken(cfg.GitHubToken)
		if err != nil {
			slog.Warn("GitHub client not initialized", "error", err)
		} else {
			slog.Info("GitHub client initialized")
		}
	} else {
		slog.Warn("GitHub token not set, PR creation disabled")
	}

	// Optional S3 config for custom binaries (PR testing)
	binaryS3Bucket := os.Getenv("BINARY_S3_BUCKET")
	binaryS3Prefix := os.Getenv("BINARY_S3_PREFIX")
	if binaryS3Bucket != "" && binaryS3Prefix != "" {
		slog.Info("custom binary S3 config set",
			"bucket", binaryS3Bucket,
			"prefix", binaryS3Prefix)
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
		BinaryS3Bucket:     binaryS3Bucket,
		BinaryS3Prefix:     binaryS3Prefix,
	})
	slog.Info("orchestrator initialized")

	// Start background workers
	bgCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go orch.RunQueueProcessor(bgCtx)
	go orch.RunHealthChecker(bgCtx)
	go orch.RunCleanupWorker(bgCtx)
	go db.RunGC(bgCtx)
	slog.Info("background workers started",
		"workers", []string{"queue_processor", "health_checker", "cleanup", "gc"})

	// Initialize API with config
	apiHandler := api.NewWithConfig(api.Config{
		Store:        db,
		Orchestrator: orch,
		APIKey:       cfg.APIKey,
	})
	slog.Info("API handler initialized")

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
		slog.Info("HTTP server listening", "addr", *httpAddr)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
		}
	}()

	go func() {
		if *certFile != "" && *keyFile != "" {
			slog.Info("HTTPS server listening", "addr", *addr)
			if err := httpsServer.ListenAndServeTLS(*certFile, *keyFile); err != http.ErrServerClosed {
				slog.Error("HTTPS server error", "error", err)
			}
		} else {
			slog.Warn("TLS not configured, HTTPS server disabled")
		}
	}()

	slog.Info("C2 server ready")

	<-sigChan
	slog.Info("shutting down server")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	cancel() // Stop background workers

	if err := httpsServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTPS server shutdown error", "error", err)
	}
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
	}

	slog.Info("C2 server stopped")
}
