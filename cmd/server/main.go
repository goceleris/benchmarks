// Package main provides the entry point for running benchmark servers.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/goceleris/benchmarks/servers/baseline/chi"
	"github.com/goceleris/benchmarks/servers/baseline/echo"
	"github.com/goceleris/benchmarks/servers/baseline/fiber"
	"github.com/goceleris/benchmarks/servers/baseline/gin"
	irisserver "github.com/goceleris/benchmarks/servers/baseline/iris"
	"github.com/goceleris/benchmarks/servers/baseline/stdhttp"
)

func main() {
	serverType := flag.String("server", "stdhttp-h1", "Server type to run")
	port := flag.String("port", "8080", "Port to listen on")
	flag.Parse()

	log.Printf("Starting benchmark server: %s on port %s", *serverType, *port)

	var err error

	switch *serverType {
	case "stdhttp-h1":
		server := stdhttp.NewHTTP1Server(*port)
		err = server.Run()

	case "stdhttp-h2":
		server := stdhttp.NewHTTP2Server(*port)
		err = server.Run()

	case "stdhttp-hybrid":
		server := stdhttp.NewHybridServer(*port)
		err = server.Run()

	case "fiber-h1":
		server := fiber.NewServer(*port)
		err = server.Run()

	case "iris-h1":
		server := irisserver.NewServer(*port, false)
		err = server.Run()

	case "iris-h2", "iris-hybrid":
		server := irisserver.NewServer(*port, true)
		err = server.Run()

	case "gin-h1":
		server := gin.NewServer(*port, false)
		err = server.Run()

	case "gin-h2", "gin-hybrid":
		server := gin.NewServer(*port, true)
		err = server.Run()

	case "chi-h1":
		server := chi.NewServer(*port, false)
		err = server.Run()

	case "chi-h2", "chi-hybrid":
		server := chi.NewServer(*port, true)
		err = server.Run()

	case "echo-h1":
		server := echo.NewServer(*port, false)
		err = server.Run()

	case "echo-h2", "echo-hybrid":
		server := echo.NewServer(*port, true)
		err = server.Run()

	case "epoll-h1", "epoll-h2", "epoll-hybrid":
		log.Printf("Epoll server %s - Linux only", *serverType)
		err = runEpollServer(*serverType, *port)

	case "iouring-h1", "iouring-h2", "iouring-hybrid":
		log.Printf("io_uring server %s - Linux 5.19+ only", *serverType)
		err = runIOUringServer(*serverType, *port)

	default:
		fmt.Fprintf(os.Stderr, "Unknown server type: %s\n", *serverType)
		fmt.Fprintf(os.Stderr, "Available types:\n")
		fmt.Fprintf(os.Stderr, "  Baseline: stdhttp-h1, stdhttp-h2, stdhttp-hybrid, fiber-h1, iris-h2, gin-h1, chi-h1, echo-h1\n")
		fmt.Fprintf(os.Stderr, "  Theoretical: epoll-h1, epoll-h2, epoll-hybrid, iouring-h1, iouring-h2, iouring-hybrid\n")
		os.Exit(1)
	}

	if err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
