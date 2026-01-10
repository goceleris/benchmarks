// Package common provides shared types and utilities for benchmark servers.
package common

import (
	"encoding/json"
	"net/http"
)

// BenchmarkType defines the type of benchmark to run.
type BenchmarkType string

const (
	BenchmarkSimple     BenchmarkType = "simple"
	BenchmarkJSON       BenchmarkType = "json"
	BenchmarkPath       BenchmarkType = "path"
	BenchmarkBigRequest BenchmarkType = "big-request"
)

// ServerConfig holds configuration for benchmark servers.
type ServerConfig struct {
	Port          string
	ServerType    string // "stdhttp-h1", "stdhttp-h2", "fiber", "iris", etc.
	BenchmarkType BenchmarkType
}

// JSONResponse is the standard JSON response for the /json endpoint.
type JSONResponse struct {
	Message string `json:"message"`
	Server  string `json:"server"`
}

// WriteSimple writes a simple "Hello, World!" response.
func WriteSimple(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Hello, World!"))
}

// WriteJSON writes a JSON response.
func WriteJSON(w http.ResponseWriter, serverType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(JSONResponse{
		Message: "Hello, World!",
		Server:  serverType,
	})
}

// WritePath writes a response with the extracted path parameter.
func WritePath(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("User ID: " + id))
}

// WriteBigRequest handles the big request benchmark (reads body, writes OK).
func WriteBigRequest(w http.ResponseWriter, bodySize int) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}
