//go:build linux

package iouring

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// HybridServer is a barebones server that muxes HTTP/1.1 and H2C using io_uring.
type HybridServer struct {
	port          string
	ringFd        int
	listenFd      int
	sqHead        *uint32
	sqTail        *uint32
	cqHead        *uint32
	cqTail        *uint32
	sqMask        uint32
	cqMask        uint32
	sqeTail       uint32
	submittedTail uint32
	sqes          []IoUringSqe
	cqes          []IoUringCqe
	sqArray       []uint32
	// Pre-allocated buffers indexed by fd for O(1) access
	buffers   [bufferCount][]byte
	connPos   [bufferCount]int
	connState [bufferCount]*hybridioConnState
}

type hybridioConnState struct {
	protocol     int // 0=unknown, 1=HTTP/1.1, 2=H2C
	prefaceRecv  bool
	settingsSent bool
	streams      map[uint32]string
}

const (
	protoUnknownIO = 0
	protoHTTP1IO   = 1
	protoH2CIO     = 2
)

// NewHybridServer creates a new barebones io_uring hybrid mux server.
func NewHybridServer(port string) *HybridServer {
	s := &HybridServer{
		port: port,
	}
	// Pre-allocate all buffers
	for i := 0; i < bufferCount; i++ {
		s.buffers[i] = make([]byte, bufferSize)
	}
	return s
}

// Run starts the hybrid io_uring event loop.
func (s *HybridServer) Run() error {
	listenFd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
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

	if err := s.setupRing(); err != nil {
		return fmt.Errorf("setup ring: %w", err)
	}

	s.submitAccept()

	return s.eventLoop()
}

func (s *HybridServer) setupRing() error {
	params := &IoUringParams{
		Flags: 0,
	}

	ringFd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, sqeCount, uintptr(unsafe.Pointer(params)), 0)
	if errno != 0 {
		return fmt.Errorf("io_uring_setup: %w", errno)
	}
	s.ringFd = int(ringFd)

	sqSize := params.SqOff.Array + params.SqEntries*4
	cqSize := params.CqOff.Cqes + params.CqEntries*uint32(unsafe.Sizeof(IoUringCqe{}))

	sqPtr, err := unix.Mmap(s.ringFd, 0, int(sqSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap sq: %w", err)
	}

	cqPtr, err := unix.Mmap(s.ringFd, 0x8000000, int(cqSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap cq: %w", err)
	}

	sqeSize := params.SqEntries * uint32(unsafe.Sizeof(IoUringSqe{}))
	sqePtr, err := unix.Mmap(s.ringFd, 0x10000000, int(sqeSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap sqes: %w", err)
	}

	s.sqHead = (*uint32)(unsafe.Pointer(&sqPtr[params.SqOff.Head]))
	s.sqTail = (*uint32)(unsafe.Pointer(&sqPtr[params.SqOff.Tail]))
	s.sqMask = *((*uint32)(unsafe.Pointer(&sqPtr[params.SqOff.RingMask])))
	s.sqArray = unsafe.Slice((*uint32)(unsafe.Pointer(&sqPtr[params.SqOff.Array])), params.SqEntries)

	s.cqHead = (*uint32)(unsafe.Pointer(&cqPtr[params.CqOff.Head]))
	s.cqTail = (*uint32)(unsafe.Pointer(&cqPtr[params.CqOff.Tail]))
	s.cqMask = *((*uint32)(unsafe.Pointer(&cqPtr[params.CqOff.RingMask])))
	s.cqes = unsafe.Slice((*IoUringCqe)(unsafe.Pointer(&cqPtr[params.CqOff.Cqes])), params.CqEntries)

	s.sqes = unsafe.Slice((*IoUringSqe)(unsafe.Pointer(&sqePtr[0])), params.SqEntries)

	s.sqeTail = atomic.LoadUint32(s.sqTail)
	s.submittedTail = s.sqeTail

	return nil
}

func (s *HybridServer) submitAccept() {
	head := atomic.LoadUint32(s.sqHead)
	next := s.sqeTail + 1

	if next-head > uint32(sqeCount) {
		return
	}

	idx := s.sqeTail & s.sqMask
	sqe := &s.sqes[idx]
	s.sqeTail = next

	*sqe = IoUringSqe{}
	sqe.Opcode = IORING_OP_ACCEPT
	sqe.Fd = int32(s.listenFd)
	sqe.UserData = uint64(s.listenFd)

	tail := s.sqeTail
	sqTail := atomic.LoadUint32(s.sqTail)
	for i := sqTail; i < tail; i++ {
		s.sqArray[i&s.sqMask] = i & s.sqMask
	}
	atomic.StoreUint32(s.sqTail, tail)

	// Immediate submit pattern (like http1.go)
	_, _, _ = unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(s.ringFd), 1, 0, 0, 0, 0)
}

func (s *HybridServer) submitRecv(fd int) {
	pos := s.connPos[fd]

	head := atomic.LoadUint32(s.sqHead)
	next := s.sqeTail + 1

	if next-head > uint32(sqeCount) {
		return
	}

	idx := s.sqeTail & s.sqMask
	sqe := &s.sqes[idx]
	s.sqeTail = next

	*sqe = IoUringSqe{}
	sqe.Opcode = IORING_OP_RECV
	sqe.Fd = int32(fd)
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&s.buffers[fd][pos])))
	sqe.Len = uint32(bufferSize - pos)
	sqe.UserData = uint64(fd)

	tail := s.sqeTail
	sqTail := atomic.LoadUint32(s.sqTail)
	for i := sqTail; i < tail; i++ {
		s.sqArray[i&s.sqMask] = i & s.sqMask
	}
	atomic.StoreUint32(s.sqTail, tail)
}

func (s *HybridServer) submitSend(fd int, data []byte) {
	// Use direct syscall for minimal latency on responses
	rawWriteIO(fd, data)
}

func (s *HybridServer) eventLoop() error {
	for {
		// Process all available CQEs
		for {
			head := atomic.LoadUint32(s.cqHead)
			tail := atomic.LoadUint32(s.cqTail)

			if head == tail {
				break
			}

			cqe := &s.cqes[head&s.cqMask]
			atomic.StoreUint32(s.cqHead, head+1)

			fd := int(cqe.UserData & 0xFFFFFFFF)
			isSend := (cqe.UserData>>32)&1 == 1

			if fd == s.listenFd && !isSend {
				if cqe.Res >= 0 {
					connFd := int(cqe.Res)
					if connFd < bufferCount {
						_ = unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
						s.connPos[connFd] = 0
						s.connState[connFd] = &hybridioConnState{
							protocol: protoUnknownIO,
							streams:  make(map[uint32]string),
						}
						s.submitRecv(connFd)
					} else {
						_ = unix.Close(connFd)
					}
					s.submitAccept()
				}
			} else if !isSend {
				if cqe.Res > 0 {
					s.handleData(fd, int(cqe.Res))
				} else {
					s.closeConnection(fd)
				}
			} else {
				// Send completion - if error, close
				if cqe.Res < 0 {
					s.closeConnection(fd)
				}
			}
		}

		// Batch submit pending
		tail := atomic.LoadUint32(s.sqTail)
		toSubmit := tail - s.submittedTail
		s.submittedTail = tail

		_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(s.ringFd),
			uintptr(toSubmit), 1, IORING_ENTER_GETEVENTS, 0, 0)
		if errno != 0 && errno != unix.EINTR {
			return fmt.Errorf("io_uring_enter: %w", errno)
		}
	}
}

func (s *HybridServer) handleData(fd int, n int) {
	state := s.connState[fd]
	if state == nil {
		s.closeConnection(fd)
		return
	}

	s.connPos[fd] += n
	buf := s.buffers[fd]
	pos := s.connPos[fd]

	// Process all complete requests
	for {
		data := buf[:pos]

		// Detect protocol on first data
		if state.protocol == protoUnknownIO {
			if pos < 4 {
				break
			}
			if bytes.HasPrefix(data, []byte("PRI ")) {
				if pos < h2PrefaceLen {
					break
				}
				if string(data[:h2PrefaceLen]) == h2Preface {
					state.protocol = protoH2CIO
				} else {
					s.closeConnection(fd)
					return
				}
			} else if data[0] == 'G' || data[0] == 'P' || data[0] == 'H' || data[0] == 'D' {
				state.protocol = protoHTTP1IO
			} else {
				s.closeConnection(fd)
				return
			}
		}

		var consumed int
		var closed bool

		switch state.protocol {
		case protoHTTP1IO:
			consumed = s.handleHTTP1(fd, buf[:pos])
		case protoH2CIO:
			consumed, closed = s.handleH2C(fd, state, buf[:pos])
			if closed {
				return
			}
		}

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

	s.connPos[fd] = pos
	if pos < bufferSize {
		s.submitRecv(fd)
	} else {
		s.closeConnection(fd)
	}
}

func (s *HybridServer) handleHTTP1(fd int, data []byte) int {
	headerEnd := bytes.Index(data, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		return 0
	}

	reqLen := headerEnd + 4

	// Check for POST with body
	if len(data) >= 4 && data[0] == 'P' && data[1] == 'O' && data[2] == 'S' && data[3] == 'T' {
		clIdx := bytes.Index(data[:headerEnd], []byte("Content-Length: "))
		if clIdx > 0 {
			clEnd := bytes.IndexByte(data[clIdx+16:headerEnd], '\r')
			if clEnd > 0 {
				cl := parseContentLengthIO(data[clIdx+16 : clIdx+16+clEnd])
				reqLen += cl
				if len(data) < reqLen {
					return 0
				}
			}
		}
	}

	// Ultra-fast path matching
	var response []byte
	if data[0] == 'G' {
		if len(data) > 6 && data[4] == '/' && data[5] == ' ' {
			response = responseSimple
		} else if len(data) > 9 && data[5] == 'j' {
			response = responseJSON
		} else if len(data) > 11 && data[5] == 'u' {
			response = responseUser
		} else {
			response = response404
		}
	} else if data[0] == 'P' {
		response = responseOK
	} else {
		response = response404
	}

	rawWriteIO(fd, response)
	return reqLen
}

func (s *HybridServer) closeConnection(fd int) {
	_ = unix.Close(fd)
	s.connPos[fd] = 0
	s.connState[fd] = nil
}

func (s *HybridServer) handleH2C(fd int, state *hybridioConnState, data []byte) (consumed int, closed bool) {
	offset := 0

	if !state.prefaceRecv {
		if len(data) >= h2PrefaceLen {
			if string(data[:h2PrefaceLen]) == h2Preface {
				state.prefaceRecv = true
				offset = h2PrefaceLen

				if !state.settingsSent {
					rawWriteIO(fd, frameSettings)
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
				rawWriteIO(fd, frameSettingsAck)
			}
		case h2FrameTypeHeaders:
			endStream := flags&h2FlagEndStream != 0
			s.handleH2Request(fd, streamID, frameData, endStream, state)

		case h2FrameTypeData:
			if length > 0 {
				s.sendWindowUpdates(fd, streamID, uint32(length))
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

func (s *HybridServer) handleH2Request(fd int, streamID uint32, headerBlock []byte, endStream bool, state *hybridioConnState) {
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

func (s *HybridServer) send100Continue(fd int, streamID uint32) {
	h100 := []byte{0x08, 0x03, 0x31, 0x30, 0x30}
	frame := make([]byte, 9+len(h100))
	frame[2] = byte(len(h100))
	frame[3] = h2FrameTypeHeaders
	frame[4] = h2FlagEndHeaders
	binary.BigEndian.PutUint32(frame[5:9], streamID)
	copy(frame[9:], h100)
	rawWriteIO(fd, frame)
}

func (s *HybridServer) sendH2Response(fd int, streamID uint32, path string) {
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

	rawWriteIO(fd, frame)
}

func (s *HybridServer) sendWindowUpdates(fd int, streamID uint32, increment uint32) {
	// Coalesce both window updates
	frame := make([]byte, 26)
	frame[2] = 0x04
	frame[3] = 0x08
	binary.BigEndian.PutUint32(frame[9:13], increment)
	frame[15] = 0x04
	frame[16] = 0x08
	binary.BigEndian.PutUint32(frame[18:22], streamID)
	binary.BigEndian.PutUint32(frame[22:26], increment)
	rawWriteIO(fd, frame)
}
