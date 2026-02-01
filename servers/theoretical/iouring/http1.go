//go:build linux

// Package iouring provides barebones HTTP servers using io_uring for maximum throughput.
// These implementations use multiple worker threads with SO_REUSEPORT for true parallelism.
// Buffer pool and queue sizes scale automatically based on available CPUs.
//
// OPTIMIZATION FEATURES:
// - SQPOLL mode: Kernel thread polls SQ, eliminating io_uring_enter syscalls in hot path
// - Multishot accept: Single SQE handles multiple accepts, reducing submission overhead
// - Cooperative task running: Reduced context switching overhead
// - Single issuer mode: Optimized for single-threaded submission per ring
// - Batched CQ processing: Process multiple completions before submitting
package iouring

import (
	"bytes"
	"fmt"
	"log"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// io_uring operation codes
	IORING_OP_ACCEPT = 13
	IORING_OP_RECV   = 27
	IORING_OP_SEND   = 26

	// io_uring_enter flags
	IORING_ENTER_GETEVENTS = 1 << 0
	IORING_ENTER_SQ_WAKEUP = 1 << 1 // Wake up SQ polling thread

	// io_uring_setup flags for advanced features
	IORING_SETUP_SQPOLL        = 1 << 1  // Kernel polls SQ, eliminating syscalls
	IORING_SETUP_COOP_TASKRUN  = 1 << 8  // Cooperative task running (kernel 5.19+)
	IORING_SETUP_SINGLE_ISSUER = 1 << 12 // Only one thread submits (kernel 6.0+)

	// Accept flags for multishot (kernel 5.19+)
	// Single SQE handles multiple accepts continuously
	IORING_ACCEPT_MULTISHOT = 1 << 0

	// CQE flags
	IORING_CQE_F_MORE = 1 << 1 // More completions coming (multishot indicator)

	// SQ ring flags (read from mapped ring)
	IORING_SQ_NEED_WAKEUP = 1 << 0 // SQ poll thread needs wakeup

	// Feature flags returned in params.Features
	IORING_FEAT_FAST_POLL = 1 << 5 // Fast poll support (indicates modern kernel)

	// Base values for scaling - increased for high throughput
	sqeCountBase      = 2048  // Base submission queue size (doubled for batching)
	bufferCountPerCPU = 512   // Buffers per CPU per worker
	bufferSize        = 65536 // Size of each buffer (64KB for large POST bodies)

	// Limits
	sqeCountMin    = 2048
	sqeCountMax    = 32768 // Increased for high connection counts
	bufferCountMin = 1024
	bufferCountMax = 65536

	// Batching parameters for optimal throughput
	maxBatchSize = 256  // Max CQEs to process before submitting
	sqPollIdleMs = 2000 // SQ poll thread idle timeout in milliseconds
)

// getScaledIOUringLimits returns io_uring limits scaled to available CPUs
func getScaledIOUringLimits() (sqeCount int, bufferCount int) {
	numCPU := runtime.NumCPU()

	// Scale submission queue with CPU count
	sqeCount = sqeCountBase * numCPU
	if sqeCount < sqeCountMin {
		sqeCount = sqeCountMin
	}
	if sqeCount > sqeCountMax {
		sqeCount = sqeCountMax
	}

	// Scale buffer pool with CPU count
	bufferCount = bufferCountPerCPU * numCPU
	if bufferCount < bufferCountMin {
		bufferCount = bufferCountMin
	}
	if bufferCount > bufferCountMax {
		bufferCount = bufferCountMax
	}

	return sqeCount, bufferCount
}

var (
	responseSimple = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 13\r\nConnection: keep-alive\r\n\r\nHello, World!")
	responseJSON   = []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 49\r\nConnection: keep-alive\r\n\r\n{\"message\":\"Hello, World!\",\"server\":\"iouring-h1\"}")
	responseOK     = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\nConnection: keep-alive\r\n\r\nOK")
	response404    = []byte("HTTP/1.1 404 Not Found\r\nContent-Type: text/plain\r\nContent-Length: 9\r\nConnection: keep-alive\r\n\r\nNot Found")
	responseUser   = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 13\r\nConnection: keep-alive\r\n\r\nHello, World!")
)

type IoUringSqe struct {
	Opcode      uint8
	Flags       uint8
	Ioprio      uint16
	Fd          int32
	Off         uint64
	Addr        uint64
	Len         uint32
	OpcodeFlags uint32
	UserData    uint64
	BufIndex    uint16
	Personality uint16
	SpliceFdIn  int32
	Pad2        [2]uint64
}

type IoUringCqe struct {
	UserData uint64
	Res      int32
	Flags    uint32
}

type IoUringParams struct {
	SqEntries    uint32
	CqEntries    uint32
	Flags        uint32
	SqThreadCpu  uint32
	SqThreadIdle uint32
	Features     uint32
	WqFd         uint32
	Resv         [3]uint32
	SqOff        IoSqringOffsets
	CqOff        IoCqringOffsets
}

type IoSqringOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Flags       uint32
	Dropped     uint32
	Array       uint32
	Resv1       uint32
	Resv2       uint64
}

type IoCqringOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Overflow    uint32
	Cqes        uint32
	Flags       uint32
	Resv1       uint32
	Resv2       uint64
}

type ioWorker struct {
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
	sqeCount      int
	bufferCount   int

	// Optimization flags
	useSQPoll          bool // Using SQPOLL mode (kernel polls SQ)
	useMultishotAccept bool // Using multishot accept
	acceptPending      bool // Multishot accept is active
}

// HTTP1Server is a multi-threaded HTTP/1.1 server using io_uring.
type HTTP1Server struct {
	port        string
	numWorkers  int
	workers     []*ioWorker
	sqeCount    int
	bufferCount int
}

// NewHTTP1Server creates a new multi-threaded io_uring HTTP/1.1 server.
// Queue sizes and buffer pools scale automatically based on available CPUs.
func NewHTTP1Server(port string) *HTTP1Server {
	numWorkers := runtime.NumCPU()
	if numWorkers < 1 {
		numWorkers = 1
	}

	sqeCount, bufferCount := getScaledIOUringLimits()

	return &HTTP1Server{
		port:        port,
		numWorkers:  numWorkers,
		workers:     make([]*ioWorker, numWorkers),
		sqeCount:    sqeCount,
		bufferCount: bufferCount,
	}
}

// Run starts multiple io_uring event loops.
func (s *HTTP1Server) Run() error {
	var portNum int
	_, _ = fmt.Sscanf(s.port, "%d", &portNum)

	errCh := make(chan error, s.numWorkers)

	for i := 0; i < s.numWorkers; i++ {
		w := &ioWorker{
			id:          i,
			port:        portNum,
			buffers:     make([][]byte, s.bufferCount),
			connPos:     make([]int, s.bufferCount),
			sqeCount:    s.sqeCount,
			bufferCount: s.bufferCount,
		}
		for j := 0; j < s.bufferCount; j++ {
			w.buffers[j] = make([]byte, bufferSize)
		}
		s.workers[i] = w

		go func(worker *ioWorker) {
			runtime.LockOSThread()
			if err := worker.run(); err != nil {
				errCh <- err
			}
		}(w)
	}

	log.Printf("iouring-h1 server listening on port %s with %d workers (sqeCount=%d, bufferCount=%d per worker)",
		s.port, s.numWorkers, s.sqeCount, s.bufferCount)

	return <-errCh
}

func (w *ioWorker) run() error {
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
func (w *ioWorker) setupRing() error {
	// Try to setup with SQPOLL mode first for maximum performance.
	// SQPOLL eliminates io_uring_enter syscalls in the hot path by having
	// a kernel thread continuously poll the submission queue.
	params := &IoUringParams{
		Flags:        IORING_SETUP_SQPOLL | IORING_SETUP_COOP_TASKRUN | IORING_SETUP_SINGLE_ISSUER,
		SqThreadIdle: sqPollIdleMs,
	}

	ringFd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, uintptr(w.sqeCount), uintptr(unsafe.Pointer(params)), 0)
	if errno != 0 {
		// SQPOLL may fail due to permissions (needs CAP_SYS_NICE) or kernel version.
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
		if w.id == 0 {
			log.Printf("io_uring: SQPOLL mode enabled (eliminates syscalls in hot path)")
		}
	}
	w.ringFd = int(ringFd)

	// Check for multishot accept support (kernel 5.19+).
	// IORING_FEAT_FAST_POLL indicates a modern kernel with multishot support.
	w.useMultishotAccept = (params.Features & IORING_FEAT_FAST_POLL) != 0
	if w.id == 0 && w.useMultishotAccept {
		log.Printf("io_uring: Multishot accept enabled (single SQE handles multiple accepts)")
	}

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

func (w *ioWorker) getSqe() *IoUringSqe {
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

func (w *ioWorker) flushAndSubmit() {
	tail := w.sqeTail
	for i := w.lastFlushed; i < tail; i++ {
		w.sqArray[i&w.sqMask] = i & w.sqMask
	}
	atomic.StoreUint32(w.sqTail, tail)
	w.lastFlushed = tail
}

// prepareAccept submits an accept operation.
// With multishot accept (kernel 5.19+), a single SQE handles multiple accepts
// continuously, avoiding the overhead of re-submitting after each connection.
func (w *ioWorker) prepareAccept() {
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

	// Enable multishot accept if supported - single SQE handles multiple connections
	if w.useMultishotAccept {
		sqe.Ioprio = IORING_ACCEPT_MULTISHOT
		w.acceptPending = true
	}
}

func (w *ioWorker) prepareRecv(fd int) {
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
// It processes multiple CQEs before submitting to reduce syscall overhead.
func (w *ioWorker) eventLoop() error {
	for {
		processed := 0

		// Process completions in batches for better cache efficiency
		// and reduced syscall overhead
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
				// Handle accept completion
				if cqe.Res >= 0 {
					connFd := int(cqe.Res)
					if connFd < w.bufferCount {
						// Set TCP_NODELAY for low latency
						_ = unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
						w.connPos[connFd] = 0
						w.prepareRecv(connFd)
					} else {
						_ = unix.Close(connFd)
					}
				}

				// For multishot accept, check if we need to re-arm.
				// IORING_CQE_F_MORE indicates more completions will come from this SQE.
				if w.useMultishotAccept {
					if cqe.Flags&IORING_CQE_F_MORE == 0 {
						// Multishot terminated (error or kernel decision), re-arm
						w.acceptPending = false
						w.prepareAccept()
					}
					// Otherwise multishot is still active, no need to re-submit
				} else {
					// Single-shot mode: always re-submit accept
					w.prepareAccept()
				}
			} else if !isSend {
				// Handle recv completion
				if cqe.Res > 0 {
					w.handleData(fd, int(cqe.Res))
				} else {
					_ = unix.Close(fd)
					w.connPos[fd] = 0
				}
			} else {
				// Handle send completion
				if cqe.Res < 0 {
					_ = unix.Close(fd)
					w.connPos[fd] = 0
				}
			}
		}

		w.flushAndSubmit()

		tail := atomic.LoadUint32(w.sqTail)
		toSubmit := tail - w.submittedTail
		w.submittedTail = tail

		// In SQPOLL mode, the kernel thread polls the SQ ring automatically.
		// We only need to call io_uring_enter to wait for completions or
		// wake up the polling thread if it went idle.
		if w.useSQPoll {
			// Check if SQ poll thread needs wakeup (went idle due to no work)
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
			// Normal mode: submit and wait for completions
			_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(w.ringFd),
				uintptr(toSubmit), 1, IORING_ENTER_GETEVENTS, 0, 0)
			if errno != 0 && errno != unix.EINTR {
				return fmt.Errorf("io_uring_enter: %w", errno)
			}
		}
	}
}

func (w *ioWorker) handleData(fd int, n int) {
	w.connPos[fd] += n
	buf := w.buffers[fd]
	pos := w.connPos[fd]

	for {
		consumed := w.processRequest(fd, buf[:pos])
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

	w.connPos[fd] = pos
	if pos < bufferSize {
		w.prepareRecv(fd)
	} else {
		_ = unix.Close(fd)
		w.connPos[fd] = 0
	}
}

func (w *ioWorker) processRequest(fd int, data []byte) int {
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

	requestLen := headerEnd + 4

	if len(data) >= 4 && data[0] == 'P' && data[1] == 'O' && data[2] == 'S' && data[3] == 'T' {
		clIdx := bytes.Index(data[:headerEnd], []byte("Content-Length: "))
		if clIdx > 0 {
			// Search for \r in the rest of the data (not limited to headerEnd, since
			// headerEnd points to the final \r\n\r\n, not the header line's \r\n)
			clEnd := bytes.IndexByte(data[clIdx+16:], '\r')
			if clEnd > 0 {
				cl := parseContentLengthIO(data[clIdx+16 : clIdx+16+clEnd])
				requestLen += cl
				if len(data) < requestLen {
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

	rawWriteIO(fd, response)
	return requestLen
}

func parseContentLengthIO(b []byte) int {
	n := 0
	for _, c := range b {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

func rawWriteIO(fd int, data []byte) {
	if len(data) == 0 {
		return
	}
	_, _, _ = syscall.Syscall(syscall.SYS_WRITE,
		uintptr(fd),
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)))
}
