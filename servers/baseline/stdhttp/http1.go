// Package stdhttp provides baseline HTTP servers using Go's standard library.
package stdhttp

import (
	"io"
	"net/http"
	"strings"

	"github.com/goceleris/benchmarks/servers/common"
)

// HTTP1Server is a baseline HTTP/1.1 server using net/http.
type HTTP1Server struct {
	config common.ServerConfig
}

// NewHTTP1Server creates a new HTTP/1.1 baseline server.
func NewHTTP1Server(port string) *HTTP1Server {
	return &HTTP1Server{
		config: common.ServerConfig{
			Port:       port,
			ServerType: "stdhttp-h1",
		},
	}
}

// Run starts the HTTP/1.1 server.
func (s *HTTP1Server) Run() error {
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	server := &http.Server{
		Addr:              ":" + s.config.Port,
		Handler:           mux,
		MaxHeaderBytes:    1 << 20,
		ReadHeaderTimeout: 0,
		WriteTimeout:      0,
		IdleTimeout:       0,
	}

	return server.ListenAndServe()
}

func (s *HTTP1Server) registerRoutes(mux *http.ServeMux) {
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
