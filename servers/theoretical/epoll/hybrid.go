//go:build linux

package epoll

import (
	"bytes"
	"fmt"
	"log"
	"syscall"

	"golang.org/x/sys/unix"
)

// HybridServer is a barebones server that muxes HTTP/1.1 and H2C.
// It uses connection preface detection to route to the appropriate handler.
type HybridServer struct {
	port      string
	epollFd   int
	listenFd  int
	connState map[int]*hybridConnState
}

type hybridConnState struct {
	buf      []byte
	pos      int // Current position in buffer for accumulation
	protocol int // 0 = unknown, 1 = HTTP/1.1, 2 = H2C
	h2state  *h2ConnState
}

const (
	protoUnknown = 0
	protoHTTP1   = 1
	protoH2C     = 2
)

// NewHybridServer creates a new barebones hybrid mux server.
func NewHybridServer(port string) *HybridServer {
	return &HybridServer{
		port:      port,
		connState: make(map[int]*hybridConnState),
	}
}

// Run starts the hybrid epoll event loop.
func (s *HybridServer) Run() error {
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

	if err := unix.Listen(listenFd, 4096); err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	epollFd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return fmt.Errorf("epoll_create1: %w", err)
	}
	s.epollFd = epollFd

	event := &unix.EpollEvent{
		Events: unix.EPOLLIN | unix.EPOLLET,
		Fd:     int32(listenFd),
	}
	_ = unix.EpollCtl(epollFd, unix.EPOLL_CTL_ADD, listenFd, event)

	log.Printf("epoll-hybrid server listening on port %s", s.port)
	return s.eventLoop()
}

func (s *HybridServer) eventLoop() error {
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

func (s *HybridServer) acceptConnections() {
	for {
		connFd, _, err := unix.Accept4(s.listenFd, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				break
			}
			continue
		}

		_ = unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)

		event := &unix.EpollEvent{
			Events: unix.EPOLLIN | unix.EPOLLET,
			Fd:     int32(connFd),
		}
		_ = unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_ADD, connFd, event)

		s.connState[connFd] = &hybridConnState{
			buf:      make([]byte, 16384),
			protocol: protoUnknown,
		}
	}
}

func (s *HybridServer) handleRead(fd int) {
	state := s.connState[fd]
	if state == nil {
		s.closeConnection(fd)
		return
	}

	for {
		// Read into buffer at current position
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

		data := state.buf[:state.pos]

		// Detect protocol on first read
		if state.protocol == protoUnknown {
			if state.pos < 4 {
				continue // Need more data for minimum method/preface
			}

			if bytes.HasPrefix(data, []byte("PRI ")) {
				if state.pos < h2PrefaceLen {
					continue // Wait for full preface
				}
				if string(data[:h2PrefaceLen]) == h2Preface {
					state.protocol = protoH2C
					state.h2state = &h2ConnState{
						buf:     state.buf,
						streams: make(map[uint32]string),
					}
				} else {
					s.closeConnection(fd)
					return
				}
			} else if bytes.HasPrefix(data, []byte("GET ")) ||
				bytes.HasPrefix(data, []byte("POST ")) ||
				bytes.HasPrefix(data, []byte("PUT ")) ||
				bytes.HasPrefix(data, []byte("DELETE ")) {
				// Check for HTTP/1.1 upgrade to H2C
				if bytes.Contains(data, []byte("Upgrade: h2c")) {
					state.protocol = protoH2C
					state.h2state = &h2ConnState{
						buf:     state.buf,
						streams: make(map[uint32]string),
					}
					s.handleH2CUpgrade(fd, state, data)
					state.pos = 0
					continue
				}
				state.protocol = protoHTTP1
			} else {
				s.closeConnection(fd)
				return
			}
		}

		switch state.protocol {
		case protoHTTP1:
			// Process all complete requests in buffer
			for {
				consumed, complete := s.handleHTTP1(fd, state)
				if !complete {
					break
				}
				if consumed > 0 {
					remaining := state.pos - consumed
					if remaining > 0 {
						copy(state.buf[:], state.buf[consumed:state.pos])
						state.pos = remaining
						// Continue loop to process next request
					} else {
						state.pos = 0
						break
					}
				} else {
					break // Should not happen if complete is true, but safety
				}
			}
		case protoH2C:
			for {
				consumed, closed := s.handleH2C(fd, state.h2state, state.buf[:state.pos])

				if closed {
					s.closeConnection(fd)
					return
				}
				if consumed > 0 {
					remaining := state.pos - consumed
					if remaining > 0 {
						copy(state.buf[:], state.buf[consumed:state.pos])
						state.pos = remaining
						// Continue loop
					} else {
						state.pos = 0
						break
					}
				} else {
					break // Need more data
				}
			}
		}
	}
}

func (s *HybridServer) handleHTTP1(fd int, state *hybridConnState) (int, bool) {
	data := state.buf[:state.pos]

	// Wait for complete HTTP request (headers end with \r\n\r\n)
	headerEnd := bytes.Index(data, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		return 0, false // Incomplete request
	}

	reqLen := headerEnd + 4

	// For POST requests with Content-Length, ensure body is received
	if bytes.HasPrefix(data, []byte("POST")) {
		clIdx := bytes.Index(data, []byte("Content-Length: "))
		if clIdx > 0 && clIdx < headerEnd {
			clEnd := bytes.Index(data[clIdx:], []byte("\r\n"))
			if clEnd > 0 {
				var contentLen int
				_, _ = fmt.Sscanf(string(data[clIdx+16:clIdx+clEnd]), "%d", &contentLen)
				reqLen += contentLen
				if len(data) < reqLen {
					return 0, false // Body not fully received
				}
			}
		}
	}

	var response []byte

	if bytes.HasPrefix(data, []byte("GET / ")) {
		response = responseSimple
	} else if bytes.HasPrefix(data, []byte("GET /json")) {
		response = responseJSON
	} else if bytes.HasPrefix(data, []byte("GET /users/")) {
		response = responseSimple // Simplified
	} else if bytes.HasPrefix(data, []byte("POST /upload")) {
		response = responseOK
	} else if bytes.HasPrefix(data, []byte("POST ")) {
		// Any POST to /upload with different spacing
		lineEnd := bytes.Index(data, []byte("\r\n"))
		if lineEnd > 0 && bytes.Contains(data[:lineEnd], []byte("/upload")) {
			response = responseOK
		} else {
			response = response404
		}
	} else {
		response = response404
	}

	_, _ = unix.Write(fd, response)
	return reqLen, true
}

func (s *HybridServer) handleH2CUpgrade(fd int, state *hybridConnState, data []byte) {
	// Send 101 Switching Protocols
	upgradeResponse := []byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n")
	_, _ = unix.Write(fd, upgradeResponse)

	// Send server preface (SETTINGS)
	state.h2state.settingsSent = true
	settingsFrame := []byte{
		0x00, 0x00, 0x06, h2FrameTypeSettings, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
	}
	_, _ = unix.Write(fd, settingsFrame)
}

func (s *HybridServer) handleH2C(fd int, state *h2ConnState, data []byte) (int, bool) {
	// Reuse the HTTP2Server logic
	// Note: We create a temporary HTTP2Server struct just to access the method.
	// This is safe because HTTP2Server only uses epollFd which we provide.
	h2s := &HTTP2Server{epollFd: s.epollFd}
	return h2s.processH2Data(fd, state, data)
}

func (s *HybridServer) closeConnection(fd int) {
	_ = unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_DEL, fd, nil)
	_ = unix.Close(fd)
	delete(s.connState, fd)
}
