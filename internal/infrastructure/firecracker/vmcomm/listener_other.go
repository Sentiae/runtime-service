//go:build !linux

package vmcomm

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"

	"github.com/google/uuid"
)

// Listener is a TCP-based fallback for non-Linux platforms (macOS, etc.)
// where AF_VSOCK is not available. It listens on localhost:<port> so the
// guest agent's TCP fallback mode can connect during development.
type Listener struct {
	ln      net.Listener
	port    uint32
	mu      sync.Mutex
	closed  bool
	clients map[uuid.UUID]*Client
}

// NewListener creates a TCP listener on localhost:<port>.
// This is the development fallback; production uses vsock (Linux).
func NewListener(port uint32) (*Listener, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen tcp %s: %w", addr, err)
	}

	log.Printf("vmcomm: listening on TCP %s (vsock fallback)", addr)
	return &Listener{
		ln:      ln,
		port:    port,
		clients: make(map[uuid.UUID]*Client),
	}, nil
}

// Accept blocks until a guest agent connects and returns a Client for the
// new connection. On non-Linux platforms the CID is always 0 since TCP
// does not have vsock context IDs.
func (l *Listener) Accept(ctx context.Context, vmID uuid.UUID) (*Client, error) {
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	ch := make(chan acceptResult, 1)

	go func() {
		conn, err := l.ln.Accept()
		ch <- acceptResult{conn, err}
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("accept: %w", ctx.Err())
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("accept tcp: %w", r.err)
		}

		client := newClient(r.conn, vmID, 0)
		l.mu.Lock()
		l.clients[vmID] = client
		l.mu.Unlock()

		log.Printf("vmcomm: accepted TCP connection from %s (VM %s)", r.conn.RemoteAddr(), vmID)
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

	return l.ln.Close()
}
