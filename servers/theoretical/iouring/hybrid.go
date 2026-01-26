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
	sqeTail       uint32 // Local tracking
	submittedTail uint32 // Track submitted entries
	sqes          []IoUringSqe
	cqes          []IoUringCqe
	sqArray       []uint32
	buffers       []byte
	connState     map[int]*hybridioConnState
}

type hybridioConnState struct {
	protocol        int // 0=unknown, 1=HTTP/1.1, 2=H2C
	prefaceRecv     bool
	settingsSent    bool
	inflightBuffers [][]byte

	pos int // Current buffer position
	// Stream tracking - use map for proper H2 multiplexing
	streams map[uint32]string
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

	// Immediate submit removed for batching
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

	// Immediate submit removed for batching
}

func (s *HybridServer) eventLoop() error {
	for {
		// Process all available CQEs first
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
					s.connState[connFd] = &hybridioConnState{
					protocol: protoUnknownIO,
					pos:      0,
					streams:  make(map[uint32]string),
				}

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
						// Process all complete requests in buffer (pipelining fix)
						for {
							data := s.buffers[fd*bufferSize : fd*bufferSize+state.pos]
							consumed, closed := s.handleData(fd, data)

							if closed {
								_ = unix.Close(fd)
								delete(s.connState, fd)
								state = nil
								break
							}

							if consumed > 0 {
								copy(s.buffers[fd*bufferSize:], s.buffers[fd*bufferSize+consumed:fd*bufferSize+state.pos])
								state.pos -= consumed
								// Continue to process next request in buffer
							} else {
								// Not enough data for a complete request
								break
							}
						}

						if state != nil {
							if state.pos >= bufferSize {
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
					// Do NOT submitRecv here. We keep the read loop active via Read Completion.
					// This avoids double-submission race conditions.
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

func (s *HybridServer) handleData(fd int, data []byte) (consumed int, closed bool) {
	state := s.connState[fd]
	if state == nil {
		return 0, true
	}

	if state.protocol == protoUnknownIO {
		// Fast check for HTTP/2 Preface (PRI * ...)
		if len(data) >= h2PrefaceLen && bytes.Equal(data[:h2PrefaceLen], []byte(h2Preface)) {
			state.protocol = protoH2CIO
		} else if len(data) > 0 {
			// Infer HTTP/1.1 from method
			// Check "GET ", "POST", "HEAD" etc.
			// Simple check: Is it ASCII? Just default to HTTP/1.1 for now if H2 check fails
			// and let the handler validate.
			// But maintain Upgrade check logic just in case.
			if bytes.Contains(data, []byte("Upgrade: h2c")) {
				state.protocol = protoH2CIO
				s.handleH2CUpgrade(fd, state)
				return len(data), false
			}
			state.protocol = protoHTTP1IO
		} else {
			return 0, false
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
				// Send settings ACK
				s.submitSend(fd, frameSettingsAck)
			}
		case h2FrameTypeHeaders:
			// Parse minimal headers
			endStream := flags&h2FlagEndStream != 0
			s.handleH2Request(fd, streamID, frameData, flags, endStream)

		case h2FrameTypeData:
			// Consume data and send Window Updates (coalesced)
			if length > 0 {
				s.submitWindowUpdates(fd, streamID, uint32(length))
			}

			endStream := flags&h2FlagEndStream != 0
			if endStream {
				connState := s.connState[fd]
				if connState != nil {
					if path, ok := connState.streams[streamID]; ok {
						s.sendH2Response(fd, streamID, path)
						delete(connState.streams, streamID)
					}
				}
			}
		}

		offset += totalFrameLen
	}

	return offset, false
}

func (s *HybridServer) handleH2Request(fd int, streamID uint32, headerBlock []byte, flags byte, endStream bool) {
	// HPACK path detection
	var path string

	// ASCII literal check first
	// Note: We skip complex Huffman length checks here for brevity in Hybrid, assume simple routing or basic literals
	if bytes.Contains(headerBlock, []byte("/json")) {
		path = "/json"
	} else if bytes.Contains(headerBlock, []byte("/users/")) {
		path = "/users/123"
	} else if bytes.Contains(headerBlock, []byte("/upload")) {
		path = "/upload"
	} else {
		// Check for :path header (index 4) with Huffman-encoded value
		// Look for name index 4 (:path) or 5 (:path)
		// Masks:
		// - 0x80 (Indexed) -> 1xxxxxxx. Index = val & 0x7f. (Check 4 or 5)
		// - 0x40 (Literal Incr Enum) -> 01xxxxxx. Index = val & 0x3f. (Check 4 or 5)
		// - 0x00/0x10 (Literal No/Never Index) -> 00xxxxxx. Index = val & 0x0f. (Check 4 or 5)

		for i := 0; i < len(headerBlock)-1; i++ {
			b := headerBlock[i]
			var idx int
			isMatch := false

			if b&0x80 != 0 {
				// Indexed field - skip (no value) unless 0x84/0x85 (default path)
				if b == 0x84 {
					if path == "" {
						path = "/"
					}
				}
				continue
			} else if b&0xC0 == 0x40 {
				// Literal with Incremental Indexing (01xxxxxx)
				idx = int(b & 0x3F)
				if idx == 4 || idx == 5 {
					isMatch = true
				}
			} else if b&0xF0 == 0x00 || b&0xF0 == 0x10 {
				// Literal without Indexing (0000xxxx) or Never Indexed (0001xxxx)
				idx = int(b & 0x0F)
				if idx == 4 || idx == 5 {
					isMatch = true
				}
			}

			if isMatch {
				// Next byte is the length (possibly with Huffman bit)
				if i+1 < len(headerBlock) {
					length := int(headerBlock[i+1] & 0x7f)
					huffman := (headerBlock[i+1] & 0x80) != 0

					if i+2+length <= len(headerBlock) {
						pathBytes := headerBlock[i+2 : i+2+length]
						if huffman {
							// Huffman encoded matching
							// /json = 63 a2 0f 57 (curl/Go?) or 63 8d 31 69
							if length == 4 && (bytes.Equal(pathBytes, []byte{0x63, 0x8d, 0x31, 0x69}) || bytes.Equal(pathBytes, []byte{0x63, 0xa2, 0x0f, 0x57})) {
								path = "/json"
							} else if length == 5 && bytes.Equal(pathBytes, []byte{0x62, 0xda, 0xe8, 0x38, 0xe4}) {
								path = "/upload"
							}
						}
					}
				}
			}
		}
	}

	if path == "" {
		path = "/"
	}

	if endStream {
		s.sendH2Response(fd, streamID, path)
	} else {
		if state, ok := s.connState[fd]; ok {
			// Store in streams map for proper multiplexing
			state.streams[streamID] = path

			// For upload, send 100-continue for Go clients
			if path == "/upload" {
				s.send100Continue(fd, streamID)
			}
		}
	}
}

func (s *HybridServer) send100Continue(fd int, streamID uint32) {
	// :status 100 -> 0x08 0x03 "100"
	h100 := []byte{0x08, 0x03, 0x31, 0x30, 0x30}

	frame := make([]byte, 9+len(h100))
	frame[0] = 0
	frame[1] = 0
	frame[2] = byte(len(h100))
	frame[3] = h2FrameTypeHeaders
	frame[4] = h2FlagEndHeaders
	binary.BigEndian.PutUint32(frame[5:9], streamID)
	copy(frame[9:], h100)

	s.submitSend(fd, frame)
}

func (s *HybridServer) sendH2Response(fd int, streamID uint32, path string) {
	var headerBytes, dataBytes []byte

	if path == "/json" {
		headerBytes = hpackHeadersJSON
		dataBytes = bodyJSON
	} else if path == "/upload" {
		headerBytes = hpackHeadersSimple
		dataBytes = bodyOK
	} else {
		headerBytes = hpackHeadersSimple
		dataBytes = bodySimple
	}

	// Coalesce Headers + Data frames into single buffer
	// Headers frame: 9 + len(headerBytes)
	// Data frame: 9 + len(dataBytes)
	totalLen := 9 + len(headerBytes) + 9 + len(dataBytes)
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
	dataOffset := 9 + len(headerBytes)
	frame[dataOffset] = byte(len(dataBytes) >> 16)
	frame[dataOffset+1] = byte(len(dataBytes) >> 8)
	frame[dataOffset+2] = byte(len(dataBytes))
	frame[dataOffset+3] = h2FrameTypeData
	frame[dataOffset+4] = h2FlagEndStream
	binary.BigEndian.PutUint32(frame[dataOffset+5:dataOffset+9], streamID)
	copy(frame[dataOffset+9:], dataBytes)

	s.submitSend(fd, frame)
}

// submitWindowUpdates sends both connection and stream window updates in a single buffer
func (s *HybridServer) submitWindowUpdates(fd int, streamID uint32, increment uint32) {
	// Two WINDOW_UPDATE frames coalesced: connection (stream 0) + stream
	// Each frame: 9 bytes header + 4 bytes payload = 13 bytes
	// Total: 26 bytes for both
	frame := make([]byte, 26)

	// Connection window update (stream 0)
	frame[0] = 0x00
	frame[1] = 0x00
	frame[2] = 0x04 // Length 4
	frame[3] = 0x08 // Type WINDOW_UPDATE
	frame[4] = 0x00 // Flags
	// frame[5:9] = 0 (stream 0)
	binary.BigEndian.PutUint32(frame[9:13], increment)

	// Stream window update
	frame[13] = 0x00
	frame[14] = 0x00
	frame[15] = 0x04 // Length 4
	frame[16] = 0x08 // Type WINDOW_UPDATE
	frame[17] = 0x00 // Flags
	binary.BigEndian.PutUint32(frame[18:22], streamID)
	binary.BigEndian.PutUint32(frame[22:26], increment)

	s.submitSend(fd, frame)
}
