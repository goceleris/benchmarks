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
	readBufSize  = 8192 // Larger buffer for POST bodies
	writeBufSize = 4096
)

// HTTP/1.1 response templates (pre-formatted for zero-allocation)
var (
	responseSimple = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 13\r\nConnection: keep-alive\r\n\r\nHello, World!")
	responseJSON   = []byte(`HTTP/1.1 200 OK` + "\r\n" + `Content-Type: application/json` + "\r\n" + `Content-Length: 47` + "\r\n" + `Connection: keep-alive` + "\r\n\r\n" + `{"message":"Hello, World!","server":"epoll-h1"}`)
	responseOK     = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\nConnection: keep-alive\r\n\r\nOK")
	response404    = []byte("HTTP/1.1 404 Not Found\r\nContent-Type: text/plain\r\nContent-Length: 9\r\nConnection: keep-alive\r\n\r\nNot Found")
)

// connState tracks per-connection state for request accumulation.
type connState struct {
	buf []byte
	pos int
}

// HTTP1Server is a barebones HTTP/1.1 server using raw epoll.
type HTTP1Server struct {
	port      string
	epollFd   int
	listenFd  int
	connState map[int]*connState
}

// NewHTTP1Server creates a new barebones epoll HTTP/1.1 server.
func NewHTTP1Server(port string) *HTTP1Server {
	return &HTTP1Server{
		port:      port,
		connState: make(map[int]*connState),
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
	_ = unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	_ = unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	_ = unix.SetsockoptInt(listenFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)

	// Parse port
	var portNum int
	_, _ = fmt.Sscanf(s.port, "%d", &portNum)

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
		_ = unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)

		// Add to epoll (edge-triggered)
		event := &unix.EpollEvent{
			Events: unix.EPOLLIN | unix.EPOLLET,
			Fd:     int32(connFd),
		}
		_ = unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_ADD, connFd, event)

		// Initialize connection state with larger buffer for POST
		s.connState[connFd] = &connState{
			buf: make([]byte, readBufSize*2), // 8KB to hold headers + body
			pos: 0,
		}
	}
}

func (s *HTTP1Server) handleRead(fd int) {
	state := s.connState[fd]
	if state == nil {
		state = &connState{
			buf: make([]byte, readBufSize*2),
			pos: 0,
		}
		s.connState[fd] = state
	}

	// Read into buffer at current position
	for {
		n, err := unix.Read(fd, state.buf[state.pos:])
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
		state.pos += n

		// Try to process complete request
		consumed := s.handleRequest(fd, state)
		if consumed > 0 {
			// Request handled, reset buffer for next request
			// For keep-alive, simply reset - next request will arrive fresh
			state.pos = 0
		}
		// If consumed == 0, we need more data - continue reading
	}
}

func (s *HTTP1Server) handleRequest(fd int, state *connState) int {
	data := state.buf[:state.pos]

	// Wait for complete HTTP request (headers end with \r\n\r\n)
	headerEnd := bytes.Index(data, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		// Incomplete request, wait for more data
		return 0
	}

	// Calculate total request length
	requestLen := headerEnd + 4 // headers + \r\n\r\n

	// For POST requests with Content-Length, ensure body is received
	if bytes.HasPrefix(data, []byte("POST")) {
		clIdx := bytes.Index(data, []byte("Content-Length: "))
		if clIdx > 0 && clIdx < headerEnd {
			clEnd := bytes.Index(data[clIdx:], []byte("\r\n"))
			if clEnd > 0 {
				var contentLen int
				_, _ = fmt.Sscanf(string(data[clIdx+16:clIdx+clEnd]), "%d", &contentLen)
				requestLen = headerEnd + 4 + contentLen
				if state.pos < requestLen {
					// Body not fully received yet
					return 0
				}
			}
		}
	}

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
	} else if bytes.HasPrefix(data, []byte("POST ")) {
		// Any other POST request - check if it's /upload with different spacing
		lineEnd := bytes.Index(data, []byte("\r\n"))
		if lineEnd > 0 && bytes.Contains(data[:lineEnd], []byte("/upload")) {
			response = responseOK
		} else {
			response = response404
		}
	} else {
		response = response404
	}

	// Write response directly
	_, _ = unix.Write(fd, response)
	return requestLen
}

func (s *HTTP1Server) closeConnection(fd int) {
	_ = unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_DEL, fd, nil)
	_ = unix.Close(fd)
	delete(s.connState, fd)
}

// ListenAddr returns the server's listen address.
func (s *HTTP1Server) ListenAddr() net.Addr {
	return nil // Not implemented for barebones server
}
