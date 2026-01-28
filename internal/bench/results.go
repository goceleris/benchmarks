package bench

import (
	"encoding/json"
	"os"
	"time"
)

// Result holds the benchmark results.
type Result struct {
	Requests       int64         `json:"requests"`
	Errors         int64         `json:"errors"`
	Duration       time.Duration `json:"duration"`
	RequestsPerSec float64       `json:"requests_per_sec"`
	ThroughputBPS  float64       `json:"throughput_bps"`
	Latency        Percentiles   `json:"latency"`
}

// Percentiles holds latency percentile values.
type Percentiles struct {
	Avg   time.Duration `json:"avg"`
	Min   time.Duration `json:"min"`
	Max   time.Duration `json:"max"`
	P50   time.Duration `json:"p50"`
	P75   time.Duration `json:"p75"`
	P90   time.Duration `json:"p90"`
	P99   time.Duration `json:"p99"`
	P999  time.Duration `json:"p99_9"`
	P9999 time.Duration `json:"p99_99"`
}

// BenchmarkOutput is the JSON output format for the benchmark runner.
type BenchmarkOutput struct {
	Timestamp    string          `json:"timestamp"`
	Architecture string          `json:"architecture"`
	Config       BenchmarkConfig `json:"config"`
	Results      []ServerResult  `json:"results"`
}

// BenchmarkConfig holds the benchmark configuration in output.
type BenchmarkConfig struct {
	Duration    string `json:"duration"`
	Connections int    `json:"connections"`
	Workers     int    `json:"workers"`
	CPUs        int    `json:"cpus,omitempty"`
}

// ServerResult holds results for a single server benchmark.
type ServerResult struct {
	Server         string        `json:"server"`
	Benchmark      string        `json:"benchmark"`
	Method         string        `json:"method"`
	Path           string        `json:"path"`
	RequestsPerSec float64       `json:"requests_per_sec"`
	TransferPerSec string        `json:"transfer_per_sec,omitempty"`
	Latency        LatencyResult `json:"latency"`
	TargetRate     int           `json:"target_rate,omitempty"`
}

// LatencyResult holds latency data in output format.
type LatencyResult struct {
	Avg   string `json:"avg,omitempty"`
	Max   string `json:"max,omitempty"`
	P50   string `json:"p50,omitempty"`
	P75   string `json:"p75,omitempty"`
	P90   string `json:"p90,omitempty"`
	P99   string `json:"p99,omitempty"`
	P999  string `json:"p99.9,omitempty"`
	P9999 string `json:"p99.99,omitempty"`
}

// ToServerResult converts Result to ServerResult format.
func (r *Result) ToServerResult(server, benchmark, method, path string) ServerResult {
	return ServerResult{
		Server:         server,
		Benchmark:      benchmark,
		Method:         method,
		Path:           path,
		RequestsPerSec: r.RequestsPerSec,
		TransferPerSec: formatBytes(r.ThroughputBPS) + "/s",
		Latency: LatencyResult{
			Avg:   r.Latency.Avg.String(),
			Max:   r.Latency.Max.String(),
			P50:   r.Latency.P50.String(),
			P75:   r.Latency.P75.String(),
			P90:   r.Latency.P90.String(),
			P99:   r.Latency.P99.String(),
			P999:  r.Latency.P999.String(),
			P9999: r.Latency.P9999.String(),
		},
	}
}

// ToJSON serializes the output to JSON.
func (o *BenchmarkOutput) ToJSON() ([]byte, error) {
	return json.MarshalIndent(o, "", "  ")
}

// Checkpoint tracks benchmark progress for incremental execution.
type Checkpoint struct {
	BenchmarkOutput
	Completed map[string]bool `json:"completed"` // key: "server:benchmark"
}

// NewCheckpoint creates a new checkpoint from a BenchmarkOutput.
func NewCheckpoint(output *BenchmarkOutput) *Checkpoint {
	return &Checkpoint{
		BenchmarkOutput: *output,
		Completed:       make(map[string]bool),
	}
}

// LoadCheckpoint loads a checkpoint from a file.
func LoadCheckpoint(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, err
	}
	if cp.Completed == nil {
		cp.Completed = make(map[string]bool)
	}
	return &cp, nil
}

// Save writes the checkpoint to a file atomically.
func (cp *Checkpoint) Save(path string) error {
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	// Write to temp file first, then rename for atomic operation
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// IsCompleted checks if a server:benchmark combination is done.
func (cp *Checkpoint) IsCompleted(server, benchmark string) bool {
	return cp.Completed[server+":"+benchmark]
}

// MarkCompleted marks a server:benchmark combination as done.
func (cp *Checkpoint) MarkCompleted(server, benchmark string) {
	cp.Completed[server+":"+benchmark] = true
}

// AddResult adds a result and marks it as completed.
func (cp *Checkpoint) AddResult(result ServerResult) {
	cp.Results = append(cp.Results, result)
	cp.MarkCompleted(result.Server, result.Benchmark)
}

// ToBenchmarkOutput converts checkpoint to final output (without checkpoint metadata).
func (cp *Checkpoint) ToBenchmarkOutput() *BenchmarkOutput {
	return &BenchmarkOutput{
		Timestamp:    cp.Timestamp,
		Architecture: cp.Architecture,
		Config:       cp.Config,
		Results:      cp.Results,
	}
}

// MergeResults merges results from another checkpoint, avoiding duplicates.
func (cp *Checkpoint) MergeResults(other *Checkpoint) {
	for _, result := range other.Results {
		key := result.Server + ":" + result.Benchmark
		if !cp.Completed[key] {
			cp.Results = append(cp.Results, result)
			cp.Completed[key] = true
		}
	}
}

func formatBytes(b float64) string {
	const unit = 1024
	if b < unit {
		return formatFloat(b) + "B"
	}
	div, exp := float64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return formatFloat(b/div) + string("KMGTPE"[exp]) + "B"
}

func formatFloat(f float64) string {
	if f >= 100 {
		return java_like_format(f, 0)
	} else if f >= 10 {
		return java_like_format(f, 1)
	}
	return java_like_format(f, 2)
}

func java_like_format(f float64, precision int) string {
	format := "%." + string('0'+byte(precision)) + "f"
	return sprintf(format, f)
}

func sprintf(format string, a ...interface{}) string {
	switch format {
	case "%.0f":
		return sprintfInt(a[0].(float64))
	case "%.1f":
		return sprintfDec(a[0].(float64), 1)
	case "%.2f":
		return sprintfDec(a[0].(float64), 2)
	default:
		return sprintfDec(a[0].(float64), 2)
	}
}

func sprintfInt(f float64) string {
	return itoa(int(f + 0.5))
}

func sprintfDec(f float64, decimals int) string {
	intPart := int(f)
	fracPart := f - float64(intPart)

	result := itoa(intPart) + "."
	for i := 0; i < decimals; i++ {
		fracPart *= 10
		result += string('0' + byte(int(fracPart)%10))
	}
	return result
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
