//go:build linux

package vmcomm

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"syscall"
	"unsafe"

	"github.com/google/uuid"
)

const (
	// AF_VSOCK is the address family for vsock.
	afVSOCK = 40
	// VMADDR_CID_ANY accepts connections from any guest CID.
	vmaddrCIDAny = 0xFFFFFFFF
)

// sockaddrVM mirrors the C struct sockaddr_vm.
type sockaddrVM struct {
	family    uint16
	reserved1 uint16
	port      uint32
	cid       uint32
	flags     uint8
	zero      [3]uint8 // padding
}

// Listener accepts vsock connections from guest agents inside Firecracker
// microVMs. Call Accept to get a Client for each connecting VM.
//
// On Linux this uses real AF_VSOCK sockets. For development on other
// platforms, see listener_other.go which provides a TCP fallback.
type Listener struct {
	fd   int
	port uint32

	mu      sync.Mutex
	closed  bool
	clients map[uuid.UUID]*Client
}

// NewListener creates a vsock listener bound to the given port.
// The port should match the guest agent's connection port (AgentPort = 52).
func NewListener(port uint32) (*Listener, error) {
	fd, err := syscall.Socket(afVSOCK, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("create vsock socket: %w", err)
	}

	addr := sockaddrVM{
		family: afVSOCK,
		port:   port,
		cid:    vmaddrCIDAny,
	}

	_, _, errno := syscall.Syscall(
		syscall.SYS_BIND,
		uintptr(fd),
		uintptr(unsafe.Pointer(&addr)),
		unsafe.Sizeof(addr),
	)
	if errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("bind vsock port %d: %w", port, errno)
	}

	if err := syscall.Listen(fd, 128); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("listen vsock: %w", err)
	}

	log.Printf("vmcomm: listening on vsock port %d", port)
	return &Listener{
		fd:      fd,
		port:    port,
		clients: make(map[uuid.UUID]*Client),
	}, nil
}

// Accept blocks until a guest agent connects and returns a Client for the
// new connection. The vmID should be the UUID of the Firecracker VM whose
// agent is expected to connect.
//
// Callers typically run Accept in a goroutine right after booting the VM:
//
//	go func() {
//	    client, err := listener.Accept(ctx, vmID)
//	    ...
//	}()
func (l *Listener) Accept(ctx context.Context, vmID uuid.UUID) (*Client, error) {
	// Use a goroutine + channel so we can respect context cancellation.
	type acceptResult struct {
		fd  int
		cid uint32
		err error
	}
	ch := make(chan acceptResult, 1)

	go func() {
		var peerAddr sockaddrVM
		addrLen := uint32(unsafe.Sizeof(peerAddr))

		nfd, _, errno := syscall.Syscall(
			syscall.SYS_ACCEPT,
			uintptr(l.fd),
			uintptr(unsafe.Pointer(&peerAddr)),
			uintptr(unsafe.Pointer(&addrLen)),
		)
		if errno != 0 {
			ch <- acceptResult{err: fmt.Errorf("accept vsock: %w", errno)}
			return
		}
		ch <- acceptResult{fd: int(nfd), cid: peerAddr.cid}
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("accept: %w", ctx.Err())
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}

		// Wrap the raw fd in a net.Conn via net.FileConn.
		f := fdToFile(r.fd)
		conn, err := net.FileConn(f)
		f.Close() // FileConn dups the fd
		if err != nil {
			syscall.Close(r.fd)
			return nil, fmt.Errorf("fileconn: %w", err)
		}

		client := newClient(conn, vmID, r.cid)
		l.mu.Lock()
		l.clients[vmID] = client
		l.mu.Unlock()

		log.Printf("vmcomm: accepted connection from CID %d (VM %s)", r.cid, vmID)
		return client, nil
	}
}

// GetClient returns the Client associated with a VM, or nil if not connected.
func (l *Listener) GetClient(vmID uuid.UUID) *Client {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.clients[vmID]
}

// RemoveClient removes and closes the client for a VM.
func (l *Listener) RemoveClient(vmID uuid.UUID) {
	l.mu.Lock()
	client, ok := l.clients[vmID]
	if ok {
		delete(l.clients, vmID)
	}
	l.mu.Unlock()
	if ok && client != nil {
		_ = client.Close()
	}
}

// Close closes the listener and all active client connections.
func (l *Listener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return nil
	}
	l.closed = true

	for id, client := range l.clients {
		_ = client.Close()
		delete(l.clients, id)
	}

	return syscall.Close(l.fd)
}
