//go:build linux

package iouring

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"syscall"
	"unsafe"

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
	bodySimple         = []byte("Hello, World!")
	bodyJSON           = []byte(`{"message":"Hello, World!","server":"iouring-h2"}`)
	bodyOK             = []byte("OK")

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

// h2ioConnState tracks HTTP/2 connection state
type h2ioConnState struct {
	prefaceRecv  bool
	settingsSent bool
	streams      map[uint32]string
}

// HTTP2Server is a barebones H2C server using io_uring.
type HTTP2Server struct {
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
	// Pre-allocated buffers indexed by fd
	buffers [bufferCount][]byte
	connPos [bufferCount]int
	connH2  [bufferCount]*h2ioConnState
}

// NewHTTP2Server creates a new barebones io_uring H2C server.
func NewHTTP2Server(port string) *HTTP2Server {
	s := &HTTP2Server{
		port: port,
	}
	for i := 0; i < bufferCount; i++ {
		s.buffers[i] = make([]byte, bufferSize)
	}
	return s
}

// Run starts the io_uring event loop for H2C.
func (s *HTTP2Server) Run() error {
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

func (s *HTTP2Server) setupRing() error {
	params := &IoUringParams{Flags: 0}
	ringFd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, sqeCount, uintptr(unsafe.Pointer(params)), 0)
	if errno != 0 {
		return fmt.Errorf("io_uring_setup: %w", errno)
	}
	s.ringFd = int(ringFd)

	sqSize := params.SqOff.Array + params.SqEntries*4
	cqSize := params.CqOff.Cqes + params.CqEntries*uint32(unsafe.Sizeof(IoUringCqe{}))

	sqPtr, _ := unix.Mmap(s.ringFd, 0, int(sqSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	cqPtr, _ := unix.Mmap(s.ringFd, 0x8000000, int(cqSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	sqeSize := params.SqEntries * uint32(unsafe.Sizeof(IoUringSqe{}))
	sqePtr, _ := unix.Mmap(s.ringFd, 0x10000000, int(sqeSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)

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

func (s *HTTP2Server) getSqe() *IoUringSqe {
	head := atomic.LoadUint32(s.sqHead)
	next := s.sqeTail + 1
	if next-head > uint32(sqeCount) {
		return nil
	}
	idx := s.sqeTail & s.sqMask
	s.sqeTail = next
	sqe := &s.sqes[idx]
	*sqe = IoUringSqe{}
	return sqe
}

func (s *HTTP2Server) flushSq() {
	tail := s.sqeTail
	sqTail := atomic.LoadUint32(s.sqTail)
	for i := sqTail; i < tail; i++ {
		s.sqArray[i&s.sqMask] = i & s.sqMask
	}
	atomic.StoreUint32(s.sqTail, tail)
}

func (s *HTTP2Server) submitAccept() {
	sqe := s.getSqe()
	if sqe == nil {
		return
	}
	sqe.Opcode = IORING_OP_ACCEPT
	sqe.Fd = int32(s.listenFd)
	sqe.UserData = uint64(s.listenFd)
	s.flushSq()
}

func (s *HTTP2Server) submitRecv(fd int) {
	sqe := s.getSqe()
	if sqe == nil {
		return
	}
	pos := s.connPos[fd]
	sqe.Opcode = IORING_OP_RECV
	sqe.Fd = int32(fd)
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&s.buffers[fd][pos])))
	sqe.Len = uint32(bufferSize - pos)
	sqe.UserData = uint64(fd)
	s.flushSq()
}

func (s *HTTP2Server) eventLoop() error {
	for {
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
						s.connH2[connFd] = &h2ioConnState{streams: make(map[uint32]string)}
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
					s.closeConn(fd)
				}
			} else {
				if cqe.Res < 0 {
					s.closeConn(fd)
				}
			}
		}

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

func (s *HTTP2Server) handleData(fd int, n int) {
	s.connPos[fd] += n
	buf := s.buffers[fd]
	pos := s.connPos[fd]
	state := s.connH2[fd]
	if state == nil {
		s.closeConn(fd)
		return
	}

	for {
		consumed, closed := s.processH2(fd, state, buf[:pos])
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
		} else {
			break
		}
	}

	s.connPos[fd] = pos
	if pos < bufferSize {
		s.submitRecv(fd)
	} else {
		s.closeConn(fd)
	}
}

func (s *HTTP2Server) processH2(fd int, state *h2ioConnState, data []byte) (int, bool) {
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
			s.closeConn(fd)
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

func (s *HTTP2Server) handleH2Request(fd int, streamID uint32, headerBlock []byte, endStream bool, state *h2ioConnState) {
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
	rawWriteIO(fd, frame)
}

func (s *HTTP2Server) sendWindowUpdates(fd int, streamID uint32, increment uint32) {
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

	rawWriteIO(fd, frame)
}

func (s *HTTP2Server) closeConn(fd int) {
	_ = unix.Close(fd)
	s.connPos[fd] = 0
	s.connH2[fd] = nil
}

func rawWriteH2(fd int, data []byte) {
	if len(data) == 0 {
		return
	}
	_, _, _ = syscall.Syscall(syscall.SYS_WRITE,
		uintptr(fd),
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)))
}
