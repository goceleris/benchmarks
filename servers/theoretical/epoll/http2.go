//go:build linux

package epoll

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"runtime"
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

	frameSettings = []byte{
		0x00, 0x00, 0x0C,
		h2FrameTypeSettings,
		0x00,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x01, 0x00, 0x00, 0x10, 0x00, // MAX_CONCURRENT_STREAMS = 4096
		0x00, 0x04, 0x00, 0x60, 0x00, 0x00, // INITIAL_WINDOW_SIZE = 6MB
	}
	frameSettingsAck = []byte{0x00, 0x00, 0x00, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00}
)

// h2ConnState tracks HTTP/2 connection state
type h2ConnState struct {
	prefaceRecv  bool
	settingsSent bool
	streams      map[uint32]string
}

// h2Worker represents a single HTTP/2 event loop worker
type h2Worker struct {
	id       int
	port     int
	epollFd  int
	listenFd int
	connBuf  [][]byte
	connPos  []int
	connH2   []*h2ConnState
}

// HTTP2Server is a multi-threaded H2C server using raw epoll.
type HTTP2Server struct {
	port       string
	numWorkers int
	workers    []*h2Worker
}

// NewHTTP2Server creates a new multi-threaded epoll H2C server.
func NewHTTP2Server(port string) *HTTP2Server {
	numWorkers := runtime.NumCPU()
	if numWorkers < 1 {
		numWorkers = 1
	}
	return &HTTP2Server{
		port:       port,
		numWorkers: numWorkers,
		workers:    make([]*h2Worker, numWorkers),
	}
}

// Run starts multiple epoll event loops for H2C.
func (s *HTTP2Server) Run() error {
	var portNum int
	_, _ = fmt.Sscanf(s.port, "%d", &portNum)

	errCh := make(chan error, s.numWorkers)

	for i := 0; i < s.numWorkers; i++ {
		w := &h2Worker{
			id:      i,
			port:    portNum,
			connBuf: make([][]byte, maxConns),
			connPos: make([]int, maxConns),
			connH2:  make([]*h2ConnState, maxConns),
		}
		for j := 0; j < maxConns; j++ {
			w.connBuf[j] = make([]byte, readBufSize)
		}
		s.workers[i] = w

		go func(worker *h2Worker) {
			runtime.LockOSThread()
			if err := worker.run(); err != nil {
				errCh <- err
			}
		}(w)
	}

	log.Printf("epoll-h2 server listening on port %s with %d workers", s.port, s.numWorkers)
	return <-errCh
}

func (w *h2Worker) run() error {
	listenFd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	w.listenFd = listenFd

	_ = unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	_ = unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	_ = unix.SetsockoptInt(listenFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
	_ = unix.SetsockoptInt(listenFd, unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1)
	_ = unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_RCVBUF, 65536)
	_ = unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_SNDBUF, 65536)

	addr := &unix.SockaddrInet4{Port: w.port}
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
	w.epollFd = epollFd

	event := &unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(listenFd),
	}
	_ = unix.EpollCtl(epollFd, unix.EPOLL_CTL_ADD, listenFd, event)

	return w.eventLoop()
}

func (w *h2Worker) eventLoop() error {
	events := make([]unix.EpollEvent, maxEvents)

	for {
		n, err := unix.EpollWait(w.epollFd, events, -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return fmt.Errorf("epoll_wait: %w", err)
		}

		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)

			if fd == w.listenFd {
				w.acceptConnections()
			} else if events[i].Events&unix.EPOLLIN != 0 {
				w.handleRead(fd)
			} else if events[i].Events&(unix.EPOLLERR|unix.EPOLLHUP) != 0 {
				w.closeConnection(fd)
			}
		}
	}
}

func (w *h2Worker) acceptConnections() {
	for {
		connFd, _, err := unix.Accept4(w.listenFd, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
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
		_ = unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1)

		event := &unix.EpollEvent{
			Events: unix.EPOLLIN | unix.EPOLLET,
			Fd:     int32(connFd),
		}
		_ = unix.EpollCtl(w.epollFd, unix.EPOLL_CTL_ADD, connFd, event)

		w.connPos[connFd] = 0
		w.connH2[connFd] = &h2ConnState{
			streams: make(map[uint32]string),
		}
	}
}

func (w *h2Worker) handleRead(fd int) {
	state := w.connH2[fd]
	if state == nil {
		w.closeConnection(fd)
		return
	}

	buf := w.connBuf[fd]
	pos := w.connPos[fd]

	for {
		n, err := unix.Read(fd, buf[pos:])
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				break
			}
			w.closeConnection(fd)
			return
		}
		if n == 0 {
			w.closeConnection(fd)
			return
		}
		pos += n

		for {
			consumed, closed := w.processH2Data(fd, state, buf[:pos])
			if closed {
				return
			}
			if consumed > 0 {
				if consumed < pos {
					copy(buf, buf[consumed:pos])
					pos -= consumed
				} else {
					pos = 0
					break
				}
			} else {
				break
			}
		}
	}

	w.connPos[fd] = pos
}

func (w *h2Worker) processH2Data(fd int, state *h2ConnState, data []byte) (int, bool) {
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
			w.closeConnection(fd)
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
			w.handleH2Request(fd, streamID, frameData, endStream, state)

		case h2FrameTypeData:
			if length > 0 {
				w.sendWindowUpdates(fd, streamID, uint32(length))
			}

			endStream := flags&h2FlagEndStream != 0
			if endStream {
				if path, ok := state.streams[streamID]; ok {
					w.sendH2Response(fd, streamID, path)
					delete(state.streams, streamID)
				}
			}
		}

		offset += totalLen
	}

	return offset, false
}

func (w *h2Worker) handleH2Request(fd int, streamID uint32, headerBlock []byte, endStream bool, state *h2ConnState) {
	var path string

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
		w.sendH2Response(fd, streamID, path)
	} else {
		state.streams[streamID] = path
		if path == "/upload" {
			w.send100Continue(fd, streamID)
		}
	}
}

func (w *h2Worker) send100Continue(fd int, streamID uint32) {
	h100 := []byte{0x08, 0x03, 0x31, 0x30, 0x30}
	frame := make([]byte, 9+len(h100))
	frame[2] = byte(len(h100))
	frame[3] = h2FrameTypeHeaders
	frame[4] = h2FlagEndHeaders
	binary.BigEndian.PutUint32(frame[5:9], streamID)
	copy(frame[9:], h100)
	rawWrite(fd, frame)
}

func (w *h2Worker) sendWindowUpdates(fd int, streamID uint32, increment uint32) {
	frame := make([]byte, 26)
	frame[2] = 0x04
	frame[3] = 0x08
	binary.BigEndian.PutUint32(frame[9:13], increment)
	frame[15] = 0x04
	frame[16] = 0x08
	binary.BigEndian.PutUint32(frame[18:22], streamID)
	binary.BigEndian.PutUint32(frame[22:26], increment)
	rawWrite(fd, frame)
}

func (w *h2Worker) sendH2Response(fd int, streamID uint32, path string) {
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

	totalLen := 9 + len(headerBytes) + 9 + len(bodyBytes)
	frame := make([]byte, totalLen)

	frame[0] = byte(len(headerBytes) >> 16)
	frame[1] = byte(len(headerBytes) >> 8)
	frame[2] = byte(len(headerBytes))
	frame[3] = h2FrameTypeHeaders
	frame[4] = h2FlagEndHeaders
	binary.BigEndian.PutUint32(frame[5:9], streamID)
	copy(frame[9:], headerBytes)

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

func (w *h2Worker) closeConnection(fd int) {
	_ = unix.EpollCtl(w.epollFd, unix.EPOLL_CTL_DEL, fd, nil)
	_ = unix.Close(fd)
	w.connPos[fd] = 0
	w.connH2[fd] = nil
}
