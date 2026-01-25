// Package iris provides a baseline H2C server using Iris framework.
package iris

import (
	"net/http"

	"github.com/kataras/iris/v12"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// Server is a baseline H2C server using Iris.
// Iris supports HTTP/2 cleartext via its configuration.
type Server struct {
	port   string
	app    *iris.Application
	useH2C bool
}

// NewServer creates a new Iris baseline server.
func NewServer(port string, useH2C bool) *Server {
	app := iris.New()
	app.Logger().SetLevel("warn") // Reduce logging noise

	s := &Server{
		port:   port,
		app:    app,
		useH2C: useH2C,
	}

	s.registerRoutes()
	return s
}

// Run starts the Iris server.
func (s *Server) Run() error {
	if s.useH2C {
		// Configure H2C (HTTP/2 Cleartext)
		h2cHandler := h2c.NewHandler(s.app, &http2.Server{})

		srv := &http.Server{
			Addr:    ":" + s.port,
			Handler: h2cHandler,
		}
		return srv.ListenAndServe()
	}

	// Standard HTTP/1.1
	return s.app.Listen(":"+s.port,
		iris.WithOptimizations,
		iris.WithoutServerError(iris.ErrServerClosed),
		iris.WithoutStartupLog,
	)
}

func (s *Server) registerRoutes() {
	// Simple benchmark: plain text response
	s.app.Get("/", func(ctx iris.Context) {
		ctx.ContentType("text/plain")
		_, _ = ctx.WriteString("Hello, World!")
	})

	// JSON benchmark: JSON serialization
	s.app.Get("/json", func(ctx iris.Context) {
		_ = ctx.JSON(iris.Map{
			"message": "Hello, World!",
			"server":  "iris-h2",
		})
	})

	// Path benchmark: path parameter extraction
	s.app.Get("/users/{id:string}", func(ctx iris.Context) {
		id := ctx.Params().Get("id")
		ctx.ContentType("text/plain")
		_, _ = ctx.WriteString("User ID: " + id)
	})

	// Big request benchmark: POST with body
	s.app.Post("/upload", func(ctx iris.Context) {
		_, _ = ctx.GetBody() // Read body
		ctx.ContentType("text/plain")
		_, _ = ctx.WriteString("OK")
	})
}
