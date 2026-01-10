package stdhttp

import (
	"io"
	"net"
	"net/http"
	"strings"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/goceleris/benchmarks/servers/common"
)

// HTTP2Server is a baseline H2C (HTTP/2 Cleartext) server using net/http with x/net/http2.
// This supports HTTP/2 prior knowledge (no TLS, no upgrade).
type HTTP2Server struct {
	config common.ServerConfig
}

// NewHTTP2Server creates a new H2C baseline server.
func NewHTTP2Server(port string) *HTTP2Server {
	return &HTTP2Server{
		config: common.ServerConfig{
			Port:       port,
			ServerType: "stdhttp-h2",
		},
	}
}

// Run starts the H2C server with prior knowledge support.
func (s *HTTP2Server) Run() error {
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	// Create H2C handler that supports HTTP/2 prior knowledge
	h2s := &http2.Server{
		MaxConcurrentStreams: 1000,
		MaxReadFrameSize:     1 << 20, // 1MB
	}

	handler := h2c.NewHandler(mux, h2s)

	server := &http.Server{
		Addr:    ":" + s.config.Port,
		Handler: handler,
	}

	// Use raw listener for better control
	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}

	return server.Serve(ln)
}

func (s *HTTP2Server) registerRoutes(mux *http.ServeMux) {
	// Simple benchmark: plain text response
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		common.WriteSimple(w)
	})

	// JSON benchmark: JSON serialization
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		common.WriteJSON(w, s.config.ServerType)
	})

	// Path benchmark: path parameter extraction
	mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/users/")
		if id == "" {
			http.NotFound(w, r)
			return
		}
		common.WritePath(w, id)
	})

	// Big request benchmark: POST with body
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read body", http.StatusBadRequest)
			return
		}
		common.WriteBigRequest(w, len(body))
	})
}
