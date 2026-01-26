// Package echo provides a baseline HTTP/1.1 server using Echo framework.
package echo

import (
	"io"
	"net/http"

	"github.com/labstack/echo/v4"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// Server is a baseline HTTP/1.1 server using Echo.
type Server struct {
	port   string
	e      *echo.Echo
	useH2C bool
}

// NewServer creates a new Echo baseline server.
func NewServer(port string, useH2C bool) *Server {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	s := &Server{
		port:   port,
		e:      e,
		useH2C: useH2C,
	}

	s.registerRoutes()
	return s
}

// Run starts the Echo server.
func (s *Server) Run() error {
	if s.useH2C {
		h2cHandler := h2c.NewHandler(s.e, &http2.Server{})
		server := &http.Server{
			Addr:    ":" + s.port,
			Handler: h2cHandler,
		}
		return server.ListenAndServe()
	}
	return s.e.Start(":" + s.port)
}

func (s *Server) registerRoutes() {
	s.e.GET("/", func(c echo.Context) error {
		return c.String(http.StatusOK, "Hello, World!")
	})

	s.e.GET("/json", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{
			"message": "Hello, World!",
			"server":  "echo-h1",
		})
	})

	s.e.GET("/users/:id", func(c echo.Context) error {
		id := c.Param("id")
		return c.String(http.StatusOK, "User ID: "+id)
	})

	s.e.POST("/upload", func(c echo.Context) error {
		_, _ = io.Copy(io.Discard, c.Request().Body)
		return c.String(http.StatusOK, "OK")
	})
}
