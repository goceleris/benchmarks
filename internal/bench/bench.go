// Package bench provides a high-performance HTTP benchmarking tool.
package bench

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Config holds benchmark configuration.
type Config struct {
	URL         string
	Method      string
	Body        []byte
	Headers     map[string]string
	Duration    time.Duration
	Connections int
	Workers     int
	WarmupTime  time.Duration
	KeepAlive   bool
}

// DefaultConfig returns sensible defaults for benchmarking.
func DefaultConfig() Config {
	return Config{
		Method:      "GET",
		Duration:    30 * time.Second,
		Connections: 256,
		Workers:     8,
		WarmupTime:  5 * time.Second,
		KeepAlive:   true,
	}
}

// Benchmarker runs HTTP benchmarks.
type Benchmarker struct {
	config Config
	client *http.Client

	// Metrics
	requests  atomic.Int64
	errors    atomic.Int64
	bytesRead atomic.Int64

	// Latency tracking
	latencies *LatencyRecorder

	// Control
	running atomic.Bool
	wg      sync.WaitGroup
}

// New creates a new Benchmarker with the given configuration.
func New(cfg Config) *Benchmarker {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        cfg.Connections,
		MaxIdleConnsPerHost: cfg.Connections,
		MaxConnsPerHost:     cfg.Connections,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   !cfg.KeepAlive,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	return &Benchmarker{
		config: cfg,
		client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		latencies: NewLatencyRecorder(),
	}
}

// Run executes the benchmark and returns results.
func (b *Benchmarker) Run(ctx context.Context) (*Result, error) {
	// Warmup phase
	if b.config.WarmupTime > 0 {
		b.warmup(ctx)
	}

	// Reset metrics for actual benchmark
	b.requests.Store(0)
	b.errors.Store(0)
	b.bytesRead.Store(0)
	b.latencies.Reset()

	// Start workers
	b.running.Store(true)
	start := time.Now()

	for i := 0; i < b.config.Workers; i++ {
		b.wg.Add(1)
		go b.worker(ctx)
	}

	// Wait for duration
	select {
	case <-ctx.Done():
	case <-time.After(b.config.Duration):
	}

	b.running.Store(false)
	b.wg.Wait()

	elapsed := time.Since(start)

	return b.buildResult(elapsed), nil
}

func (b *Benchmarker) warmup(ctx context.Context) {
	warmupCtx, cancel := context.WithTimeout(ctx, b.config.WarmupTime)
	defer cancel()

	b.running.Store(true)

	for i := 0; i < b.config.Workers/2; i++ {
		b.wg.Add(1)
		go b.worker(warmupCtx)
	}

	<-warmupCtx.Done()
	b.running.Store(false)
	b.wg.Wait()
}

func (b *Benchmarker) worker(ctx context.Context) {
	defer b.wg.Done()

	for b.running.Load() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		start := time.Now()
		bytesRead, err := b.doRequest(ctx)
		latency := time.Since(start)

		if err != nil {
			b.errors.Add(1)
		} else {
			b.requests.Add(1)
			b.bytesRead.Add(int64(bytesRead))
			b.latencies.Record(latency)
		}
	}
}

func (b *Benchmarker) doRequest(ctx context.Context) (int, error) {
	var body io.Reader
	if len(b.config.Body) > 0 {
		body = bytes.NewReader(b.config.Body)
	}

	req, err := http.NewRequestWithContext(ctx, b.config.Method, b.config.URL, body)
	if err != nil {
		return 0, err
	}

	// Set Content-Length explicitly to avoid chunked transfer encoding
	if len(b.config.Body) > 0 {
		req.ContentLength = int64(len(b.config.Body))
	}

	for k, v := range b.config.Headers {
		req.Header.Set(k, v)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	n, _ := io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		return int(n), fmt.Errorf("status %d", resp.StatusCode)
	}

	return int(n), nil
}

func (b *Benchmarker) buildResult(elapsed time.Duration) *Result {
	reqs := b.requests.Load()
	errs := b.errors.Load()
	bytesRead := b.bytesRead.Load()

	rps := float64(reqs) / elapsed.Seconds()
	throughput := float64(bytesRead) / elapsed.Seconds()

	return &Result{
		Requests:       reqs,
		Errors:         errs,
		Duration:       elapsed,
		RequestsPerSec: rps,
		ThroughputBPS:  throughput,
		Latency:        b.latencies.Percentiles(),
	}
}
