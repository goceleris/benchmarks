// Package echo provides a baseline HTTP/1.1 server using Echo framework.
package echo

import (
	"io"
	"net/http"

	"github.com/labstack/echo/v4"
)

// Server is a baseline HTTP/1.1 server using Echo.
type Server struct {
	port string
	e    *echo.Echo
}

// NewServer creates a new Echo baseline server.
func NewServer(port string) *Server {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	s := &Server{
		port: port,
		e:    e,
	}

	s.registerRoutes()
	return s
}

// Run starts the Echo server.
func (s *Server) Run() error {
	return s.e.Start(":" + s.port)
}

func (s *Server) registerRoutes() {
	// Simple benchmark: plain text response
	s.e.GET("/", func(c echo.Context) error {
		return c.String(http.StatusOK, "Hello, World!")
	})

	// JSON benchmark: JSON serialization
	s.e.GET("/json", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{
			"message": "Hello, World!",
			"server":  "echo-h1",
		})
	})

	// Path benchmark: path parameter extraction
	s.e.GET("/users/:id", func(c echo.Context) error {
		id := c.Param("id")
		return c.String(http.StatusOK, "User ID: "+id)
	})

	// Big request benchmark: POST with body
	s.e.POST("/upload", func(c echo.Context) error {
		_, _ = io.Copy(io.Discard, c.Request().Body) // Read body
		return c.String(http.StatusOK, "OK")
	})
}
