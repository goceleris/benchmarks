//go:build linux

// Package iouring provides barebones HTTP servers using io_uring for maximum throughput.
// These implementations use multiple worker threads with SO_REUSEPORT for true parallelism.
package iouring

import (
	"bytes"
	"fmt"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	IORING_OP_ACCEPT = 13
	IORING_OP_RECV   = 27
	IORING_OP_SEND   = 26

	IORING_ENTER_GETEVENTS = 1 << 0

	sqeCount    = 4096
	bufferCount = 4096 // Reduced from 65536 - matches sqeCount
	bufferSize  = 8192 // Sufficient for most HTTP requests including POST with body
)

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
}

// HTTP1Server is a multi-threaded HTTP/1.1 server using io_uring.
type HTTP1Server struct {
	port       string
	numWorkers int
	workers    []*ioWorker
}

// NewHTTP1Server creates a new multi-threaded io_uring HTTP/1.1 server.
func NewHTTP1Server(port string) *HTTP1Server {
	numWorkers := runtime.NumCPU()
	if numWorkers < 1 {
		numWorkers = 1
	}
	return &HTTP1Server{
		port:       port,
		numWorkers: numWorkers,
		workers:    make([]*ioWorker, numWorkers),
	}
}

// Run starts multiple io_uring event loops.
func (s *HTTP1Server) Run() error {
	var portNum int
	_, _ = fmt.Sscanf(s.port, "%d", &portNum)

	errCh := make(chan error, s.numWorkers)

	for i := 0; i < s.numWorkers; i++ {
		w := &ioWorker{
			id:      i,
			port:    portNum,
			buffers: make([][]byte, bufferCount),
			connPos: make([]int, bufferCount),
		}
		for j := 0; j < bufferCount; j++ {
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

func (w *ioWorker) setupRing() error {
	params := &IoUringParams{
		Flags: 0,
	}

	ringFd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, sqeCount, uintptr(unsafe.Pointer(params)), 0)
	if errno != 0 {
		return fmt.Errorf("io_uring_setup: %w", errno)
	}
	w.ringFd = int(ringFd)

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
	if next-head > uint32(sqeCount) {
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

func (w *ioWorker) prepareAccept() {
	sqe := w.getSqe()
	if sqe == nil {
		return
	}
	sqe.Opcode = IORING_OP_ACCEPT
	sqe.Fd = int32(w.listenFd)
	sqe.UserData = uint64(w.listenFd)
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

func (w *ioWorker) eventLoop() error {
	for {
		processed := 0
		for {
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
					if connFd < bufferCount {
						// Set TCP_NODELAY for low latency
						_ = unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
						w.connPos[connFd] = 0
						w.prepareRecv(connFd)
					} else {
						_ = unix.Close(connFd)
					}
					w.prepareAccept()
				}
			} else if !isSend {
				if cqe.Res > 0 {
					w.handleData(fd, int(cqe.Res))
				} else {
					_ = unix.Close(fd)
					w.connPos[fd] = 0
				}
			} else {
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

		_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(w.ringFd),
			uintptr(toSubmit), 1, IORING_ENTER_GETEVENTS, 0, 0)
		if errno != 0 && errno != unix.EINTR {
			return fmt.Errorf("io_uring_enter: %w", errno)
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
			clEnd := bytes.IndexByte(data[clIdx+16:headerEnd], '\r')
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
