//go:build linux

package epoll

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"syscall"

	"golang.org/x/sys/unix"
)

// HTTP/2 constants
const (
	h2Preface           = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	h2PrefaceLen        = 24
	h2FrameHeaderLen    = 9
	h2FrameTypeData     = 0x0
	h2FrameTypeHeaders  = 0x1
	h2FrameTypeSettings = 0x4
	h2FlagEndStream     = 0x1
	h2FlagEndHeaders    = 0x4
	h2FlagAck           = 0x1
)

// Pre-encoded HPACK responses (minimal, no dynamic table)
var (
	// :status 200, content-type: text/plain
	hpackHeadersSimple = []byte{0x88, 0x5f, 0x0a, 0x74, 0x65, 0x78, 0x74, 0x2f, 0x70, 0x6c, 0x61, 0x69, 0x6e}
	hpackHeadersJSON   = []byte{0x88, 0x5f, 0x10, 0x61, 0x70, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x2f, 0x6a, 0x73, 0x6f, 0x6e}

	bodySimple = []byte("Hello, World!")
	bodyJSON   = []byte(`{"message":"Hello, World!","server":"epoll-h2"}`)
	bodyOK     = []byte("OK")
)

// HTTP2Server is a barebones H2C server using raw epoll.
// This implements HTTP/2 prior knowledge only (no upgrade).
type HTTP2Server struct {
	port      string
	epollFd   int
	listenFd  int
	connState map[int]*h2ConnState
}

type h2ConnState struct {
	buf          []byte
	prefaceRecv  bool
	settingsSent bool
}

// NewHTTP2Server creates a new barebones epoll H2C server.
func NewHTTP2Server(port string) *HTTP2Server {
	return &HTTP2Server{
		port:      port,
		connState: make(map[int]*h2ConnState),
	}
}

// Run starts the epoll event loop for H2C.
func (s *HTTP2Server) Run() error {
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
	_ = unix.EpollCtl(epollFd, unix.EPOLL_CTL_ADD, listenFd, event)

	log.Printf("epoll-h2 server listening on port %s", s.port)
	return s.eventLoop()
}

func (s *HTTP2Server) eventLoop() error {
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

func (s *HTTP2Server) acceptConnections() {
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
		_ = unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_ADD, connFd, event)

		s.connState[connFd] = &h2ConnState{
			buf: make([]byte, 16384),
		}
	}
}

func (s *HTTP2Server) handleRead(fd int) {
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

		s.processH2Data(fd, state, state.buf[:n])
	}
}

func (s *HTTP2Server) processH2Data(fd int, state *h2ConnState, data []byte) {
	offset := 0

	// Check for HTTP/2 connection preface
	if !state.prefaceRecv {
		if len(data) >= h2PrefaceLen && string(data[:h2PrefaceLen]) == h2Preface {
			state.prefaceRecv = true
			offset = h2PrefaceLen

			// Send server settings
			if !state.settingsSent {
				s.sendSettings(fd)
				state.settingsSent = true
			}
		} else {
			s.closeConnection(fd)
			return
		}
	}

	// Process frames
	for offset+h2FrameHeaderLen <= len(data) {
		// Parse frame header
		length := int(data[offset])<<16 | int(data[offset+1])<<8 | int(data[offset+2])
		frameType := data[offset+3]
		flags := data[offset+4]
		streamID := binary.BigEndian.Uint32(data[offset+5:offset+9]) & 0x7fffffff

		offset += h2FrameHeaderLen

		if offset+length > len(data) {
			break // Incomplete frame
		}

		frameData := data[offset : offset+length]
		offset += length

		switch frameType {
		case h2FrameTypeSettings:
			if flags&h2FlagAck == 0 {
				// Send settings ACK
				s.sendSettingsAck(fd)
			}
		case h2FrameTypeHeaders:
			// Parse minimal headers and send response
			s.handleH2Request(fd, streamID, frameData, flags)
		}
	}
}

func (s *HTTP2Server) sendSettings(fd int) {
	// Empty SETTINGS frame
	frame := make([]byte, 9)
	frame[3] = h2FrameTypeSettings
	_, _ = unix.Write(fd, frame)
}

func (s *HTTP2Server) sendSettingsAck(fd int) {
	frame := make([]byte, 9)
	frame[3] = h2FrameTypeSettings
	frame[4] = h2FlagAck
	_, _ = unix.Write(fd, frame)
}

func (s *HTTP2Server) handleH2Request(fd int, streamID uint32, headerBlock []byte, flags byte) {
	// Very simplified path extraction from HPACK
	// In a real implementation, we'd decode HPACK properly
	var path string

	// Look for common encoded paths
	if bytes.Contains(headerBlock, []byte("/json")) {
		path = "/json"
	} else if bytes.Contains(headerBlock, []byte("/users/")) {
		path = "/users/123" // Simplified
	} else if bytes.Contains(headerBlock, []byte("/upload")) {
		path = "/upload"
	} else {
		path = "/"
	}

	// Send response
	var headerBytes, bodyBytes []byte

	switch path {
	case "/":
		headerBytes = hpackHeadersSimple
		bodyBytes = bodySimple
	case "/json":
		headerBytes = hpackHeadersJSON
		bodyBytes = bodyJSON
	default:
		headerBytes = hpackHeadersSimple
		bodyBytes = bodyOK
	}

	// HEADERS frame
	headersFrame := make([]byte, 9+len(headerBytes))
	headersFrame[0] = byte(len(headerBytes) >> 16)
	headersFrame[1] = byte(len(headerBytes) >> 8)
	headersFrame[2] = byte(len(headerBytes))
	headersFrame[3] = h2FrameTypeHeaders
	headersFrame[4] = h2FlagEndHeaders
	binary.BigEndian.PutUint32(headersFrame[5:9], streamID)
	copy(headersFrame[9:], headerBytes)
	unix.Write(fd, headersFrame)

	// DATA frame
	dataFrame := make([]byte, 9+len(bodyBytes))
	dataFrame[0] = byte(len(bodyBytes) >> 16)
	dataFrame[1] = byte(len(bodyBytes) >> 8)
	dataFrame[2] = byte(len(bodyBytes))
	dataFrame[3] = h2FrameTypeData
	dataFrame[4] = h2FlagEndStream
	binary.BigEndian.PutUint32(dataFrame[5:9], streamID)
	copy(dataFrame[9:], bodyBytes)
	unix.Write(fd, dataFrame)
}

func (s *HTTP2Server) closeConnection(fd int) {
	unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_DEL, fd, nil)
	unix.Close(fd)
	delete(s.connState, fd)
}
