//go:build linux

package iouring

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"unsafe"

	"golang.org/x/sys/unix"
)

// HybridServer is a barebones server that muxes HTTP/1.1 and H2C using io_uring.
type HybridServer struct {
	port      string
	ringFd    int
	listenFd  int
	sqHead    *uint32
	sqTail    *uint32
	cqHead    *uint32
	cqTail    *uint32
	sqMask    uint32
	cqMask    uint32
	sqes      []IoUringSqe
	cqes      []IoUringCqe
	sqArray   []uint32
	buffers   []byte
	connState map[int]*hybridioConnState
}

type hybridioConnState struct {
	protocol        int // 0=unknown, 1=HTTP/1.1, 2=H2C
	prefaceRecv     bool
	settingsSent    bool
	inflightBuffers [][]byte
}

const (
	protoUnknownIO = 0
	protoHTTP1IO   = 1
	protoH2CIO     = 2
)

// NewHybridServer creates a new barebones io_uring hybrid mux server.
func NewHybridServer(port string) *HybridServer {
	return &HybridServer{
		port:      port,
		connState: make(map[int]*hybridioConnState),
	}
}

// Run starts the hybrid io_uring event loop.
func (s *HybridServer) Run() error {
	// Allocate buffers using mmap (pinned memory)
	bufSize := bufferCount * bufferSize
	bufPtr, err := unix.Mmap(-1, 0, bufSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return fmt.Errorf("mmap buffers: %w", err)
	}
	s.buffers = (*[1 << 30]byte)(unsafe.Pointer(&bufPtr[0]))[:bufSize:bufSize]

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

	if err := s.setupRing(); err != nil {
		return fmt.Errorf("setup ring: %w", err)
	}

	// Submit initial accept
	s.submitAccept()

	log.Printf("iouring-hybrid server listening on port %s", s.port)
	return s.eventLoop()
}

func (s *HybridServer) setupRing() error {
	params := &IoUringParams{
		Flags: IORING_SETUP_COOP_TASKRUN | IORING_SETUP_SINGLE_ISSUER,
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
	s.sqArray = (*[ringSize]uint32)(unsafe.Pointer(&sqPtr[params.SqOff.Array]))[:]

	s.cqHead = (*uint32)(unsafe.Pointer(&cqPtr[params.CqOff.Head]))
	s.cqTail = (*uint32)(unsafe.Pointer(&cqPtr[params.CqOff.Tail]))
	s.cqMask = *((*uint32)(unsafe.Pointer(&cqPtr[params.CqOff.RingMask])))
	s.cqes = (*[ringSize]IoUringCqe)(unsafe.Pointer(&cqPtr[params.CqOff.Cqes]))[:]

	s.sqes = (*[ringSize]IoUringSqe)(unsafe.Pointer(&sqePtr[0]))[:]

	return nil
}

func (s *HybridServer) submitAccept() {
	tail := *s.sqTail
	idx := tail & s.sqMask

	sqe := &s.sqes[idx]
	sqe.Opcode = IORING_OP_ACCEPT
	sqe.Fd = int32(s.listenFd)
	sqe.OpcodeFlags = 0
	sqe.UserData = uint64(s.listenFd)

	s.sqArray[idx] = idx
	*s.sqTail = tail + 1
}

func (s *HybridServer) submitRecv(fd int) {
	tail := *s.sqTail
	idx := tail & s.sqMask

	s.sqes[idx] = IoUringSqe{} // Zero out
	sqe := &s.sqes[idx]
	sqe.Opcode = IORING_OP_RECV
	sqe.Fd = int32(fd)
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&s.buffers[fd*bufferSize])))
	sqe.Len = bufferSize
	sqe.Off = 0
	sqe.Flags = 0
	sqe.OpcodeFlags = 0
	sqe.UserData = uint64(fd)

	s.sqArray[idx] = idx
	*s.sqTail = tail + 1
}

func (s *HybridServer) submitSend(fd int, data []byte) {
	if state, ok := s.connState[fd]; ok {
		state.inflightBuffers = append(state.inflightBuffers, data)
	}

	tail := *s.sqTail
	idx := tail & s.sqMask

	sqe := &s.sqes[idx]
	sqe.Opcode = IORING_OP_SEND
	sqe.Fd = int32(fd)
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&data[0])))
	sqe.Len = uint32(len(data))
	sqe.UserData = uint64(fd) | (1 << 32)

	s.sqArray[idx] = idx
	*s.sqTail = tail + 1
}

func (s *HybridServer) eventLoop() error {
	for {
		_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(s.ringFd),
			uintptr(*s.sqTail-*s.sqHead), 1, IORING_ENTER_GETEVENTS, 0, 0)
		if errno != 0 && errno != unix.EINTR {
			return fmt.Errorf("io_uring_enter: %w", errno)
		}

		for head := *s.cqHead; head != *s.cqTail; head++ {
			idx := head & s.cqMask
			cqe := &s.cqes[idx]

			fd := int(cqe.UserData & 0xFFFFFFFF)
			isSend := (cqe.UserData>>32)&1 == 1

			if fd == s.listenFd && !isSend {
				if cqe.Res >= 0 {
					connFd := int(cqe.Res)
					_ = unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
					s.connState[connFd] = &hybridioConnState{protocol: protoUnknownIO}

					// Submit first recv
					// Ensure FD is within buffer limits
					if connFd < bufferCount {
						s.submitRecv(connFd)
					} else {
						unix.Close(connFd)
					}

					// Re-arm accept
					s.submitAccept()
				}
			} else if !isSend {
				if cqe.Res > 0 {
					dataLen := int(cqe.Res)
					data := s.buffers[fd*bufferSize : fd*bufferSize+dataLen]

					s.handleData(fd, data)

					// Keep reading
					s.submitRecv(fd)
				} else if cqe.Res <= 0 {
					_ = unix.Close(fd)
					delete(s.connState, fd)
				}
			} else if isSend {
				// Send completion
				if state, ok := s.connState[fd]; ok && len(state.inflightBuffers) > 0 {
					state.inflightBuffers = state.inflightBuffers[1:]
				}
				if cqe.Res < 0 {
					_ = unix.Close(fd)
					delete(s.connState, fd)
				}
			}

			*s.cqHead = head + 1
		}
	}
}

func (s *HybridServer) handleData(fd int, data []byte) {
	state := s.connState[fd]
	if state == nil {
		return
	}

	// Protocol detection on first data
	if state.protocol == protoUnknownIO {
		if len(data) >= h2PrefaceLen && string(data[:h2PrefaceLen]) == h2Preface {
			state.protocol = protoH2CIO
		} else if bytes.HasPrefix(data, []byte("GET ")) ||
			bytes.HasPrefix(data, []byte("POST ")) {
			// Check for H2C upgrade
			if bytes.Contains(data, []byte("Upgrade: h2c")) {
				state.protocol = protoH2CIO
				s.handleH2CUpgrade(fd, state)
				return
			}
			state.protocol = protoHTTP1IO
		}
	}

	switch state.protocol {
	case protoHTTP1IO:
		s.handleHTTP1(fd, data)
	case protoH2CIO:
		s.handleH2C(fd, state, data)
	}
}

func (s *HybridServer) handleHTTP1(fd int, data []byte) {
	var response []byte

	if bytes.HasPrefix(data, []byte("GET / ")) {
		response = responseSimple
	} else if bytes.HasPrefix(data, []byte("GET /json")) {
		response = responseJSON
	} else if bytes.HasPrefix(data, []byte("POST /upload")) {
		response = responseOK
	} else {
		response = response404
	}

	s.submitSend(fd, response)
}

func (s *HybridServer) handleH2CUpgrade(fd int, state *hybridioConnState) {
	upgradeResponse := []byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n")
	s.submitSend(fd, upgradeResponse)

	// Settings Frame
	s.submitSend(fd, frameSettings)
	state.settingsSent = true
}

func (s *HybridServer) handleH2C(fd int, state *hybridioConnState, data []byte) {
	offset := 0

	if !state.prefaceRecv {
		if len(data) >= h2PrefaceLen && string(data[:h2PrefaceLen]) == h2Preface {
			state.prefaceRecv = true
			offset = h2PrefaceLen

			if !state.settingsSent {
				s.submitSend(fd, frameSettings)
				state.settingsSent = true
			}
		}
	}

	for offset+h2FrameHeaderLen <= len(data) {
		length := int(data[offset])<<16 | int(data[offset+1])<<8 | int(data[offset+2])
		frameType := data[offset+3]
		flags := data[offset+4]
		streamID := binary.BigEndian.Uint32(data[offset+5:offset+9]) & 0x7fffffff

		offset += h2FrameHeaderLen

		if offset+length > len(data) {
			break
		}

		frameData := data[offset : offset+length]
		offset += length

		switch frameType {
		case h2FrameTypeSettings:
			if flags&h2FlagAck == 0 {
				s.submitSend(fd, frameSettingsAck)
			}
		case h2FrameTypeHeaders:
			s.handleH2Request(fd, streamID, frameData)
		}
	}
}

func (s *HybridServer) handleH2Request(fd int, streamID uint32, headerBlock []byte) {
	var headerBytes, dataBytes []byte

	if bytes.Contains(headerBlock, []byte("/json")) {
		headerBytes = hpackHeadersJSON
		dataBytes = bodyJSON
	} else {
		headerBytes = hpackHeadersSimple
		dataBytes = bodySimple
	}

	headersFrame := make([]byte, 9+len(headerBytes))
	headersFrame[0] = byte(len(headerBytes) >> 16)
	headersFrame[1] = byte(len(headerBytes) >> 8)
	headersFrame[2] = byte(len(headerBytes))
	headersFrame[3] = h2FrameTypeHeaders
	headersFrame[4] = h2FlagEndHeaders
	binary.BigEndian.PutUint32(headersFrame[5:9], streamID)
	copy(headersFrame[9:], headerBytes)
	s.submitSend(fd, headersFrame)

	dataFrame := make([]byte, 9+len(dataBytes))
	dataFrame[0] = byte(len(dataBytes) >> 16)
	dataFrame[1] = byte(len(dataBytes) >> 8)
	dataFrame[2] = byte(len(dataBytes))
	dataFrame[3] = h2FrameTypeData
	dataFrame[4] = h2FlagEndStream
	binary.BigEndian.PutUint32(dataFrame[5:9], streamID)
	copy(dataFrame[9:], dataBytes)
	s.submitSend(fd, dataFrame)
}
