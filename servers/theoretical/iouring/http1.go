//go:build linux

// Package iouring provides barebones HTTP servers using io_uring for maximum throughput.
// These implementations are intentionally minimal to test the theoretical I/O limits.
package iouring

import (
	"bytes"
	"fmt"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// io_uring constants
	IORING_OP_NOP     = 0
	IORING_OP_ACCEPT  = 13
	IORING_OP_RECV    = 27
	IORING_OP_SEND    = 26
	IORING_OP_CLOSE   = 19
	IORING_OP_SENDMSG = 9

	IORING_ENTER_GETEVENTS = 1 << 0

	// Ring sizes - power of 2
	sqeCount    = 4096
	bufferCount = 65536
	bufferSize  = 16384

	UserDataBufferProvision = ^uint64(0)
)

// Pre-built HTTP/1.1 responses
var (
	responseSimple = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 13\r\nConnection: keep-alive\r\n\r\nHello, World!")
	responseJSON   = []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 49\r\nConnection: keep-alive\r\n\r\n{\"message\":\"Hello, World!\",\"server\":\"iouring-h1\"}")
	responseOK     = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\nConnection: keep-alive\r\n\r\nOK")
	response404    = []byte("HTTP/1.1 404 Not Found\r\nContent-Type: text/plain\r\nContent-Length: 9\r\nConnection: keep-alive\r\n\r\nNot Found")
	responseUser   = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 13\r\nConnection: keep-alive\r\n\r\nHello, World!")
)

// io_uring structures
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

// HTTP1Server is a barebones HTTP/1.1 server using io_uring.
type HTTP1Server struct {
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
}

// NewHTTP1Server creates a new barebones io_uring HTTP/1.1 server.
func NewHTTP1Server(port string) *HTTP1Server {
	s := &HTTP1Server{
		port: port,
	}
	// Pre-allocate all buffers
	for i := 0; i < bufferCount; i++ {
		s.buffers[i] = make([]byte, bufferSize)
	}
	return s
}

// Run starts the io_uring event loop.
func (s *HTTP1Server) Run() error {
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

func (s *HTTP1Server) setupRing() error {
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

func (s *HTTP1Server) getSqe() *IoUringSqe {
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

func (s *HTTP1Server) flushSq() {
	tail := s.sqeTail
	sqTail := atomic.LoadUint32(s.sqTail)
	for i := sqTail; i < tail; i++ {
		s.sqArray[i&s.sqMask] = i & s.sqMask
	}
	atomic.StoreUint32(s.sqTail, tail)
}

func (s *HTTP1Server) submitAccept() {
	sqe := s.getSqe()
	if sqe == nil {
		return
	}
	sqe.Opcode = IORING_OP_ACCEPT
	sqe.Fd = int32(s.listenFd)
	sqe.UserData = uint64(s.listenFd)
	s.flushSq()
}

func (s *HTTP1Server) submitRecv(fd int) {
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

func (s *HTTP1Server) submitSend(fd int, data []byte) {
	sqe := s.getSqe()
	if sqe == nil {
		return
	}
	sqe.Opcode = IORING_OP_SEND
	sqe.Fd = int32(fd)
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&data[0])))
	sqe.Len = uint32(len(data))
	sqe.UserData = uint64(fd) | (1 << 32)
	s.flushSq()
}

func (s *HTTP1Server) eventLoop() error {
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
					_ = unix.Close(fd)
					s.connPos[fd] = 0
				}
			} else {
				if cqe.Res < 0 {
					_ = unix.Close(fd)
					s.connPos[fd] = 0
				}
			}
		}

		// Batch submit
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

func (s *HTTP1Server) handleData(fd int, n int) {
	s.connPos[fd] += n
	buf := s.buffers[fd]
	pos := s.connPos[fd]

	// Process all complete requests
	for {
		data := buf[:pos]
		consumed := s.processRequest(fd, data)
		if consumed == 0 {
			break
		}
		if consumed < pos {
			copy(buf, buf[consumed:pos])
			pos -= consumed
		} else {
			pos = 0
		}
	}

	s.connPos[fd] = pos
	if pos < bufferSize {
		s.submitRecv(fd)
	} else {
		_ = unix.Close(fd)
		s.connPos[fd] = 0
	}
}

func (s *HTTP1Server) processRequest(fd int, data []byte) int {
	headerEnd := bytes.Index(data, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		return 0
	}

	requestLen := headerEnd + 4

	// Check for POST with body
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

	// Route request
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

	// Direct write for response
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
