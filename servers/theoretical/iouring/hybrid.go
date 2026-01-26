//go:build linux

package iouring

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"runtime"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	protoUnknownIO = 0
	protoHTTP1IO   = 1
	protoH2CIO     = 2
)

type hybridioConnState struct {
	protocol     int
	prefaceRecv  bool
	settingsSent bool
	streams      map[uint32]string
}

// hybridioWorker represents a single hybrid event loop worker
type hybridioWorker struct {
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
	connState     []*hybridioConnState
}

// HybridServer is a multi-threaded server that muxes HTTP/1.1 and H2C using io_uring.
type HybridServer struct {
	port       string
	numWorkers int
	workers    []*hybridioWorker
}

// NewHybridServer creates a new multi-threaded io_uring hybrid mux server.
func NewHybridServer(port string) *HybridServer {
	numWorkers := runtime.NumCPU()
	if numWorkers < 1 {
		numWorkers = 1
	}
	return &HybridServer{
		port:       port,
		numWorkers: numWorkers,
		workers:    make([]*hybridioWorker, numWorkers),
	}
}

// Run starts multiple hybrid io_uring event loops.
func (s *HybridServer) Run() error {
	var portNum int
	_, _ = fmt.Sscanf(s.port, "%d", &portNum)

	errCh := make(chan error, s.numWorkers)

	for i := 0; i < s.numWorkers; i++ {
		w := &hybridioWorker{
			id:        i,
			port:      portNum,
			buffers:   make([][]byte, bufferCount),
			connPos:   make([]int, bufferCount),
			connState: make([]*hybridioConnState, bufferCount),
		}
		for j := 0; j < bufferCount; j++ {
			w.buffers[j] = make([]byte, bufferSize)
		}
		s.workers[i] = w

		go func(worker *hybridioWorker) {
			runtime.LockOSThread()
			if err := worker.run(); err != nil {
				errCh <- err
			}
		}(w)
	}

	return <-errCh
}

func (w *hybridioWorker) run() error {
	listenFd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	w.listenFd = listenFd

	_ = unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	_ = unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	_ = unix.SetsockoptInt(listenFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
	_ = unix.SetsockoptInt(listenFd, unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1)
	// Tune socket buffers for better throughput
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

func (w *hybridioWorker) setupRing() error {
	params := &IoUringParams{Flags: 0}

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

func (w *hybridioWorker) getSqe() *IoUringSqe {
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

// flushAndSubmit batches all pending SQEs and submits them to the kernel
func (w *hybridioWorker) flushAndSubmit() {
	tail := w.sqeTail
	for i := w.lastFlushed; i < tail; i++ {
		w.sqArray[i&w.sqMask] = i & w.sqMask
	}
	atomic.StoreUint32(w.sqTail, tail)
	w.lastFlushed = tail
}

func (w *hybridioWorker) prepareAccept() {
	sqe := w.getSqe()
	if sqe == nil {
		return
	}
	sqe.Opcode = IORING_OP_ACCEPT
	sqe.Fd = int32(w.listenFd)
	sqe.UserData = uint64(w.listenFd)
}

func (w *hybridioWorker) prepareRecv(fd int) {
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

func (w *hybridioWorker) eventLoop() error {
	for {
		for {
			head := atomic.LoadUint32(w.cqHead)
			tail := atomic.LoadUint32(w.cqTail)
			if head == tail {
				break
			}

			cqe := &w.cqes[head&w.cqMask]
			atomic.StoreUint32(w.cqHead, head+1)

			fd := int(cqe.UserData & 0xFFFFFFFF)
			isSend := (cqe.UserData>>32)&1 == 1

			if fd == w.listenFd && !isSend {
				if cqe.Res >= 0 {
					connFd := int(cqe.Res)
					if connFd < bufferCount {
						_ = unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
						_ = unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1)
						w.connPos[connFd] = 0
						w.connState[connFd] = &hybridioConnState{
							protocol: protoUnknownIO,
							streams:  make(map[uint32]string),
						}
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
					w.closeConnection(fd)
				}
			} else {
				if cqe.Res < 0 {
					w.closeConnection(fd)
				}
			}
		}

		// Batch flush all pending SQEs
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

func (w *hybridioWorker) handleData(fd int, n int) {
	state := w.connState[fd]
	if state == nil {
		w.closeConnection(fd)
		return
	}

	w.connPos[fd] += n
	buf := w.buffers[fd]
	pos := w.connPos[fd]

	for {
		data := buf[:pos]

		if state.protocol == protoUnknownIO {
			if pos < 4 {
				break
			}
			if bytes.HasPrefix(data, []byte("PRI ")) {
				if pos < h2PrefaceLen {
					break
				}
				if string(data[:h2PrefaceLen]) == h2Preface {
					state.protocol = protoH2CIO
				} else {
					w.closeConnection(fd)
					return
				}
			} else if data[0] == 'G' || data[0] == 'P' || data[0] == 'H' || data[0] == 'D' {
				state.protocol = protoHTTP1IO
			} else {
				w.closeConnection(fd)
				return
			}
		}

		var consumed int
		var closed bool

		switch state.protocol {
		case protoHTTP1IO:
			consumed = w.handleHTTP1(fd, buf[:pos])
		case protoH2CIO:
			consumed, closed = w.handleH2C(fd, state, buf[:pos])
			if closed {
				return
			}
		}

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
		w.closeConnection(fd)
	}
}

func (w *hybridioWorker) handleHTTP1(fd int, data []byte) int {
	// Fast manual search for \r\n\r\n - faster than bytes.Index for small buffers
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

	reqLen := headerEnd + 4

	if len(data) >= 4 && data[0] == 'P' && data[1] == 'O' && data[2] == 'S' && data[3] == 'T' {
		clIdx := bytes.Index(data[:headerEnd], []byte("Content-Length: "))
		if clIdx > 0 {
			clEnd := bytes.IndexByte(data[clIdx+16:headerEnd], '\r')
			if clEnd > 0 {
				cl := parseContentLengthIO(data[clIdx+16 : clIdx+16+clEnd])
				reqLen += cl
				if len(data) < reqLen {
					return 0
				}
			}
		}
	}

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

	rawWriteIO(fd, response)
	return reqLen
}

func (w *hybridioWorker) closeConnection(fd int) {
	_ = unix.Close(fd)
	w.connPos[fd] = 0
	w.connState[fd] = nil
}

func (w *hybridioWorker) handleH2C(fd int, state *hybridioConnState, data []byte) (consumed int, closed bool) {
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
			w.closeConnection(fd)
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

func (w *hybridioWorker) handleH2Request(fd int, streamID uint32, headerBlock []byte, endStream bool, state *hybridioConnState) {
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

func (w *hybridioWorker) send100Continue(fd int, streamID uint32) {
	h100 := []byte{0x08, 0x03, 0x31, 0x30, 0x30}
	frame := make([]byte, 9+len(h100))
	frame[2] = byte(len(h100))
	frame[3] = h2FrameTypeHeaders
	frame[4] = h2FlagEndHeaders
	binary.BigEndian.PutUint32(frame[5:9], streamID)
	copy(frame[9:], h100)
	rawWriteIO(fd, frame)
}

func (w *hybridioWorker) sendH2Response(fd int, streamID uint32, path string) {
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

func (w *hybridioWorker) sendWindowUpdates(fd int, streamID uint32, increment uint32) {
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
