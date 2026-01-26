// Package gin provides a baseline HTTP/1.1 server using Gin framework.
package gin

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// Server is a baseline HTTP/1.1 server using Gin.
type Server struct {
	port   string
	engine *gin.Engine
	useH2C bool
}

// NewServer creates a new Gin baseline server.
func NewServer(port string, useH2C bool) *Server {
	gin.SetMode(gin.ReleaseMode)

	engine := gin.New()
	engine.UseRawPath = true

	s := &Server{
		port:   port,
		engine: engine,
		useH2C: useH2C,
	}

	s.registerRoutes()
	return s
}

// Run starts the Gin server.
func (s *Server) Run() error {
	if s.useH2C {
		h2cHandler := h2c.NewHandler(s.engine, &http2.Server{})
		server := &http.Server{
			Addr:    ":" + s.port,
			Handler: h2cHandler,
		}
		return server.ListenAndServe()
	}
	return s.engine.Run(":" + s.port)
}

func (s *Server) registerRoutes() {
	s.engine.GET("/", func(c *gin.Context) {
		c.Header("Content-Type", "text/plain")
		c.String(200, "Hello, World!")
	})

	s.engine.GET("/json", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"message": "Hello, World!",
			"server":  "gin-h1",
		})
	})

	s.engine.GET("/users/:id", func(c *gin.Context) {
		id := c.Param("id")
		c.Header("Content-Type", "text/plain")
		c.String(200, "User ID: "+id)
	})

	s.engine.POST("/upload", func(c *gin.Context) {
		_, _ = c.GetRawData()
		c.Header("Content-Type", "text/plain")
		c.String(200, "OK")
	})
}
