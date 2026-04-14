package vmcomm

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// FirecrackerListener manages vsock connections via Firecracker's UDS-based vsock.
// Each VM has its own UDS socket file (created by Firecracker at the configured uds_path).
// The host connects to the UDS to communicate with the guest agent.
type FirecrackerListener struct {
	mu      sync.Mutex
	clients map[uuid.UUID]*Client
}

// NewFirecrackerListener creates a listener for Firecracker vsock UDS connections.
func NewFirecrackerListener() *FirecrackerListener {
	return &FirecrackerListener{
		clients: make(map[uuid.UUID]*Client),
	}
}

// ConnectToVM connects to a Firecracker VM's vsock UDS socket and performs
// the agent handshake. The UDS path is typically <socket_path>.vsock_52
// (Firecracker appends _<port> to the uds_path).
func (l *FirecrackerListener) ConnectToVM(ctx context.Context, vmID uuid.UUID, socketPath string) (*Client, error) {
	// Firecracker vsock UDS is at <socket_path>.vsock (configured via /vsock API)
	udsPath := socketPath + ".vsock"

	log.Printf("vmcomm: connecting to VM %s vsock at %s", vmID, udsPath)

	// Wait for the UDS file to appear (VM needs to boot first)
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}

	var conn net.Conn
	var err error
	for time.Now().Before(deadline) {
		conn, err = net.DialTimeout("unix", udsPath, 2*time.Second)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
			// Retry
		}
	}
	if err != nil {
		return nil, fmt.Errorf("connect to vsock UDS %s: %w", udsPath, err)
	}

	// Firecracker vsock handshake with retry.
	// The guest agent needs time to start and listen on the vsock port.
	// We keep reconnecting + sending CONNECT until we get OK.
	connectMsg := fmt.Sprintf("CONNECT %d\n", AgentPort)
	var response string
	for time.Now().Before(deadline) {
		// Ensure we have a connection
		if conn == nil {
			time.Sleep(500 * time.Millisecond)
			conn, _ = net.DialTimeout("unix", udsPath, 2*time.Second)
			continue
		}

		_, werr := conn.Write([]byte(connectMsg))
		if werr != nil {
			conn.Close()
			conn = nil
			continue
		}

		// Read exactly one line (the OK response), leaving remaining
		// bytes (protobuf Ready message) in the buffer for WaitReady.
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		reader := bufio.NewReader(conn)
		line, rerr := reader.ReadString('\n')
		if rerr != nil {
			conn.Close()
			conn = nil
			continue
		}
		conn.SetReadDeadline(time.Time{})

		response = strings.TrimSpace(line)
		if strings.HasPrefix(response, "OK") {
			// Wrap conn with the buffered reader so remaining bytes
			// (the Ready message) aren't lost
			conn = newBufferedConn(conn, reader)
			break
		}
		conn.Close()
		conn = nil
	}
	if len(response) < 2 || response[:2] != "OK" {
		if conn != nil {
			conn.Close()
		}
		return nil, fmt.Errorf("vsock handshake timed out for VM %s", vmID)
	}

	log.Printf("vmcomm: vsock handshake complete for VM %s (%s)", vmID, strings.TrimSpace(response))

	client := newClient(conn, vmID, 0)

	l.mu.Lock()
	l.clients[vmID] = client
	l.mu.Unlock()

	return client, nil
}

// GetClient returns the connected client for a VM.
func (l *FirecrackerListener) GetClient(vmID uuid.UUID) *Client {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.clients[vmID]
}

// RemoveClient disconnects and removes a VM's client.
func (l *FirecrackerListener) RemoveClient(vmID uuid.UUID) {
	l.mu.Lock()
	client, ok := l.clients[vmID]
	if ok {
		delete(l.clients, vmID)
	}
	l.mu.Unlock()

	if client != nil {
		_ = client.Close()
	}
}

// Close closes all connections.
func (l *FirecrackerListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, client := range l.clients {
		_ = client.Close()
		delete(l.clients, id)
	}
	return nil
}
