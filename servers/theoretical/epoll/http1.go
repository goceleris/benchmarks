//go:build linux

// Package epoll provides barebones HTTP servers using raw epoll for maximum throughput.
// These implementations use multiple worker threads with SO_REUSEPORT for true parallelism.
// Connection limits and buffer sizes scale automatically based on available CPUs.
//
// Performance Optimizations:
// - SO_REUSEPORT: Enables kernel-level load balancing across multiple accept threads
// - TCP_NODELAY: Disables Nagle's algorithm for lower latency
// - TCP_QUICKACK: Forces immediate ACKs instead of delayed ACKs
// - SO_RCVBUF/SO_SNDBUF: Larger socket buffers for higher throughput
// - SO_BUSY_POLL: Reduces latency by busy-polling the socket
// - Pre-allocated buffers: Zero-allocation request handling
// - sync.Pool: Efficient buffer recycling for dynamic allocations
// - EPOLLET (edge-triggered): More efficient event notification
package epoll

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Scaling constants - these scale based on CPU count
const (
	maxEventsBase  = 1024   // Base epoll_wait batch size
	maxConnsPerCPU = 512    // Connections per CPU per worker
	readBufSize    = 65536  // Per-connection read buffer (64KB for large POST bodies)
	maxConnsMin    = 1024   // Minimum connections per worker
	maxConnsMax    = 65536  // Maximum connections per worker (fd limit)
	socketBufSize  = 262144 // Socket buffer size (256KB for higher throughput)
	busyPollUsec   = 50     // Busy poll timeout in microseconds (reduces latency)
)

// bufferPool provides efficient recycling of byte buffers for dynamic allocations
// This reduces GC pressure during high-throughput scenarios
var bufferPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, readBufSize)
		return &buf
	},
}

// getScaledLimits returns connection limits scaled to available CPUs
func getScaledLimits() (maxEvents int, maxConns int) {
	numCPU := runtime.NumCPU()

	// Scale max events with CPU count (more CPUs = more events to process)
	maxEvents = maxEventsBase * numCPU
	if maxEvents > 8192 {
		maxEvents = 8192
	}

	// Scale max connections per worker
	// On large machines, each worker handles more connections
	maxConns = maxConnsPerCPU * numCPU
	if maxConns < maxConnsMin {
		maxConns = maxConnsMin
	}
	if maxConns > maxConnsMax {
		maxConns = maxConnsMax
	}

	return maxEvents, maxConns
}

// HTTP/1.1 response templates (pre-formatted for zero-allocation)
var (
	responseSimple = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 13\r\nConnection: keep-alive\r\n\r\nHello, World!")
	responseJSON   = []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 47\r\nConnection: keep-alive\r\n\r\n{\"message\":\"Hello, World!\",\"server\":\"epoll-h1\"}")
	responseOK     = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\nConnection: keep-alive\r\n\r\nOK")
	response404    = []byte("HTTP/1.1 404 Not Found\r\nContent-Type: text/plain\r\nContent-Length: 9\r\nConnection: keep-alive\r\n\r\nNot Found")
	responseUser   = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 13\r\nConnection: keep-alive\r\n\r\nHello, World!")
)

// epollWorker represents a single event loop worker
type epollWorker struct {
	id        int
	port      int
	epollFd   int
	listenFd  int
	connBuf   [][]byte
	connPos   []int
	maxEvents int
	maxConns  int
}

// HTTP1Server is a multi-threaded HTTP/1.1 server using raw epoll.
type HTTP1Server struct {
	port       string
	numWorkers int
	workers    []*epollWorker
	maxEvents  int
	maxConns   int
}

// NewHTTP1Server creates a new multi-threaded epoll HTTP/1.1 server.
// Connection limits scale automatically based on available CPUs.
func NewHTTP1Server(port string) *HTTP1Server {
	numWorkers := runtime.NumCPU()
	if numWorkers < 1 {
		numWorkers = 1
	}

	maxEvents, maxConns := getScaledLimits()

	return &HTTP1Server{
		port:       port,
		numWorkers: numWorkers,
		workers:    make([]*epollWorker, numWorkers),
		maxEvents:  maxEvents,
		maxConns:   maxConns,
	}
}

// Run starts multiple epoll event loops.
func (s *HTTP1Server) Run() error {
	var portNum int
	_, _ = fmt.Sscanf(s.port, "%d", &portNum)

	errCh := make(chan error, s.numWorkers)

	for i := 0; i < s.numWorkers; i++ {
		w := &epollWorker{
			id:        i,
			port:      portNum,
			connBuf:   make([][]byte, s.maxConns),
			connPos:   make([]int, s.maxConns),
			maxEvents: s.maxEvents,
			maxConns:  s.maxConns,
		}
		// Pre-allocate buffers for this worker
		for j := 0; j < s.maxConns; j++ {
			w.connBuf[j] = make([]byte, readBufSize)
		}
		s.workers[i] = w

		go func(worker *epollWorker) {
			runtime.LockOSThread()
			if err := worker.run(); err != nil {
				errCh <- err
			}
		}(w)
	}

	log.Printf("epoll-h1 server listening on port %s with %d workers (maxEvents=%d, maxConns=%d per worker)",
		s.port, s.numWorkers, s.maxEvents, s.maxConns)

	// Wait for any worker to fail
	return <-errCh
}

func (w *epollWorker) run() error {
	// Each worker creates its own listening socket with SO_REUSEPORT
	listenFd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	w.listenFd = listenFd

	// SO_REUSEADDR: Allow reuse of local addresses for faster server restarts
	_ = unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)

	// SO_REUSEPORT: Enable kernel load balancing across multiple accept threads
	// Each worker creates its own listening socket, and the kernel distributes
	// incoming connections across all workers automatically
	_ = unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)

	// TCP_NODELAY: Disable Nagle's algorithm to reduce latency
	// Sends data immediately without waiting to coalesce small packets
	_ = unix.SetsockoptInt(listenFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)

	// TCP_QUICKACK: Disable delayed ACKs for lower latency
	// Forces immediate acknowledgment of received data
	_ = unix.SetsockoptInt(listenFd, unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1)

	// SO_RCVBUF/SO_SNDBUF: Larger socket buffers for higher throughput
	// 256KB buffers allow more data in flight and reduce syscall overhead
	_ = unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_RCVBUF, socketBufSize)
	_ = unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_SNDBUF, socketBufSize)

	// SO_BUSY_POLL: Enable busy polling for reduced latency
	// Kernel will busy-wait for incoming packets instead of sleeping
	// This trades CPU cycles for lower latency - beneficial under high load
	_ = unix.SetsockoptInt(listenFd, unix.SOL_SOCKET, unix.SO_BUSY_POLL, busyPollUsec)

	addr := &unix.SockaddrInet4{Port: w.port}
	if err := unix.Bind(listenFd, addr); err != nil {
		return fmt.Errorf("bind: %w", err)
	}

	if err := unix.Listen(listenFd, 65535); err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	epollFd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return fmt.Errorf("epoll_create1: %w", err)
	}
	w.epollFd = epollFd

	event := &unix.EpollEvent{
		Events: unix.EPOLLIN,
		Fd:     int32(listenFd),
	}
	if err := unix.EpollCtl(epollFd, unix.EPOLL_CTL_ADD, listenFd, event); err != nil {
		return fmt.Errorf("epoll_ctl add listen: %w", err)
	}

	return w.eventLoop()
}

func (w *epollWorker) eventLoop() error {
	events := make([]unix.EpollEvent, w.maxEvents)

	for {
		n, err := unix.EpollWait(w.epollFd, events, -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return fmt.Errorf("epoll_wait: %w", err)
		}

		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)

			if fd == w.listenFd {
				w.acceptConnections()
			} else if events[i].Events&unix.EPOLLIN != 0 {
				w.handleRead(fd)
			} else if events[i].Events&(unix.EPOLLERR|unix.EPOLLHUP) != 0 {
				w.closeConnection(fd)
			}
		}
	}
}

func (w *epollWorker) acceptConnections() {
	for {
		connFd, _, err := unix.Accept4(w.listenFd, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				break
			}
			continue
		}

		if connFd >= w.maxConns {
			_ = unix.Close(connFd)
			continue
		}

		// Apply TCP optimizations to accepted connection
		// TCP_NODELAY: Disable Nagle's algorithm for immediate sends
		_ = unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)

		// TCP_QUICKACK: Force immediate ACKs for lower latency
		_ = unix.SetsockoptInt(connFd, unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1)

		// Larger socket buffers for accepted connections
		_ = unix.SetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_RCVBUF, socketBufSize)
		_ = unix.SetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_SNDBUF, socketBufSize)

		// SO_BUSY_POLL on connection socket for lower latency
		_ = unix.SetsockoptInt(connFd, unix.SOL_SOCKET, unix.SO_BUSY_POLL, busyPollUsec)

		// Register with epoll using edge-triggered mode (EPOLLET)
		// Edge-triggered is more efficient as it only notifies once per state change
		event := &unix.EpollEvent{
			Events: unix.EPOLLIN | unix.EPOLLET,
			Fd:     int32(connFd),
		}
		_ = unix.EpollCtl(w.epollFd, unix.EPOLL_CTL_ADD, connFd, event)

		w.connPos[connFd] = 0
	}
}

func (w *epollWorker) handleRead(fd int) {
	buf := w.connBuf[fd]
	pos := w.connPos[fd]

	for {
		n, err := unix.Read(fd, buf[pos:])
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				break
			}
			w.closeConnection(fd)
			return
		}
		if n == 0 {
			w.closeConnection(fd)
			return
		}
		pos += n

		// Process all complete requests (pipelining support)
		for {
			consumed := processHTTP1Request(fd, buf[:pos])
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
	}

	w.connPos[fd] = pos
}

func processHTTP1Request(fd int, data []byte) int {
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

	// Check for POST with body
	if len(data) >= 4 && data[0] == 'P' && data[1] == 'O' && data[2] == 'S' && data[3] == 'T' {
		clIdx := bytes.Index(data[:headerEnd], []byte("Content-Length: "))
		if clIdx > 0 {
			// Search for \r in the rest of the data (not limited to headerEnd, since
			// headerEnd points to the final \r\n\r\n, not the header line's \r\n)
			clEnd := bytes.IndexByte(data[clIdx+16:], '\r')
			if clEnd > 0 {
				cl := parseContentLength(data[clIdx+16 : clIdx+16+clEnd])
				requestLen += cl
				if len(data) < requestLen {
					return 0
				}
			}
		}
	}

	// Ultra-fast path matching
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

	rawWrite(fd, response)
	return requestLen
}

func parseContentLength(b []byte) int {
	n := 0
	for _, c := range b {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

func rawWrite(fd int, data []byte) {
	if len(data) == 0 {
		return
	}
	_, _, _ = syscall.Syscall(syscall.SYS_WRITE,
		uintptr(fd),
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)))
}

func (w *epollWorker) closeConnection(fd int) {
	_ = unix.EpollCtl(w.epollFd, unix.EPOLL_CTL_DEL, fd, nil)
	_ = unix.Close(fd)
	w.connPos[fd] = 0
}

// ListenAddr returns the server's listen address.
func (s *HTTP1Server) ListenAddr() net.Addr {
	return nil
}
