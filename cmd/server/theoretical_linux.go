//go:build linux

package main

import (
	"github.com/goceleris/benchmarks/servers/theoretical/epoll"
	"github.com/goceleris/benchmarks/servers/theoretical/iouring"
)

// runEpollServer runs the epoll-based theoretical maximum servers.
func runEpollServer(serverType, port string) error {
	switch serverType {
	case "epoll-h1":
		server := epoll.NewHTTP1Server(port)
		return server.Run()
	case "epoll-h2":
		server := epoll.NewHTTP2Server(port)
		return server.Run()
	case "epoll-hybrid":
		server := epoll.NewHybridServer(port)
		return server.Run()
	}
	return nil
}

// runIOUringServer runs the io_uring-based theoretical maximum servers.
func runIOUringServer(serverType, port string) error {
	switch serverType {
	case "iouring-h1":
		server := iouring.NewHTTP1Server(port)
		return server.Run()
	case "iouring-h2":
		server := iouring.NewHTTP2Server(port)
		return server.Run()
	case "iouring-hybrid":
		server := iouring.NewHybridServer(port)
		return server.Run()
	}
	return nil
}
