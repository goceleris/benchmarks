// Package fiber provides a baseline HTTP/1.1 server using Fiber framework.
package fiber

import (
	"github.com/gofiber/fiber/v2"
)

// Server is a baseline HTTP/1.1 server using Fiber.
// Fiber only supports HTTP/1.1 natively.
type Server struct {
	port string
	app  *fiber.App
}

// NewServer creates a new Fiber baseline server.
func NewServer(port string) *Server {
	app := fiber.New(fiber.Config{
		ServerHeader:          "fiber-benchmark",
		DisableStartupMessage: true,
		Prefork:               false,
		ReadBufferSize:        16384,
		WriteBufferSize:       16384,
	})

	s := &Server{
		port: port,
		app:  app,
	}

	s.registerRoutes()
	return s
}

// Run starts the Fiber server.
func (s *Server) Run() error {
	return s.app.Listen(":" + s.port)
}

func (s *Server) registerRoutes() {
	s.app.Get("/", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "text/plain")
		return c.SendString("Hello, World!")
	})

	s.app.Get("/json", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"message": "Hello, World!",
			"server":  "fiber-h1",
		})
	})

	s.app.Get("/users/:id", func(c *fiber.Ctx) error {
		id := c.Params("id")
		c.Set("Content-Type", "text/plain")
		return c.SendString("User ID: " + id)
	})

	s.app.Post("/upload", func(c *fiber.Ctx) error {
		_ = c.Body()
		c.Set("Content-Type", "text/plain")
		return c.SendString("OK")
	})
}
