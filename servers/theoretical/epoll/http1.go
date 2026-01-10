//go:build linux

// Package epoll provides barebones HTTP servers using raw epoll for maximum throughput.
// These implementations are intentionally minimal to test the theoretical I/O limits.
package epoll

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	maxEvents    = 1024
	readBufSize  = 4096
	writeBufSize = 4096
)

// HTTP/1.1 response templates (pre-formatted for zero-allocation)
var (
	responseSimple = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 13\r\nConnection: keep-alive\r\n\r\nHello, World!")
	responseJSON   = []byte(`HTTP/1.1 200 OK` + "\r\n" + `Content-Type: application/json` + "\r\n" + `Content-Length: 47` + "\r\n" + `Connection: keep-alive` + "\r\n\r\n" + `{"message":"Hello, World!","server":"epoll-h1"}`)
	responseOK     = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\nConnection: keep-alive\r\n\r\nOK")
	response404    = []byte("HTTP/1.1 404 Not Found\r\nContent-Type: text/plain\r\nContent-Length: 9\r\nConnection: close\r\n\r\nNot Found")
)

// HTTP1Server is a barebones HTTP/1.1 server using raw epoll.
type HTTP1Server struct {
	port     string
	epollFd  int
	listenFd int
	readBufs map[int][]byte
}

// NewHTTP1Server creates a new barebones epoll HTTP/1.1 server.
func NewHTTP1Server(port string) *HTTP1Server {
	return &HTTP1Server{
		port:     port,
		readBufs: make(map[int][]byte),
	}
}

// Run starts the epoll event loop.
func (s *HTTP1Server) Run() error {
	// Create listening socket
	listenFd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	s.listenFd = listenFd

	// Set socket options
	unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	unix.SetsockoptInt(listenFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)

	// Parse port
	var portNum int
	fmt.Sscanf(s.port, "%d", &portNum)

	// Bind
	addr := &unix.SockaddrInet4{Port: portNum}
	if err := unix.Bind(listenFd, addr); err != nil {
		return fmt.Errorf("bind: %w", err)
	}

	// Listen with large backlog
	if err := unix.Listen(listenFd, 4096); err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	// Create epoll instance
	epollFd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return fmt.Errorf("epoll_create1: %w", err)
	}
	s.epollFd = epollFd

	// Add listen socket to epoll
	event := &unix.EpollEvent{
		Events: unix.EPOLLIN | unix.EPOLLET,
		Fd:     int32(listenFd),
	}
	if err := unix.EpollCtl(epollFd, unix.EPOLL_CTL_ADD, listenFd, event); err != nil {
		return fmt.Errorf("epoll_ctl add listen: %w", err)
	}

	log.Printf("epoll-h1 server listening on port %s", s.port)
	return s.eventLoop()
}

func (s *HTTP1Server) eventLoop() error {
	events := make([]unix.EpollEvent, maxEvents)

	for {
		n, err := unix.EpollWait(s.epollFd, events, -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return fmt.Errorf("epoll_wait: %w", err)
		}

		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)

			if fd == s.listenFd {
				// Accept new connections
				s.acceptConnections()
			} else if events[i].Events&unix.EPOLLIN != 0 {
				// Read data
				s.handleRead(fd)
			} else if events[i].Events&(unix.EPOLLERR|unix.EPOLLHUP) != 0 {
				// Connection closed or error
				s.closeConnection(fd)
			}
		}
	}
}

func (s *HTTP1Server) acceptConnections() {
	for {
		connFd, _, err := unix.Accept4(s.listenFd, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				break
			}
			continue
		}

		// Set TCP_NODELAY for low latency
		unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)

		// Add to epoll (edge-triggered)
		event := &unix.EpollEvent{
			Events: unix.EPOLLIN | unix.EPOLLET,
			Fd:     int32(connFd),
		}
		unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_ADD, connFd, event)

		// Allocate read buffer
		s.readBufs[connFd] = make([]byte, readBufSize)
	}
}

func (s *HTTP1Server) handleRead(fd int) {
	buf := s.readBufs[fd]
	if buf == nil {
		buf = make([]byte, readBufSize)
		s.readBufs[fd] = buf
	}

	for {
		n, err := unix.Read(fd, buf)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				break
			}
			s.closeConnection(fd)
			return
		}
		if n == 0 {
			s.closeConnection(fd)
			return
		}

		// Parse request and send response
		s.handleRequest(fd, buf[:n])
	}
}

func (s *HTTP1Server) handleRequest(fd int, data []byte) {
	// Minimal HTTP/1.1 parsing - just look at first line
	var response []byte

	if bytes.HasPrefix(data, []byte("GET / ")) {
		response = responseSimple
	} else if bytes.HasPrefix(data, []byte("GET /json")) {
		response = responseJSON
	} else if bytes.HasPrefix(data, []byte("GET /users/")) {
		// Extract user ID from path
		lineEnd := bytes.Index(data, []byte("\r\n"))
		if lineEnd > 0 {
			path := string(data[11:lineEnd]) // Skip "GET /users/"
			spaceIdx := bytes.Index([]byte(path), []byte(" "))
			if spaceIdx > 0 {
				id := path[:spaceIdx]
				resp := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: keep-alive\r\n\r\nUser ID: %s", 9+len(id), id)
				response = []byte(resp)
			}
		}
		if response == nil {
			response = response404
		}
	} else if bytes.HasPrefix(data, []byte("POST /upload")) {
		response = responseOK
	} else {
		response = response404
	}

	// Write response directly
	unix.Write(fd, response)
}

func (s *HTTP1Server) closeConnection(fd int) {
	unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_DEL, fd, nil)
	unix.Close(fd)
	delete(s.readBufs, fd)
}

// ListenAddr returns the server's listen address.
func (s *HTTP1Server) ListenAddr() net.Addr {
	return nil // Not implemented for barebones server
}
