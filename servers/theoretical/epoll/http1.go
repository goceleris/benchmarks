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
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	maxEvents   = 4096
	maxConns    = 65536
	readBufSize = 16384
)

// HTTP/1.1 response templates (pre-formatted for zero-allocation)
var (
	responseSimple = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 13\r\nConnection: keep-alive\r\n\r\nHello, World!")
	responseJSON   = []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 47\r\nConnection: keep-alive\r\n\r\n{\"message\":\"Hello, World!\",\"server\":\"epoll-h1\"}")
	responseOK     = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\nConnection: keep-alive\r\n\r\nOK")
	response404    = []byte("HTTP/1.1 404 Not Found\r\nContent-Type: text/plain\r\nContent-Length: 9\r\nConnection: keep-alive\r\n\r\nNot Found")
	// Pre-computed path responses
	responseUser = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 13\r\nConnection: keep-alive\r\n\r\nHello, World!")
)

// HTTP1Server is a barebones HTTP/1.1 server using raw epoll.
// Uses pre-allocated arrays for maximum performance.
type HTTP1Server struct {
	port     string
	epollFd  int
	listenFd int
	// Pre-allocated connection state - indexed by fd for O(1) access
	connBuf [maxConns][]byte
	connPos [maxConns]int
}

// NewHTTP1Server creates a new barebones epoll HTTP/1.1 server.
func NewHTTP1Server(port string) *HTTP1Server {
	s := &HTTP1Server{
		port: port,
	}
	// Pre-allocate all buffers
	for i := 0; i < maxConns; i++ {
		s.connBuf[i] = make([]byte, readBufSize)
	}
	return s
}

// Run starts the epoll event loop.
func (s *HTTP1Server) Run() error {
	listenFd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	s.listenFd = listenFd

	_ = unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	_ = unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	_ = unix.SetsockoptInt(listenFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)

	var portNum int
	_, _ = fmt.Sscanf(s.port, "%d", &portNum)

	addr := &unix.SockaddrInet4{Port: portNum}
	if err := unix.Bind(listenFd, addr); err != nil {
		return fmt.Errorf("bind: %w", err)
	}

	if err := unix.Listen(listenFd, 65535); err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	epollFd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return fmt.Errorf("epoll_create1: %w", err)
	}
	s.epollFd = epollFd

	event := &unix.EpollEvent{
		Events: unix.EPOLLIN,
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
				s.acceptConnections()
			} else if events[i].Events&unix.EPOLLIN != 0 {
				s.handleRead(fd)
			} else if events[i].Events&(unix.EPOLLERR|unix.EPOLLHUP) != 0 {
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

		if connFd >= maxConns {
			_ = unix.Close(connFd)
			continue
		}

		_ = unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)

		event := &unix.EpollEvent{
			Events: unix.EPOLLIN | unix.EPOLLET,
			Fd:     int32(connFd),
		}
		_ = unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_ADD, connFd, event)

		s.connPos[connFd] = 0
	}
}

func (s *HTTP1Server) handleRead(fd int) {
	buf := s.connBuf[fd]
	pos := s.connPos[fd]

	for {
		n, err := unix.Read(fd, buf[pos:])
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
		pos += n

		// Process all complete requests (pipelining support)
		for {
			data := buf[:pos]
			consumed := s.processRequest(fd, data)
			if consumed == 0 {
				break
			}
			if consumed < pos {
				copy(buf, buf[consumed:pos])
				pos -= consumed
			} else {
				pos = 0
			}
		}
	}

	s.connPos[fd] = pos
}

//go:noinline
func (s *HTTP1Server) processRequest(fd int, data []byte) int {
	// Fast path: find end of headers
	headerEnd := bytes.Index(data, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		return 0
	}

	requestLen := headerEnd + 4

	// Check for POST with body
	if len(data) >= 4 && data[0] == 'P' && data[1] == 'O' && data[2] == 'S' && data[3] == 'T' {
		// Find Content-Length
		clIdx := bytes.Index(data[:headerEnd], []byte("Content-Length: "))
		if clIdx > 0 {
			clEnd := bytes.IndexByte(data[clIdx+16:headerEnd], '\r')
			if clEnd > 0 {
				cl := parseContentLength(data[clIdx+16 : clIdx+16+clEnd])
				requestLen += cl
				if len(data) < requestLen {
					return 0
				}
			}
		}
	}

	// Route request - ultra-fast path matching
	var response []byte

	// Check first 4 bytes for method
	if data[0] == 'G' { // GET
		if len(data) > 6 && data[4] == '/' && data[5] == ' ' {
			response = responseSimple
		} else if len(data) > 9 && data[5] == 'j' { // /json
			response = responseJSON
		} else if len(data) > 11 && data[5] == 'u' { // /users/
			response = responseUser
		} else {
			response = response404
		}
	} else if data[0] == 'P' { // POST
		response = responseOK
	} else {
		response = response404
	}

	// Direct syscall for write - no Go overhead
	rawWrite(fd, response)
	return requestLen
}

// parseContentLength parses Content-Length value without allocations
func parseContentLength(b []byte) int {
	n := 0
	for _, c := range b {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

// rawWrite uses direct syscall for minimum overhead
func rawWrite(fd int, data []byte) {
	if len(data) == 0 {
		return
	}
	_, _, _ = syscall.Syscall(syscall.SYS_WRITE,
		uintptr(fd),
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)))
}

func (s *HTTP1Server) closeConnection(fd int) {
	_ = unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_DEL, fd, nil)
	_ = unix.Close(fd)
	s.connPos[fd] = 0
}

// ListenAddr returns the server's listen address.
func (s *HTTP1Server) ListenAddr() net.Addr {
	return nil
}
