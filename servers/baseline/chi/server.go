// Package chi provides a baseline HTTP/1.1 server using Chi router.
package chi

import (
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// Server is a baseline HTTP/1.1 server using Chi router.
type Server struct {
	port   string
	router chi.Router
	useH2C bool
}

// NewServer creates a new Chi baseline server.
func NewServer(port string, useH2C bool) *Server {
	router := chi.NewRouter()

	s := &Server{
		port:   port,
		router: router,
		useH2C: useH2C,
	}

	s.registerRoutes()
	return s
}

// Run starts the Chi server.
func (s *Server) Run() error {
	var handler http.Handler = s.router
	if s.useH2C {
		handler = h2c.NewHandler(s.router, &http2.Server{})
	}
	return http.ListenAndServe(":"+s.port, handler)
}

func (s *Server) registerRoutes() {
	// Simple benchmark: plain text response
	s.router.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("Hello, World!"))
	})

	// JSON benchmark: JSON serialization
	s.router.Get("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":"Hello, World!","server":"chi-h1"}`))
	})

	// Path benchmark: path parameter extraction
	s.router.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("User ID: " + id))
	})

	// Big request benchmark: POST with body
	s.router.Post("/upload", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // Read body (benchmarks body parsing)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("OK"))
	})
}
