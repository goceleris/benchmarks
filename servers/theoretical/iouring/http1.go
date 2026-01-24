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

// ... (existing code)

func (s *HTTP1Server) submitMultishotAccept() {
	tail := *s.sqTail
	idx := tail & s.sqMask

	sqe := &s.sqes[idx]
	sqe.Opcode = IORING_OP_ACCEPT
	sqe.Fd = int32(s.listenFd)
	sqe.Addr = 0
	sqe.Off = 0
	sqe.Ioprio = 0 // Single Shot Accept
	sqe.OpcodeFlags = 0
	sqe.UserData = uint64(s.listenFd) // Mark as listen fd

	s.sqArray[idx] = idx
	*s.sqTail = tail + 1
}

func (s *HTTP1Server) submitRecv(fd int) {
	tail := *s.sqTail
	idx := tail & s.sqMask

	s.sqes[idx] = IoUringSqe{} // Zero out the SQE
	sqe := &s.sqes[idx]
	sqe.Opcode = IORING_OP_RECV
	sqe.Fd = int32(fd)
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&s.buffers[fd*bufferSize])))
	sqe.Len = bufferSize
	sqe.Off = 0
	sqe.Flags = 0
	sqe.Ioprio = 0
	sqe.OpcodeFlags = 0
	sqe.UserData = uint64(fd)

	s.sqArray[idx] = idx
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

	s.sqArray[idx] = idx
	*s.sqTail = tail + 1
}

func (s *HTTP1Server) eventLoop() error {
	for {
		toSubmit := *s.sqTail - *s.sqHead

		_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(s.ringFd),
			uintptr(toSubmit), 1, IORING_ENTER_GETEVENTS, 0, 0)
		if errno != 0 && errno != unix.EINTR {
			return fmt.Errorf("io_uring_enter: %w", errno)
		}

		// Process completions
		// Note: Without atomics, we assume io_uring_enter acts as barrier
		head := *s.cqHead
		if head == *s.cqTail {
			continue
		}

		for ; head != *s.cqTail; head++ {
			idx := head & s.cqMask
			cqe := &s.cqes[idx]

			if cqe.UserData == UserDataBufferProvision {
				// Buffer provision completion or nop
				*s.cqHead = head + 1
				continue
			}

			fd := int(cqe.UserData & 0xFFFFFFFF)
			isSend := (cqe.UserData>>32)&1 == 1

			if fd == s.listenFd && !isSend {
				if cqe.Res >= 0 {
					connFd := int(cqe.Res)
					_ = unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)

					if connFd < bufferCount {
						s.submitRecv(connFd)
					} else {
						_ = unix.Close(connFd)
					}

					s.submitMultishotAccept()
				}
			} else if !isSend {
				if cqe.Res > 0 {
					dataLen := int(cqe.Res)
					data := s.buffers[fd*bufferSize : fd*bufferSize+dataLen]
					s.handleRequest(fd, data)
				} else if cqe.Res <= 0 {
					_ = unix.Close(fd)
				}
			} else if isSend {
				if cqe.Res < 0 {
					_ = unix.Close(fd)
				} else if fd < bufferCount {
					s.submitRecv(fd)
				}
			}

			*s.cqHead = head + 1
		}
	}
}

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
	bufferCount = 16384 // Process up to 16k connections
	bufferSize  = 4096
	bufferGroup = 0

	UserDataBufferProvision = ^uint64(0)
)

// Pre-built HTTP/1.1 responses
var (
	responseSimple = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 13\r\nConnection: keep-alive\r\n\r\nHello, World!")
	responseJSON   = []byte(`HTTP/1.1 200 OK` + "\r\n" + `Content-Type: application/json` + "\r\n" + `Content-Length: 50` + "\r\n" + `Connection: keep-alive` + "\r\n\r\n" + `{"message":"Hello, World!","server":"iouring-h1"}`)
	responseOK     = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\nConnection: keep-alive\r\n\r\nOK")
	response404    = []byte("HTTP/1.1 404 Not Found\r\nContent-Type: text/plain\r\nContent-Length: 9\r\nConnection: keep-alive\r\n\r\nNot Found")
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
		port: port,
	}
}

// Run starts the io_uring event loop.
func (s *HTTP1Server) Run() error {
	// Allocate buffers using mmap (pinned memory)
	bufSize := bufferCount * bufferSize
	bufPtr, err := unix.Mmap(-1, 0, bufSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return fmt.Errorf("mmap buffers: %w", err)
	}
	s.buffers = (*[1 << 30]byte)(unsafe.Pointer(&bufPtr[0]))[:bufSize:bufSize]

	// Create listening socket
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

	if err := unix.Listen(listenFd, 4096); err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	// Setup io_uring
	if err := s.setupRing(); err != nil {
		return fmt.Errorf("setup ring: %w", err)
	}

	// Submit multishot accept
	log.Printf("Submitting initial Accept on fd %d", s.listenFd)
	s.submitMultishotAccept()

	// log.Printf("iouring-h1 server listening on port %s", s.port)
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
	// Use kernel-reported entry count, not hardcoded ringSize
	s.sqArray = unsafe.Slice((*uint32)(unsafe.Pointer(&sqPtr[params.SqOff.Array])), params.SqEntries)

	s.cqHead = (*uint32)(unsafe.Pointer(&cqPtr[params.CqOff.Head]))
	s.cqTail = (*uint32)(unsafe.Pointer(&cqPtr[params.CqOff.Tail]))
	s.cqMask = *((*uint32)(unsafe.Pointer(&cqPtr[params.CqOff.RingMask])))
	// CQ ring is often 2x SQ ring, use actual kernel count
	s.cqes = unsafe.Slice((*IoUringCqe)(unsafe.Pointer(&cqPtr[params.CqOff.Cqes])), params.CqEntries)

	s.sqes = unsafe.Slice((*IoUringSqe)(unsafe.Pointer(&sqePtr[0])), params.SqEntries)

	return nil
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
