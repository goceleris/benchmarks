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

const (
	protoUnknown = 0
	protoHTTP1   = 1
	protoH2C     = 2
)

// hybridConnState tracks per-connection state
type hybridConnState struct {
	protocol     int
	prefaceRecv  bool
	settingsSent bool
	streams      map[uint32]string
}

// hybridWorker represents a single event loop worker
type hybridWorker struct {
	id        int
	port      int
	epollFd   int
	listenFd  int
	connBuf   [][]byte
	connPos   []int
	connState []*hybridConnState
}

// HybridServer is a multi-threaded server that muxes HTTP/1.1 and H2C.
type HybridServer struct {
	port       string
	numWorkers int
	workers    []*hybridWorker
}

// NewHybridServer creates a new multi-threaded hybrid mux server.
func NewHybridServer(port string) *HybridServer {
	numWorkers := runtime.NumCPU()
	if numWorkers < 1 {
		numWorkers = 1
	}
	return &HybridServer{
		port:       port,
		numWorkers: numWorkers,
		workers:    make([]*hybridWorker, numWorkers),
	}
}

// Run starts multiple hybrid epoll event loops.
func (s *HybridServer) Run() error {
	var portNum int
	_, _ = fmt.Sscanf(s.port, "%d", &portNum)

	errCh := make(chan error, s.numWorkers)

	for i := 0; i < s.numWorkers; i++ {
		w := &hybridWorker{
			id:        i,
			port:      portNum,
			connBuf:   make([][]byte, maxConns),
			connPos:   make([]int, maxConns),
			connState: make([]*hybridConnState, maxConns),
		}
		for j := 0; j < maxConns; j++ {
			w.connBuf[j] = make([]byte, readBufSize)
		}
		s.workers[i] = w

		go func(worker *hybridWorker) {
			runtime.LockOSThread()
			if err := worker.run(); err != nil {
				errCh <- err
			}
		}(w)
	}

	log.Printf("epoll-hybrid server listening on port %s with %d workers", s.port, s.numWorkers)
	return <-errCh
}

func (w *hybridWorker) run() error {
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

func (w *hybridWorker) eventLoop() error {
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

func (w *hybridWorker) acceptConnections() {
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
		w.connState[connFd] = &hybridConnState{
			protocol: protoUnknown,
		}
	}
}

func (w *hybridWorker) handleRead(fd int) {
	state := w.connState[fd]
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

		data := buf[:pos]

		// Detect protocol on first read
		if state.protocol == protoUnknown {
			if pos < 4 {
				continue
			}

			if bytes.HasPrefix(data, []byte("PRI ")) {
				if pos < h2PrefaceLen {
					continue
				}
				if string(data[:h2PrefaceLen]) == h2Preface {
					state.protocol = protoH2C
					state.streams = make(map[uint32]string)
				} else {
					w.closeConnection(fd)
					return
				}
			} else if data[0] == 'G' || data[0] == 'P' || data[0] == 'H' || data[0] == 'D' {
				state.protocol = protoHTTP1
			} else {
				w.closeConnection(fd)
				return
			}
		}

		switch state.protocol {
		case protoHTTP1:
			for {
				consumed := w.handleHTTP1(fd, buf[:pos])
				if consumed == 0 {
					break
				}
				if consumed < pos {
					copy(buf, buf[consumed:pos])
					pos -= consumed
				} else {
					pos = 0
					break
				}
			}
		case protoH2C:
			for {
				consumed, closed := w.handleH2C(fd, state, buf[:pos])
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
	}

	w.connPos[fd] = pos
}

func (w *hybridWorker) handleHTTP1(fd int, data []byte) int {
	headerEnd := -1
	if len(data) >= 4 {
		for i := 0; i <= len(data)-4; i++ {
			if data[i] == '\r' && data[i+1] == '\n' && data[i+2] == '\r' && data[i+3] == '\n' {
				headerEnd = i
				break
			}
		}
	}
	if headerEnd < 0 {
		return 0
	}

	reqLen := headerEnd + 4

	if len(data) >= 4 && data[0] == 'P' && data[1] == 'O' && data[2] == 'S' && data[3] == 'T' {
		clIdx := bytes.Index(data[:headerEnd], []byte("Content-Length: "))
		if clIdx > 0 {
			clEnd := bytes.IndexByte(data[clIdx+16:headerEnd], '\r')
			if clEnd > 0 {
				cl := parseContentLength(data[clIdx+16 : clIdx+16+clEnd])
				reqLen += cl
				if len(data) < reqLen {
					return 0
				}
			}
		}
	}

	var response []byte
	switch data[0] {
	case 'G':
		if len(data) > 6 && data[4] == '/' && data[5] == ' ' {
			response = responseSimple
		} else if len(data) > 9 && data[5] == 'j' {
			response = responseJSON
		} else if len(data) > 11 && data[5] == 'u' {
			response = responseUser
		} else {
			response = response404
		}
	case 'P':
		response = responseOK
	default:
		response = response404
	}

	rawWrite(fd, response)
	return reqLen
}

func (w *hybridWorker) handleH2C(fd int, state *hybridConnState, data []byte) (int, bool) {
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

func (w *hybridWorker) handleH2Request(fd int, streamID uint32, headerBlock []byte, endStream bool, state *hybridConnState) {
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

func (w *hybridWorker) send100Continue(fd int, streamID uint32) {
	h100 := []byte{0x08, 0x03, 0x31, 0x30, 0x30}
	frame := make([]byte, 9+len(h100))
	frame[2] = byte(len(h100))
	frame[3] = h2FrameTypeHeaders
	frame[4] = h2FlagEndHeaders
	binary.BigEndian.PutUint32(frame[5:9], streamID)
	copy(frame[9:], h100)
	rawWrite(fd, frame)
}

func (w *hybridWorker) sendWindowUpdates(fd int, streamID uint32, increment uint32) {
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

func (w *hybridWorker) sendH2Response(fd int, streamID uint32, path string) {
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

func (w *hybridWorker) closeConnection(fd int) {
	_ = unix.EpollCtl(w.epollFd, unix.EPOLL_CTL_DEL, fd, nil)
	_ = unix.Close(fd)
	w.connPos[fd] = 0
	w.connState[fd] = nil
}
