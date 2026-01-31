//go:build mage

// Celeris Benchmark Suite build tasks.
// Install mage: go install github.com/magefile/mage@latest
// Run: mage [target]
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
)

const (
	binDir = "bin"
)

var (
	// Colors for output
	green  = "\033[0;32m"
	yellow = "\033[1;33m"
	nc     = "\033[0m" // No Color
)

// Default target when running mage without arguments
var Default = Build

// ----------------------------------------------------------------------------
// Build targets
// ----------------------------------------------------------------------------

// Build builds all binaries (server, bench, c2)
func Build() error {
	mg.Deps(BuildServer, BuildBench, BuildC2)
	return nil
}

// BuildServer builds the server binary
func BuildServer() error {
	printGreen("Building server...")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return err
	}
	if err := sh.Run("go", "build", "-o", filepath.Join(binDir, "server"), "./cmd/server"); err != nil {
		return err
	}
	printGreen("Server build complete: %s/server", binDir)
	return nil
}

// BuildBench builds the benchmark tool
func BuildBench() error {
	printGreen("Building benchmark tool...")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return err
	}
	if err := sh.Run("go", "build", "-o", filepath.Join(binDir, "bench"), "./cmd/bench"); err != nil {
		return err
	}
	printGreen("Benchmark tool build complete: %s/bench", binDir)
	return nil
}

// BuildC2 builds the C2 server
func BuildC2() error {
	printGreen("Building C2 server...")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return err
	}
	if err := sh.Run("go", "build", "-o", filepath.Join(binDir, "c2"), "./cmd/c2"); err != nil {
		return err
	}
	printGreen("C2 server build complete: %s/c2", binDir)
	return nil
}

// BuildLinux cross-compiles all binaries for Linux amd64
func BuildLinux() error {
	printGreen("Building for Linux amd64...")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return err
	}
	env := map[string]string{"GOOS": "linux", "GOARCH": "amd64"}
	for _, cmd := range []struct{ name, path string }{
		{"server", "./cmd/server"},
		{"bench", "./cmd/bench"},
		{"c2", "./cmd/c2"},
	} {
		if err := sh.RunWith(env, "go", "build", "-o", filepath.Join(binDir, cmd.name+"-linux-amd64"), cmd.path); err != nil {
			return err
		}
	}
	printGreen("Linux amd64 build complete")
	return nil
}

// BuildLinuxArm cross-compiles all binaries for Linux arm64
func BuildLinuxArm() error {
	printGreen("Building for Linux arm64...")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return err
	}
	env := map[string]string{"GOOS": "linux", "GOARCH": "arm64"}
	for _, cmd := range []struct{ name, path string }{
		{"server", "./cmd/server"},
		{"bench", "./cmd/bench"},
		{"c2", "./cmd/c2"},
	} {
		if err := sh.RunWith(env, "go", "build", "-o", filepath.Join(binDir, cmd.name+"-linux-arm64"), cmd.path); err != nil {
			return err
		}
	}
	printGreen("Linux arm64 build complete")
	return nil
}

// BuildAll cross-compiles for all supported platforms
func BuildAll() error {
	mg.Deps(BuildLinux, BuildLinuxArm)
	return nil
}

// ----------------------------------------------------------------------------
// Code quality targets
// ----------------------------------------------------------------------------

// Lint runs golangci-lint
func Lint() error {
	printGreen("Running golangci-lint...")
	if err := ensureGolangciLint(); err != nil {
		return err
	}
	if err := sh.Run("golangci-lint", "run", "--timeout=5m", "./..."); err != nil {
		return err
	}
	printGreen("Linting complete")
	return nil
}

// Fmt formats Go code
func Fmt() error {
	printGreen("Formatting Go code...")
	if err := sh.Run("gofmt", "-s", "-w", "."); err != nil {
		return err
	}
	printGreen("Formatting complete")
	return nil
}

// Vet runs go vet
func Vet() error {
	printGreen("Running go vet...")
	if err := sh.Run("go", "vet", "./..."); err != nil {
		return err
	}
	printGreen("Vet complete")
	return nil
}

// Test runs unit tests
func Test() error {
	printGreen("Running tests...")
	if err := sh.Run("go", "test", "-v", "./..."); err != nil {
		return err
	}
	printGreen("Tests complete")
	return nil
}

// ----------------------------------------------------------------------------
// Benchmark targets
// ----------------------------------------------------------------------------

// Benchmark runs benchmarks with 30s duration
func Benchmark() error {
	mg.Deps(Build)
	printGreen("Running benchmarks...")
	return sh.Run(filepath.Join(binDir, "bench"), "-mode", "baseline", "-duration", "30s")
}

// BenchmarkQuick runs a quick benchmark for validation (5s)
func BenchmarkQuick() error {
	mg.Deps(Build)
	printGreen("Running quick benchmark validation...")
	return sh.Run(filepath.Join(binDir, "bench"), "-mode", "baseline", "-duration", "5s")
}

// ----------------------------------------------------------------------------
// Dependency management
// ----------------------------------------------------------------------------

// Deps downloads Go dependencies
func Deps() error {
	printGreen("Downloading dependencies...")
	if err := sh.Run("go", "mod", "download"); err != nil {
		return err
	}
	if err := sh.Run("go", "mod", "tidy"); err != nil {
		return err
	}
	printGreen("Dependencies ready")
	return nil
}

// ----------------------------------------------------------------------------
// Meta targets
// ----------------------------------------------------------------------------

// Check runs all checks (deps, lint, vet, build)
func Check() error {
	mg.SerialDeps(Deps, Lint, Vet, Build)
	printGreen("All checks passed")
	return nil
}

// Clean removes build artifacts
func Clean() error {
	printGreen("Cleaning...")
	if err := os.RemoveAll(binDir); err != nil {
		return err
	}
	// Clean results
	patterns := []string{"results/*.json", "results/*.png", "results/charts"}
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		for _, match := range matches {
			os.RemoveAll(match)
		}
	}
	printGreen("Clean complete")
	return nil
}

// ----------------------------------------------------------------------------
// Helper functions
// ----------------------------------------------------------------------------

func printGreen(format string, args ...interface{}) {
	fmt.Printf("%s%s%s\n", green, fmt.Sprintf(format, args...), nc)
}

func printYellow(format string, args ...interface{}) {
	fmt.Printf("%s%s%s\n", yellow, fmt.Sprintf(format, args...), nc)
}

func ensureGolangciLint() error {
	_, err := exec.LookPath("golangci-lint")
	if err != nil {
		printYellow("golangci-lint not installed. Installing...")
		return sh.Run("go", "install", "github.com/golangci/golangci-lint/cmd/golangci-lint@latest")
	}
	return nil
}

// CPUCount returns the number of CPUs available
func CPUCount() int {
	return runtime.NumCPU()
}
