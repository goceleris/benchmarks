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

// Pre-encoded HPACK responses
var (
	hpackHeadersSimple = []byte{0x88, 0x5f, 0x0a, 0x74, 0x65, 0x78, 0x74, 0x2f, 0x70, 0x6c, 0x61, 0x69, 0x6e}
	hpackHeadersJSON   = []byte{0x88, 0x5f, 0x10, 0x61, 0x70, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x2f, 0x6a, 0x73, 0x6f, 0x6e}

	bodySimple = []byte("Hello, World!")
	bodyJSON   = []byte(`{"message":"Hello, World!","server":"epoll-h2"}`)
	bodyOK     = []byte("OK")

	// Pre-built settings frame
	frameSettings = []byte{
		0x00, 0x00, 0x0C,
		h2FrameTypeSettings,
		0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x04, 0x00, 0x10, 0x00, 0x00,
	}
	frameSettingsAck = []byte{0x00, 0x00, 0x00, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00}
)

// h2ConnState tracks HTTP/2 connection state
type h2ConnState struct {
	buf          []byte
	pos          int
	prefaceRecv  bool
	settingsSent bool
	streams      map[uint32]string
}

// HTTP2Server is a barebones H2C server using raw epoll.
type HTTP2Server struct {
	port     string
	epollFd  int
	listenFd int
	// Pre-allocated buffers indexed by fd
	connBuf  [maxConns][]byte
	connPos  [maxConns]int
	connH2   [maxConns]*h2ConnState
}

// NewHTTP2Server creates a new barebones epoll H2C server.
func NewHTTP2Server(port string) *HTTP2Server {
	s := &HTTP2Server{
		port: port,
	}
	for i := 0; i < maxConns; i++ {
		s.connBuf[i] = make([]byte, readBufSize)
	}
	return s
}

// Run starts the epoll event loop for H2C.
func (s *HTTP2Server) Run() error {
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
		s.connH2[connFd] = &h2ConnState{
			buf:     s.connBuf[connFd],
			streams: make(map[uint32]string),
		}
	}
}

func (s *HTTP2Server) handleRead(fd int) {
	state := s.connH2[fd]
	if state == nil {
		s.closeConnection(fd)
		return
	}

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
		state.pos = pos

		// Process H2 data
		for {
			consumed, closed := s.processH2Data(fd, state, buf[:pos])
			if closed {
				return
			}
			if consumed > 0 {
				if consumed < pos {
					copy(buf, buf[consumed:pos])
					pos -= consumed
				} else {
					pos = 0
				}
				state.pos = pos
			} else {
				break
			}
		}
	}

	s.connPos[fd] = pos
}

func (s *HTTP2Server) processH2Data(fd int, state *h2ConnState, data []byte) (int, bool) {
	offset := 0

	if !state.prefaceRecv {
		if len(data) >= h2PrefaceLen {
			if string(data[:h2PrefaceLen]) == h2Preface {
				state.prefaceRecv = true
				offset = h2PrefaceLen

				if !state.settingsSent {
					rawWrite(fd, frameSettings)
					state.settingsSent = true
				}
				return offset, false
			}
			s.closeConnection(fd)
			return 0, true
		}
		return 0, false
	}

	for offset+h2FrameHeaderLen <= len(data) {
		length := int(data[offset])<<16 | int(data[offset+1])<<8 | int(data[offset+2])
		frameType := data[offset+3]
		flags := data[offset+4]
		streamID := binary.BigEndian.Uint32(data[offset+5:offset+9]) & 0x7fffffff

		totalLen := h2FrameHeaderLen + length
		if offset+totalLen > len(data) {
			break
		}

		frameData := data[offset+h2FrameHeaderLen : offset+totalLen]

		switch frameType {
		case h2FrameTypeSettings:
			if flags&h2FlagAck == 0 {
				rawWrite(fd, frameSettingsAck)
			}
		case h2FrameTypeHeaders:
			endStream := flags&h2FlagEndStream != 0
			s.handleH2Request(fd, streamID, frameData, endStream, state)

		case h2FrameTypeData:
			if length > 0 {
				s.sendWindowUpdate(fd, 0, uint32(length))
				s.sendWindowUpdate(fd, streamID, uint32(length))
			}

			endStream := flags&h2FlagEndStream != 0
			if endStream {
				if path, ok := state.streams[streamID]; ok {
					s.sendH2Response(fd, streamID, path)
					delete(state.streams, streamID)
				}
			}
		}

		offset += totalLen
	}

	return offset, false
}

func (s *HTTP2Server) handleH2Request(fd int, streamID uint32, headerBlock []byte, endStream bool, state *h2ConnState) {
	var path string

	// Fast path detection
	if bytes.Contains(headerBlock, []byte("/json")) {
		path = "/json"
	} else if bytes.Contains(headerBlock, []byte("/users/")) {
		path = "/users/123"
	} else if bytes.Contains(headerBlock, []byte("/upload")) {
		path = "/upload"
	} else {
		path = "/"
	}

	if endStream {
		s.sendH2Response(fd, streamID, path)
	} else {
		state.streams[streamID] = path
		if path == "/upload" {
			s.send100Continue(fd, streamID)
		}
	}
}

func (s *HTTP2Server) send100Continue(fd int, streamID uint32) {
	h100 := []byte{0x08, 0x03, 0x31, 0x30, 0x30}
	frame := make([]byte, 9+len(h100))
	frame[2] = byte(len(h100))
	frame[3] = h2FrameTypeHeaders
	frame[4] = h2FlagEndHeaders
	binary.BigEndian.PutUint32(frame[5:9], streamID)
	copy(frame[9:], h100)
	rawWrite(fd, frame)
}

func (s *HTTP2Server) sendWindowUpdate(fd int, streamID uint32, increment uint32) {
	frame := make([]byte, 13)
	frame[2] = 0x04
	frame[3] = 0x08
	binary.BigEndian.PutUint32(frame[5:9], streamID)
	binary.BigEndian.PutUint32(frame[9:13], increment)
	rawWrite(fd, frame)
}

func (s *HTTP2Server) sendH2Response(fd int, streamID uint32, path string) {
	var headerBytes, bodyBytes []byte

	switch path {
	case "/json":
		headerBytes = hpackHeadersJSON
		bodyBytes = bodyJSON
	case "/upload":
		headerBytes = hpackHeadersSimple
		bodyBytes = bodyOK
	default:
		headerBytes = hpackHeadersSimple
		bodyBytes = bodySimple
	}

	// Coalesce headers + data into single write
	totalLen := 9 + len(headerBytes) + 9 + len(bodyBytes)
	frame := make([]byte, totalLen)

	// Headers frame
	frame[0] = byte(len(headerBytes) >> 16)
	frame[1] = byte(len(headerBytes) >> 8)
	frame[2] = byte(len(headerBytes))
	frame[3] = h2FrameTypeHeaders
	frame[4] = h2FlagEndHeaders
	binary.BigEndian.PutUint32(frame[5:9], streamID)
	copy(frame[9:], headerBytes)

	// Data frame
	off := 9 + len(headerBytes)
	frame[off] = byte(len(bodyBytes) >> 16)
	frame[off+1] = byte(len(bodyBytes) >> 8)
	frame[off+2] = byte(len(bodyBytes))
	frame[off+3] = h2FrameTypeData
	frame[off+4] = h2FlagEndStream
	binary.BigEndian.PutUint32(frame[off+5:off+9], streamID)
	copy(frame[off+9:], bodyBytes)

	rawWrite(fd, frame)
}

func (s *HTTP2Server) closeConnection(fd int) {
	_ = unix.EpollCtl(s.epollFd, unix.EPOLL_CTL_DEL, fd, nil)
	_ = unix.Close(fd)
	s.connPos[fd] = 0
	s.connH2[fd] = nil
}
