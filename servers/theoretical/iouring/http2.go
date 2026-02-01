//go:build linux

// HTTP/2 (H2C) server implementation using io_uring with advanced optimizations.
// Supports SQPOLL mode, multishot accept, and batched CQ processing.
package iouring

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"runtime"
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
		0x00, 0x01, 0x00, 0x00, 0x10, 0x00,
		0x00, 0x04, 0x00, 0x60, 0x00, 0x00,
	}
	frameSettingsAck = []byte{0x00, 0x00, 0x00, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00}
)

// h2ioConnState tracks HTTP/2 connection state
type h2ioConnState struct {
	prefaceRecv  bool
	settingsSent bool
	streams      map[uint32]string
}

// h2ioWorker represents a single HTTP/2 event loop worker
type h2ioWorker struct {
	id            int
	port          int
	ringFd        int
	listenFd      int
	sqHead        *uint32
	sqTail        *uint32
	cqHead        *uint32
	cqTail        *uint32
	sqFlags       *uint32 // SQ ring flags for SQPOLL wakeup detection
	sqMask        uint32
	cqMask        uint32
	sqeTail       uint32
	lastFlushed   uint32
	submittedTail uint32
	sqes          []IoUringSqe
	cqes          []IoUringCqe
	sqArray       []uint32
	buffers       [][]byte
	connPos       []int
	connH2        []*h2ioConnState
	sqeCount      int
	bufferCount   int

	// Optimization flags
	useSQPoll          bool // Using SQPOLL mode (kernel polls SQ)
	useMultishotAccept bool // Using multishot accept
	acceptPending      bool // Multishot accept is active
}

// HTTP2Server is a multi-threaded H2C server using io_uring.
type HTTP2Server struct {
	port        string
	numWorkers  int
	workers     []*h2ioWorker
	sqeCount    int
	bufferCount int
}

// NewHTTP2Server creates a new multi-threaded io_uring H2C server.
func NewHTTP2Server(port string) *HTTP2Server {
	numWorkers := runtime.NumCPU()
	if numWorkers < 1 {
		numWorkers = 1
	}
	sqeCount, bufferCount := getScaledIOUringLimits()
	return &HTTP2Server{
		port:        port,
		numWorkers:  numWorkers,
		workers:     make([]*h2ioWorker, numWorkers),
		sqeCount:    sqeCount,
		bufferCount: bufferCount,
	}
}

// Run starts multiple io_uring event loops for H2C.
func (s *HTTP2Server) Run() error {
	var portNum int
	_, _ = fmt.Sscanf(s.port, "%d", &portNum)

	errCh := make(chan error, s.numWorkers)

	for i := 0; i < s.numWorkers; i++ {
		w := &h2ioWorker{
			id:          i,
			port:        portNum,
			buffers:     make([][]byte, s.bufferCount),
			connPos:     make([]int, s.bufferCount),
			connH2:      make([]*h2ioConnState, s.bufferCount),
			sqeCount:    s.sqeCount,
			bufferCount: s.bufferCount,
		}
		for j := 0; j < s.bufferCount; j++ {
			w.buffers[j] = make([]byte, bufferSize)
		}
		s.workers[i] = w

		go func(worker *h2ioWorker) {
			runtime.LockOSThread()
			if err := worker.run(); err != nil {
				errCh <- err
			}
		}(w)
	}

	return <-errCh
}

func (w *h2ioWorker) run() error {
	listenFd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
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

	if err := w.setupRing(); err != nil {
		return fmt.Errorf("setup ring: %w", err)
	}

	w.prepareAccept()
	w.flushAndSubmit()

	return w.eventLoop()
}

// setupRing initializes the io_uring ring with advanced optimizations.
// It attempts SQPOLL mode first (eliminates syscalls), then falls back gracefully.
func (w *h2ioWorker) setupRing() error {
	// Try to setup with SQPOLL mode first for maximum performance.
	// SQPOLL eliminates io_uring_enter syscalls in the hot path.
	params := &IoUringParams{
		Flags:        IORING_SETUP_SQPOLL | IORING_SETUP_COOP_TASKRUN | IORING_SETUP_SINGLE_ISSUER,
		SqThreadIdle: sqPollIdleMs,
	}

	ringFd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, uintptr(w.sqeCount), uintptr(unsafe.Pointer(params)), 0)
	if errno != 0 {
		// SQPOLL may fail due to permissions or kernel version.
		// Fall back to normal mode with cooperative task running optimization.
		params = &IoUringParams{
			Flags: IORING_SETUP_COOP_TASKRUN | IORING_SETUP_SINGLE_ISSUER,
		}
		ringFd, _, errno = unix.Syscall(unix.SYS_IO_URING_SETUP, uintptr(w.sqeCount), uintptr(unsafe.Pointer(params)), 0)
		if errno != 0 {
			// Final fallback: basic setup for older kernels
			params = &IoUringParams{Flags: 0}
			ringFd, _, errno = unix.Syscall(unix.SYS_IO_URING_SETUP, uintptr(w.sqeCount), uintptr(unsafe.Pointer(params)), 0)
			if errno != 0 {
				return fmt.Errorf("io_uring_setup: %w", errno)
			}
		}
		w.useSQPoll = false
	} else {
		w.useSQPoll = true
	}
	w.ringFd = int(ringFd)

	// Check for multishot accept support (kernel 5.19+)
	w.useMultishotAccept = (params.Features & IORING_FEAT_FAST_POLL) != 0

	sqSize := params.SqOff.Array + params.SqEntries*4
	cqSize := params.CqOff.Cqes + params.CqEntries*uint32(unsafe.Sizeof(IoUringCqe{}))

	sqPtr, err := unix.Mmap(w.ringFd, 0, int(sqSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap sq: %w", err)
	}

	cqPtr, err := unix.Mmap(w.ringFd, 0x8000000, int(cqSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap cq: %w", err)
	}

	sqeSize := params.SqEntries * uint32(unsafe.Sizeof(IoUringSqe{}))
	sqePtr, err := unix.Mmap(w.ringFd, 0x10000000, int(sqeSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap sqes: %w", err)
	}

	w.sqHead = (*uint32)(unsafe.Pointer(&sqPtr[params.SqOff.Head]))
	w.sqTail = (*uint32)(unsafe.Pointer(&sqPtr[params.SqOff.Tail]))
	w.sqMask = *((*uint32)(unsafe.Pointer(&sqPtr[params.SqOff.RingMask])))
	w.sqArray = unsafe.Slice((*uint32)(unsafe.Pointer(&sqPtr[params.SqOff.Array])), params.SqEntries)

	// Store pointer to SQ flags for SQPOLL wakeup detection
	w.sqFlags = (*uint32)(unsafe.Pointer(&sqPtr[params.SqOff.Flags]))

	w.cqHead = (*uint32)(unsafe.Pointer(&cqPtr[params.CqOff.Head]))
	w.cqTail = (*uint32)(unsafe.Pointer(&cqPtr[params.CqOff.Tail]))
	w.cqMask = *((*uint32)(unsafe.Pointer(&cqPtr[params.CqOff.RingMask])))
	w.cqes = unsafe.Slice((*IoUringCqe)(unsafe.Pointer(&cqPtr[params.CqOff.Cqes])), params.CqEntries)

	w.sqes = unsafe.Slice((*IoUringSqe)(unsafe.Pointer(&sqePtr[0])), params.SqEntries)

	w.sqeTail = atomic.LoadUint32(w.sqTail)
	w.lastFlushed = w.sqeTail
	w.submittedTail = w.sqeTail

	return nil
}

func (w *h2ioWorker) getSqe() *IoUringSqe {
	head := atomic.LoadUint32(w.sqHead)
	next := w.sqeTail + 1
	if next-head > uint32(w.sqeCount) {
		return nil
	}
	idx := w.sqeTail & w.sqMask
	w.sqeTail = next
	sqe := &w.sqes[idx]
	*sqe = IoUringSqe{}
	return sqe
}

func (w *h2ioWorker) flushAndSubmit() {
	tail := w.sqeTail
	for i := w.lastFlushed; i < tail; i++ {
		w.sqArray[i&w.sqMask] = i & w.sqMask
	}
	atomic.StoreUint32(w.sqTail, tail)
	w.lastFlushed = tail
}

// prepareAccept submits an accept operation.
// With multishot accept (kernel 5.19+), a single SQE handles multiple accepts.
func (w *h2ioWorker) prepareAccept() {
	// If multishot is active and pending, don't re-submit
	if w.useMultishotAccept && w.acceptPending {
		return
	}

	sqe := w.getSqe()
	if sqe == nil {
		return
	}
	sqe.Opcode = IORING_OP_ACCEPT
	sqe.Fd = int32(w.listenFd)
	sqe.UserData = uint64(w.listenFd)

	// Enable multishot accept if supported
	if w.useMultishotAccept {
		sqe.Ioprio = IORING_ACCEPT_MULTISHOT
		w.acceptPending = true
	}
}

func (w *h2ioWorker) prepareRecv(fd int) {
	sqe := w.getSqe()
	if sqe == nil {
		return
	}
	pos := w.connPos[fd]
	sqe.Opcode = IORING_OP_RECV
	sqe.Fd = int32(fd)
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&w.buffers[fd][pos])))
	sqe.Len = uint32(bufferSize - pos)
	sqe.UserData = uint64(fd)
}

// eventLoop is the main event processing loop with batching optimization.
func (w *h2ioWorker) eventLoop() error {
	for {
		processed := 0

		// Process completions in batches for better efficiency
		for processed < maxBatchSize {
			head := atomic.LoadUint32(w.cqHead)
			tail := atomic.LoadUint32(w.cqTail)
			if head == tail {
				break
			}

			cqe := &w.cqes[head&w.cqMask]
			atomic.StoreUint32(w.cqHead, head+1)
			processed++

			fd := int(cqe.UserData & 0xFFFFFFFF)
			isSend := (cqe.UserData>>32)&1 == 1

			if fd == w.listenFd && !isSend {
				if cqe.Res >= 0 {
					connFd := int(cqe.Res)
					if connFd < w.bufferCount {
						_ = unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
						_ = unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1)
						w.connPos[connFd] = 0
						w.connH2[connFd] = &h2ioConnState{streams: make(map[uint32]string)}
						w.prepareRecv(connFd)
					} else {
						_ = unix.Close(connFd)
					}
				}

				// Handle multishot accept re-arming
				if w.useMultishotAccept {
					if cqe.Flags&IORING_CQE_F_MORE == 0 {
						w.acceptPending = false
						w.prepareAccept()
					}
				} else {
					w.prepareAccept()
				}
			} else if !isSend {
				if cqe.Res > 0 {
					w.handleData(fd, int(cqe.Res))
				} else {
					w.closeConn(fd)
				}
			} else {
				if cqe.Res < 0 {
					w.closeConn(fd)
				}
			}
		}

		w.flushAndSubmit()

		tail := atomic.LoadUint32(w.sqTail)
		toSubmit := tail - w.submittedTail
		w.submittedTail = tail

		// SQPOLL mode handling
		if w.useSQPoll {
			flags := uint32(IORING_ENTER_GETEVENTS)
			if atomic.LoadUint32(w.sqFlags)&IORING_SQ_NEED_WAKEUP != 0 {
				flags |= IORING_ENTER_SQ_WAKEUP
			}
			_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(w.ringFd),
				0, 1, uintptr(flags), 0, 0)
			if errno != 0 && errno != unix.EINTR {
				return fmt.Errorf("io_uring_enter: %w", errno)
			}
		} else {
			_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(w.ringFd),
				uintptr(toSubmit), 1, IORING_ENTER_GETEVENTS, 0, 0)
			if errno != 0 && errno != unix.EINTR {
				return fmt.Errorf("io_uring_enter: %w", errno)
			}
		}
	}
}

func (w *h2ioWorker) handleData(fd int, n int) {
	w.connPos[fd] += n
	buf := w.buffers[fd]
	pos := w.connPos[fd]
	state := w.connH2[fd]
	if state == nil {
		w.closeConn(fd)
		return
	}

	for {
		consumed, closed := w.processH2(fd, state, buf[:pos])
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

	w.connPos[fd] = pos
	if pos < bufferSize {
		w.prepareRecv(fd)
	} else {
		w.closeConn(fd)
	}
}

func (w *h2ioWorker) processH2(fd int, state *h2ioConnState, data []byte) (int, bool) {
	offset := 0

	if !state.prefaceRecv {
		if len(data) >= h2PrefaceLen {
			if string(data[:h2PrefaceLen]) == h2Preface {
				state.prefaceRecv = true
				offset = h2PrefaceLen
				if !state.settingsSent {
					rawWriteH2IO(fd, frameSettings)
					state.settingsSent = true
				}
				return offset, false
			}
			w.closeConn(fd)
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
				rawWriteH2IO(fd, frameSettingsAck)
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

func (w *h2ioWorker) handleH2Request(fd int, streamID uint32, headerBlock []byte, endStream bool, state *h2ioConnState) {
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

func (w *h2ioWorker) send100Continue(fd int, streamID uint32) {
	h100 := []byte{0x08, 0x03, 0x31, 0x30, 0x30}
	frame := make([]byte, 9+len(h100))
	frame[2] = byte(len(h100))
	frame[3] = h2FrameTypeHeaders
	frame[4] = h2FlagEndHeaders
	binary.BigEndian.PutUint32(frame[5:9], streamID)
	copy(frame[9:], h100)
	rawWriteH2IO(fd, frame)
}

func (w *h2ioWorker) sendWindowUpdates(fd int, streamID uint32, increment uint32) {
	frame := make([]byte, 26)
	frame[2] = 0x04
	frame[3] = 0x08
	binary.BigEndian.PutUint32(frame[9:13], increment)
	frame[15] = 0x04
	frame[16] = 0x08
	binary.BigEndian.PutUint32(frame[18:22], streamID)
	binary.BigEndian.PutUint32(frame[22:26], increment)
	rawWriteH2IO(fd, frame)
}

func (w *h2ioWorker) sendH2Response(fd int, streamID uint32, path string) {
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

	rawWriteH2IO(fd, frame)
}

func (w *h2ioWorker) closeConn(fd int) {
	_ = unix.Close(fd)
	w.connPos[fd] = 0
	w.connH2[fd] = nil
}

func rawWriteH2IO(fd int, data []byte) {
	if len(data) == 0 {
		return
	}
	_, _, _ = syscall.Syscall(syscall.SYS_WRITE,
		uintptr(fd),
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)))
}
