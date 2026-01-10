//go:build linux

// Package iouring provides barebones HTTP servers using io_uring with multishot.
// These implementations are intentionally minimal to test the theoretical I/O limits.
package iouring

import (
	"bytes"
	"fmt"
	"log"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// io_uring constants
	IORING_SETUP_SQPOLL        = 1 << 1
	IORING_SETUP_SQ_AFF        = 1 << 2
	IORING_SETUP_COOP_TASKRUN  = 1 << 8
	IORING_SETUP_SINGLE_ISSUER = 1 << 12

	IORING_OP_NOP              = 0
	IORING_OP_ACCEPT           = 13
	IORING_OP_RECV             = 27
	IORING_OP_SEND             = 26
	IORING_OP_PROVIDE_BUFFERS  = 31
	IORING_OP_ACCEPT_MULTISHOT = 13

	IORING_ACCEPT_MULTISHOT = 1 << 0
	IORING_RECV_MULTISHOT   = 1 << 1

	IOSQE_BUFFER_SELECT = 1 << 3

	// io_uring_enter flags
	IORING_ENTER_GETEVENTS = 1 << 0

	// Buffer ring constants
	ringSize    = 256
	sqeCount    = 256
	bufferCount = 1024
	bufferSize  = 4096
	bufferGroup = 0
)

// Pre-built HTTP/1.1 responses
var (
	responseSimple = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 13\r\nConnection: keep-alive\r\n\r\nHello, World!")
	responseJSON   = []byte(`HTTP/1.1 200 OK` + "\r\n" + `Content-Type: application/json` + "\r\n" + `Content-Length: 50` + "\r\n" + `Connection: keep-alive` + "\r\n\r\n" + `{"message":"Hello, World!","server":"iouring-h1"}`)
	responseOK     = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\nConnection: keep-alive\r\n\r\nOK")
	response404    = []byte("HTTP/1.1 404 Not Found\r\nContent-Type: text/plain\r\nContent-Length: 9\r\nConnection: close\r\n\r\nNot Found")
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

// HTTP1Server is a barebones HTTP/1.1 server using io_uring with multishot.
type HTTP1Server struct {
	port     string
	ringFd   int
	listenFd int
	sqePtr   unsafe.Pointer
	cqePtr   unsafe.Pointer
	sqHead   *uint32
	sqTail   *uint32
	cqHead   *uint32
	cqTail   *uint32
	sqMask   uint32
	cqMask   uint32
	sqes     []IoUringSqe
	cqes     []IoUringCqe
	sqArray  []uint32
	buffers  []byte
}

// NewHTTP1Server creates a new barebones io_uring HTTP/1.1 server.
func NewHTTP1Server(port string) *HTTP1Server {
	return &HTTP1Server{
		port:    port,
		buffers: make([]byte, bufferCount*bufferSize),
	}
}

// Run starts the io_uring event loop.
func (s *HTTP1Server) Run() error {
	// Create listening socket
	listenFd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	s.listenFd = listenFd

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

	// Setup io_uring
	if err := s.setupRing(); err != nil {
		return fmt.Errorf("setup ring: %w", err)
	}

	// Register buffers for multishot recv
	if err := s.setupBuffers(); err != nil {
		return fmt.Errorf("setup buffers: %w", err)
	}

	// Submit multishot accept
	s.submitMultishotAccept()

	log.Printf("iouring-h1 server listening on port %s", s.port)
	return s.eventLoop()
}

func (s *HTTP1Server) setupRing() error {
	params := &IoUringParams{
		Flags: IORING_SETUP_COOP_TASKRUN | IORING_SETUP_SINGLE_ISSUER,
	}

	ringFd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, sqeCount, uintptr(unsafe.Pointer(params)), 0)
	if errno != 0 {
		return fmt.Errorf("io_uring_setup: %w", errno)
	}
	s.ringFd = int(ringFd)

	// Memory map the ring
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
	s.sqArray = (*[ringSize]uint32)(unsafe.Pointer(&sqPtr[params.SqOff.Array]))[:]

	s.cqHead = (*uint32)(unsafe.Pointer(&cqPtr[params.CqOff.Head]))
	s.cqTail = (*uint32)(unsafe.Pointer(&cqPtr[params.CqOff.Tail]))
	s.cqMask = *((*uint32)(unsafe.Pointer(&cqPtr[params.CqOff.RingMask])))
	s.cqes = (*[ringSize]IoUringCqe)(unsafe.Pointer(&cqPtr[params.CqOff.Cqes]))[:]

	s.sqes = (*[ringSize]IoUringSqe)(unsafe.Pointer(&sqePtr[0]))[:]

	return nil
}

func (s *HTTP1Server) setupBuffers() error {
	// Provide buffers to the ring for multishot recv
	tail := *s.sqTail
	idx := tail & s.sqMask

	sqe := &s.sqes[idx]
	sqe.Opcode = IORING_OP_PROVIDE_BUFFERS
	sqe.Fd = int32(bufferCount)
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&s.buffers[0])))
	sqe.Len = bufferSize
	sqe.Off = 0
	sqe.OpcodeFlags = bufferGroup
	sqe.UserData = 0 // Special: buffer provision

	s.sqArray[idx] = uint32(idx)
	*s.sqTail = tail + 1

	// Submit
	_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(s.ringFd), 1, 0, 0, 0, 0)
	if errno != 0 {
		return fmt.Errorf("io_uring_enter (buffers): %w", errno)
	}

	return nil
}

func (s *HTTP1Server) submitMultishotAccept() {
	tail := *s.sqTail
	idx := tail & s.sqMask

	sqe := &s.sqes[idx]
	sqe.Opcode = IORING_OP_ACCEPT
	sqe.Fd = int32(s.listenFd)
	sqe.Addr = 0
	sqe.Off = 0
	sqe.OpcodeFlags = IORING_ACCEPT_MULTISHOT
	sqe.UserData = uint64(s.listenFd) // Mark as listen fd

	s.sqArray[idx] = uint32(idx)
	*s.sqTail = tail + 1
}

func (s *HTTP1Server) submitMultishotRecv(fd int) {
	tail := *s.sqTail
	idx := tail & s.sqMask

	sqe := &s.sqes[idx]
	sqe.Opcode = IORING_OP_RECV
	sqe.Fd = int32(fd)
	sqe.Addr = 0
	sqe.Len = 0
	sqe.Flags = IOSQE_BUFFER_SELECT
	sqe.OpcodeFlags = IORING_RECV_MULTISHOT | (bufferGroup << 16)
	sqe.UserData = uint64(fd)

	s.sqArray[idx] = uint32(idx)
	*s.sqTail = tail + 1
}

func (s *HTTP1Server) submitSend(fd int, data []byte) {
	tail := *s.sqTail
	idx := tail & s.sqMask

	sqe := &s.sqes[idx]
	sqe.Opcode = IORING_OP_SEND
	sqe.Fd = int32(fd)
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&data[0])))
	sqe.Len = uint32(len(data))
	sqe.UserData = uint64(fd) | (1 << 32) // Mark as send

	s.sqArray[idx] = uint32(idx)
	*s.sqTail = tail + 1
}

func (s *HTTP1Server) eventLoop() error {
	for {
		// Submit and wait for completions
		_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(s.ringFd),
			uintptr(*s.sqTail-*s.sqHead), 1, IORING_ENTER_GETEVENTS, 0, 0)
		if errno != 0 && errno != unix.EINTR {
			return fmt.Errorf("io_uring_enter: %w", errno)
		}

		// Process completions
		for head := *s.cqHead; head != *s.cqTail; head++ {
			idx := head & s.cqMask
			cqe := &s.cqes[idx]

			fd := int(cqe.UserData & 0xFFFFFFFF)
			isSend := (cqe.UserData>>32)&1 == 1

			if fd == s.listenFd && !isSend {
				// Accept completion
				if cqe.Res >= 0 {
					connFd := int(cqe.Res)
					unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
					s.submitMultishotRecv(connFd)
				}
			} else if !isSend {
				// Recv completion
				if cqe.Res > 0 {
					// Get buffer index from flags
					bufIdx := int((cqe.Flags >> 16) & 0xFFFF)
					dataLen := int(cqe.Res)
					data := s.buffers[bufIdx*bufferSize : bufIdx*bufferSize+dataLen]

					// Handle request
					s.handleRequest(fd, data)

					// Re-provide the buffer
					s.reprovideBuffer(bufIdx)
				} else if cqe.Res <= 0 {
					unix.Close(fd)
				}
			}

			*s.cqHead = head + 1
		}
	}
}

func (s *HTTP1Server) reprovideBuffer(bufIdx int) {
	tail := *s.sqTail
	idx := tail & s.sqMask

	sqe := &s.sqes[idx]
	sqe.Opcode = IORING_OP_PROVIDE_BUFFERS
	sqe.Fd = 1
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&s.buffers[bufIdx*bufferSize])))
	sqe.Len = bufferSize
	sqe.Off = uint64(bufIdx)
	sqe.OpcodeFlags = bufferGroup

	s.sqArray[idx] = uint32(idx)
	*s.sqTail = tail + 1
}

func (s *HTTP1Server) handleRequest(fd int, data []byte) {
	var response []byte

	if bytes.HasPrefix(data, []byte("GET / ")) {
		response = responseSimple
	} else if bytes.HasPrefix(data, []byte("GET /json")) {
		response = responseJSON
	} else if bytes.HasPrefix(data, []byte("GET /users/")) {
		response = responseSimple // Simplified
	} else if bytes.HasPrefix(data, []byte("POST /upload")) {
		response = responseOK
	} else {
		response = response404
	}

	s.submitSend(fd, response)
}
