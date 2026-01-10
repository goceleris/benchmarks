// Package gin provides a baseline HTTP/1.1 server using Gin framework.
package gin

import (
	"github.com/gin-gonic/gin"
)

// Server is a baseline HTTP/1.1 server using Gin.
type Server struct {
	port   string
	engine *gin.Engine
}

// NewServer creates a new Gin baseline server.
func NewServer(port string) *Server {
	gin.SetMode(gin.ReleaseMode)

	engine := gin.New()
	// Skip default middleware for maximum performance
	engine.UseRawPath = true

	s := &Server{
		port:   port,
		engine: engine,
	}

	s.registerRoutes()
	return s
}

// Run starts the Gin server.
func (s *Server) Run() error {
	return s.engine.Run(":" + s.port)
}

func (s *Server) registerRoutes() {
	// Simple benchmark: plain text response
	s.engine.GET("/", func(c *gin.Context) {
		c.Header("Content-Type", "text/plain")
		c.String(200, "Hello, World!")
	})

	// JSON benchmark: JSON serialization
	s.engine.GET("/json", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"message": "Hello, World!",
			"server":  "gin-h1",
		})
	})

	// Path benchmark: path parameter extraction
	s.engine.GET("/users/:id", func(c *gin.Context) {
		id := c.Param("id")
		c.Header("Content-Type", "text/plain")
		c.String(200, "User ID: "+id)
	})

	// Big request benchmark: POST with body
	s.engine.POST("/upload", func(c *gin.Context) {
		_, _ = c.GetRawData() // Read body (benchmarks body parsing)
		c.Header("Content-Type", "text/plain")
		c.String(200, "OK")
	})
}
