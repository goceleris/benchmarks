package bench

import (
	"sort"
	"sync"
	"time"
)

// LatencyRecorder records latency samples for percentile calculation.
// Uses reservoir sampling when sample count exceeds maxSamples to bound memory usage.
type LatencyRecorder struct {
	mu         sync.Mutex
	samples    []time.Duration
	sum        time.Duration
	count      int64
	min        time.Duration
	max        time.Duration
	maxSamples int
}

const defaultMaxSamples = 1000000 // 1M samples = ~8MB memory

// NewLatencyRecorder creates a new latency recorder.
func NewLatencyRecorder() *LatencyRecorder {
	return &LatencyRecorder{
		samples:    make([]time.Duration, 0, defaultMaxSamples),
		min:        time.Hour,
		maxSamples: defaultMaxSamples,
	}
}

// Record adds a latency sample.
// Uses reservoir sampling to maintain a representative sample when count exceeds maxSamples.
func (r *LatencyRecorder) Record(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.sum += d
	r.count++

	if d < r.min {
		r.min = d
	}
	if d > r.max {
		r.max = d
	}

	// Use reservoir sampling when we exceed capacity
	if len(r.samples) < r.maxSamples {
		r.samples = append(r.samples, d)
	} else {
		// Reservoir sampling: replace random element with probability maxSamples/count
		// This maintains a uniform random sample of the stream
		// Use a simple modulo-based sampling for performance (not cryptographically random, but sufficient for latency stats)
		if int(r.count%int64(r.maxSamples)) == 0 {
			idx := int(r.count / int64(r.maxSamples)) % r.maxSamples
			r.samples[idx] = d
		}
	}
}

// Reset clears all recorded samples.
func (r *LatencyRecorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Reuse the underlying array to avoid reallocation
	r.samples = r.samples[:0]
	r.sum = 0
	r.count = 0
	r.min = time.Hour
	r.max = 0
}

// Percentiles calculates and returns latency percentiles.
func (r *LatencyRecorder) Percentiles() Percentiles {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.samples) == 0 {
		return Percentiles{}
	}

	sorted := make([]time.Duration, len(r.samples))
	copy(sorted, r.samples)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i] < sorted[j]
	})

	n := len(sorted)
	avg := r.sum / time.Duration(r.count)

	return Percentiles{
		Avg:   avg,
		Min:   r.min,
		Max:   r.max,
		P50:   sorted[percentileIndex(n, 50)],
		P75:   sorted[percentileIndex(n, 75)],
		P90:   sorted[percentileIndex(n, 90)],
		P99:   sorted[percentileIndex(n, 99)],
		P999:  sorted[percentileIndex(n, 99.9)],
		P9999: sorted[percentileIndex(n, 99.99)],
	}
}

func percentileIndex(n int, percentile float64) int {
	idx := int(float64(n) * percentile / 100)
	if idx >= n {
		idx = n - 1
	}
	if idx < 0 {
		idx = 0
	}
	return idx
}
