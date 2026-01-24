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
	port      string
	ringFd    int
	listenFd  int
	sqHead    *uint32
	sqTail    *uint32
	cqHead    *uint32
	cqTail    *uint32
	sqMask    uint32
	cqMask    uint32
	sqeTail   uint32 // Local tracking
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
	pos             int // Current buffer position
}

const (
	protoUnknownIO = 0
	protoHTTP1IO   = 1
	protoH2CIO     = 2

	// UserDataBufferProvision is defined in http1.go
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

	// Immediate submit
	_, _, _ = unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(s.ringFd), 1, 0, 0, 0, 0)
}

func (s *HybridServer) submitRecv(fd int) {
	state := s.connState[fd]
	if state == nil {
		return
	}

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
	// Read into buffer at current position
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&s.buffers[fd*bufferSize+state.pos])))
	sqe.Len = uint32(bufferSize - state.pos)
	sqe.UserData = uint64(fd)

	tail := s.sqeTail
	sqTail := atomic.LoadUint32(s.sqTail)
	for i := sqTail; i < tail; i++ {
		s.sqArray[i&s.sqMask] = i & s.sqMask
	}
	atomic.StoreUint32(s.sqTail, tail)
}

func (s *HybridServer) submitSend(fd int, data []byte) {
	if state, ok := s.connState[fd]; ok {
		state.inflightBuffers = append(state.inflightBuffers, data)
	}

	head := atomic.LoadUint32(s.sqHead)
	next := s.sqeTail + 1

	if next-head > uint32(sqeCount) {
		return
	}

	idx := s.sqeTail & s.sqMask
	sqe := &s.sqes[idx]
	s.sqeTail = next

	*sqe = IoUringSqe{}
	sqe.Opcode = IORING_OP_SEND
	sqe.Fd = int32(fd)
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&data[0])))
	sqe.Len = uint32(len(data))
	sqe.UserData = uint64(fd) | (1 << 32)

	tail := s.sqeTail
	sqTail := atomic.LoadUint32(s.sqTail)
	for i := sqTail; i < tail; i++ {
		s.sqArray[i&s.sqMask] = i & s.sqMask
	}
	atomic.StoreUint32(s.sqTail, tail)
}

func (s *HybridServer) eventLoop() error {
	for {
		// Calculate pending submissions
		sqTail := atomic.LoadUint32(s.sqTail)
		sqHead := atomic.LoadUint32(s.sqHead)
		toSubmit := sqTail - sqHead

		// Enter kernel: submit pending SQEs and wait for at least 1 CQE
		_, _, errno := unix.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(s.ringFd),
			uintptr(toSubmit), 1, IORING_ENTER_GETEVENTS, 0, 0)
		if errno != 0 && errno != unix.EINTR {
			return fmt.Errorf("io_uring_enter: %w", errno)
		}

		for {
			head := atomic.LoadUint32(s.cqHead)
			tail := atomic.LoadUint32(s.cqTail)

			if head == tail {
				break
			}

			idx := head & s.cqMask
			cqe := &s.cqes[idx]

			atomic.StoreUint32(s.cqHead, head+1)

			if cqe.UserData == UserDataBufferProvision {
				continue
			}

			fd := int(cqe.UserData & 0xFFFFFFFF)
			isSend := (cqe.UserData>>32)&1 == 1

			if fd == s.listenFd && !isSend {
				if cqe.Res >= 0 {
					connFd := int(cqe.Res)
					_ = unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
					s.connState[connFd] = &hybridioConnState{protocol: protoUnknownIO, pos: 0}

					if connFd < bufferCount {
						s.submitRecv(connFd)
					} else {
						_ = unix.Close(connFd)
					}

					s.submitAccept()
				}
			} else if !isSend {
				if cqe.Res > 0 {
					n := int(cqe.Res)
					state := s.connState[fd]
					if state != nil {
						state.pos += n
						// Process available data
						data := s.buffers[fd*bufferSize : fd*bufferSize+state.pos]
						consumed, closed := s.handleData(fd, data)

						if closed {
							// Connection closed
							_ = unix.Close(fd)
							delete(s.connState, fd)
						} else {
							// Compact buffer if needed
							if consumed > 0 {
								copy(s.buffers[fd*bufferSize:], s.buffers[fd*bufferSize+consumed:fd*bufferSize+state.pos])
								state.pos -= consumed
							}

							if state.pos >= bufferSize {
								// Overflow
								_ = unix.Close(fd)
								delete(s.connState, fd)
							} else {
								s.submitRecv(fd)
							}
						}
					}
				} else if cqe.Res <= 0 {
					_ = unix.Close(fd)
					delete(s.connState, fd)
				}
			} else if isSend {
				if state, ok := s.connState[fd]; ok && len(state.inflightBuffers) > 0 {
					state.inflightBuffers = state.inflightBuffers[1:]
				}
				if cqe.Res < 0 {
					_ = unix.Close(fd)
					delete(s.connState, fd)
				} else if fd < bufferCount {
					// Wait for next request
					s.submitRecv(fd)
				}
			}
		}

	}
}

func (s *HybridServer) handleData(fd int, data []byte) (consumed int, closed bool) {
	state := s.connState[fd]
	if state == nil {
		return 0, true
	}

	if state.protocol == protoUnknownIO {
		if len(data) >= h2PrefaceLen && string(data[:h2PrefaceLen]) == h2Preface {
			state.protocol = protoH2CIO
		} else if bytes.HasPrefix(data, []byte("GET ")) ||
			bytes.HasPrefix(data, []byte("POST ")) {
			if bytes.Contains(data, []byte("Upgrade: h2c")) {
				state.protocol = protoH2CIO
				s.handleH2CUpgrade(fd, state)
				return len(data), false
			}
			state.protocol = protoHTTP1IO
		} else {
			if bytes.Contains(data, []byte("GET /")) {
				state.protocol = protoHTTP1IO
			} else {
				_ = unix.Close(fd)
				delete(s.connState, fd)
				return 0, true
			}
		}
	}

	switch state.protocol {
	case protoHTTP1IO:
		consumed, _ := s.handleHTTP1(fd, data)
		return consumed, false
	case protoH2CIO:
		return s.handleH2C(fd, state, data)
	default:
		return 0, false
	}
}

func (s *HybridServer) handleHTTP1(fd int, data []byte) (consumed int, complete bool) {
	// Basic HTTP/1.1 parsing
	headerEnd := bytes.Index(data, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		return 0, false
	}

	reqLen := headerEnd + 4

	// Check Content-Length for POST
	if bytes.HasPrefix(data, []byte("POST")) {
		clIdx := bytes.Index(data, []byte("Content-Length: "))
		if clIdx > 0 && clIdx < headerEnd {
			clEnd := bytes.Index(data[clIdx:], []byte("\r\n"))
			if clEnd > 0 {
				var contentLen int
				_, _ = fmt.Sscanf(string(data[clIdx+16:clIdx+clEnd]), "%d", &contentLen)
				reqLen += contentLen
				if len(data) < reqLen {
					return 0, false
				}
			}
		}
	}

	var response []byte
	requestData := data[:reqLen]

	if bytes.HasPrefix(requestData, []byte("GET / ")) {
		response = responseSimple
	} else if bytes.HasPrefix(requestData, []byte("GET /json")) {
		response = responseJSON
	} else if bytes.HasPrefix(requestData, []byte("GET /users/")) {
		// Extract user ID from path
		lineEnd := bytes.Index(requestData, []byte("\r\n"))
		if lineEnd > 0 {
			path := string(requestData[11:lineEnd])
			spaceIdx := bytes.Index([]byte(path), []byte(" "))
			if spaceIdx > 0 {
				id := path[:spaceIdx]
				resp := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: keep-alive\r\n\r\nUser ID: %s", 9+len(id), id)
				response = []byte(resp)
			}
		}
		if response == nil {
			response = response404
		}
	} else if bytes.HasPrefix(requestData, []byte("POST /upload")) {
		response = responseOK
	} else {
		response = response404
	}

	s.submitSend(fd, response)
	return reqLen, true
}

func (s *HybridServer) handleH2CUpgrade(fd int, state *hybridioConnState) {
	upgradeResponse := []byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n")
	s.submitSend(fd, upgradeResponse)

	// Settings Frame
	s.submitSend(fd, frameSettings)
	state.settingsSent = true
}

func (s *HybridServer) handleH2C(fd int, state *hybridioConnState, data []byte) (consumed int, closed bool) {
	offset := 0

	if !state.prefaceRecv {
		if len(data) >= h2PrefaceLen {
			if string(data[:h2PrefaceLen]) == h2Preface {
				state.prefaceRecv = true
				offset = h2PrefaceLen

				if !state.settingsSent {
					s.submitSend(fd, frameSettings)
					state.settingsSent = true
				}
			} else {
				// Invalid preface?
				_ = unix.Close(fd)
				delete(s.connState, fd)
				return 0, true
			}
		} else {
			return 0, false
		}
	}

	for offset+h2FrameHeaderLen <= len(data) {
		length := int(data[offset])<<16 | int(data[offset+1])<<8 | int(data[offset+2])
		totalFrameLen := h2FrameHeaderLen + length

		if offset+totalFrameLen > len(data) {
			break
		}

		frameType := data[offset+3]
		flags := data[offset+4]
		streamID := binary.BigEndian.Uint32(data[offset+5:offset+9]) & 0x7fffffff

		frameData := data[offset+h2FrameHeaderLen : offset+totalFrameLen]

		switch frameType {
		case h2FrameTypeSettings:
			if flags&h2FlagAck == 0 {
				s.submitSend(fd, frameSettingsAck)
			}
		case h2FrameTypeHeaders:
			s.handleH2Request(fd, streamID, frameData, flags)
		}

		offset += totalFrameLen
	}

	return offset, false
}

func (s *HybridServer) handleH2Request(fd int, streamID uint32, headerBlock []byte, flags byte) {
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
