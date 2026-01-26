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

// HybridServer is a baseline server that automatically handles both HTTP/1.1 and H2C.
// It supports:
// - HTTP/1.1 requests
// - HTTP/2 prior knowledge (connection preface)
// - HTTP/2 upgrade from HTTP/1.1
type HybridServer struct {
	config common.ServerConfig
}

// NewHybridServer creates a new hybrid HTTP/1.1 + H2C baseline server.
func NewHybridServer(port string) *HybridServer {
	return &HybridServer{
		config: common.ServerConfig{
			Port:       port,
			ServerType: "stdhttp-hybrid",
		},
	}
}

// Run starts the hybrid server.
func (s *HybridServer) Run() error {
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	h2s := &http2.Server{
		MaxConcurrentStreams: 1000,
		MaxReadFrameSize:     1 << 20,
	}

	handler := h2c.NewHandler(mux, h2s)

	server := &http.Server{
		Addr:           ":" + s.config.Port,
		Handler:        handler,
		MaxHeaderBytes: 1 << 20,
	}

	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}

	return server.Serve(ln)
}

func (s *HybridServer) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		common.WriteSimple(w)
	})

	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		common.WriteJSON(w, s.config.ServerType)
	})

	mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/users/")
		if id == "" {
			http.NotFound(w, r)
			return
		}
		common.WritePath(w, id)
	})

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
