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

	unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	unix.SetsockoptInt(listenFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)

	var portNum int
	fmt.Sscanf(s.port, "%d", &portNum)

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
	unix.EpollCtl(epollFd, unix.EPOLL_CTL_ADD, listenFd, event)

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

		unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)

		event := &unix.EpollEvent{
			Events: unix.EPOLLIN | unix.EPOLLET,
			Fd:     int32(connFd),
		}
		unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_ADD, connFd, event)

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
		n, err := unix.Read(fd, state.buf)
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

		data := state.buf[:n]

		// Detect protocol on first read
		if state.protocol == protoUnknown {
			if n >= h2PrefaceLen && string(data[:h2PrefaceLen]) == h2Preface {
				state.protocol = protoH2C
				state.h2state = &h2ConnState{buf: state.buf}
			} else if bytes.HasPrefix(data, []byte("GET ")) ||
				bytes.HasPrefix(data, []byte("POST ")) ||
				bytes.HasPrefix(data, []byte("PUT ")) ||
				bytes.HasPrefix(data, []byte("DELETE ")) {
				// Check for HTTP/1.1 upgrade to H2C
				if bytes.Contains(data, []byte("Upgrade: h2c")) {
					state.protocol = protoH2C
					state.h2state = &h2ConnState{buf: state.buf}
					s.handleH2CUpgrade(fd, state, data)
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
			s.handleHTTP1(fd, data)
		case protoH2C:
			s.handleH2C(fd, state.h2state, data)
		}
	}
}

func (s *HybridServer) handleHTTP1(fd int, data []byte) {
	var response []byte

	if bytes.HasPrefix(data, []byte("GET / ")) {
		response = responseSimple
	} else if bytes.HasPrefix(data, []byte("GET /json")) {
		response = responseJSON
	} else if bytes.HasPrefix(data, []byte("GET /users/")) {
		response = responseSimple // Simplified
	} else if bytes.HasPrefix(data, []byte("POST /upload")) {
		response = responseOK
	} else {
		response = response404
	}

	unix.Write(fd, response)
}

func (s *HybridServer) handleH2CUpgrade(fd int, state *hybridConnState, data []byte) {
	// Send 101 Switching Protocols
	upgradeResponse := []byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n")
	unix.Write(fd, upgradeResponse)

	// Send server preface (SETTINGS)
	state.h2state.settingsSent = true
	settingsFrame := make([]byte, 9)
	settingsFrame[3] = h2FrameTypeSettings
	unix.Write(fd, settingsFrame)
}

func (s *HybridServer) handleH2C(fd int, state *h2ConnState, data []byte) {
	// Reuse the HTTP2Server logic
	h2s := &HTTP2Server{epollFd: s.epollFd}
	h2s.processH2Data(fd, state, data)
}

func (s *HybridServer) closeConnection(fd int) {
	unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_DEL, fd, nil)
	unix.Close(fd)
	delete(s.connState, fd)
}
