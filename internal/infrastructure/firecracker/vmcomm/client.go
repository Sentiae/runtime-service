package vmcomm

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	pb "github.com/sentiae/runtime-service/internal/infrastructure/firecracker/agentpb"
)

// Client communicates with a single guest agent over a vsock (or TCP
// fallback) connection. It is safe for concurrent use; a mutex serialises
// writes while reads happen on a dedicated goroutine.
type Client struct {
	conn   net.Conn
	mu     sync.Mutex // protects writes
	vmID   uuid.UUID
	cid    uint32
	ready  *pb.Ready
	closed chan struct{}
}

// AgentInfo contains the data sent by the agent in its Ready message.
type AgentInfo struct {
	AgentVersion       string
	Hostname           string
	SupportedLanguages []string
}

// TaskResult aggregates all output and the completion message for a task.
type TaskResult struct {
	ExitCode     int32
	Stdout       []byte
	Stderr       []byte
	Error        string
	DurationMS   int64
	CPUTimeMS    int64
	MemoryPeakKB int64
}

// newClient wraps an already-established connection. It does NOT perform
// the vsock dial itself — callers (the Listener or a test helper) provide
// the connection.
func newClient(conn net.Conn, vmID uuid.UUID, cid uint32) *Client {
	return &Client{
		conn:   conn,
		vmID:   vmID,
		cid:    cid,
		closed: make(chan struct{}),
	}
}

// WaitReady blocks until the guest agent sends its Ready message or the
// context expires.
func (c *Client) WaitReady(ctx context.Context) (*AgentInfo, error) {
	type result struct {
		ready *pb.GuestMessage
		err   error
	}
	ch := make(chan result, 1)

	go func() {
		msg := new(pb.GuestMessage)
		err := recvMessage(c.conn, msg)
		ch <- result{msg, err}
	}()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("wait ready: %w", ctx.Err())
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("wait ready: %w", r.err)
		}
		readyPayload := r.ready.GetReady()
		if readyPayload == nil {
			return nil, fmt.Errorf("expected Ready message, got %T", r.ready.GetPayload())
		}
		c.ready = readyPayload
		return &AgentInfo{
			AgentVersion:       readyPayload.GetAgentVersion(),
			Hostname:           readyPayload.GetHostname(),
			SupportedLanguages: readyPayload.GetSupportedLanguages(),
		}, nil
	}
}

// ExecuteTask sends an ExecuteTask message and collects all TaskOutput and
// the final TaskComplete response. Output is streamed to the optional
// onOutput callback (may be nil).
func (c *Client) ExecuteTask(ctx context.Context, task *pb.ExecuteTask, onOutput func(*pb.TaskOutput)) (*TaskResult, error) {
	// Send the task.
	hostMsg := &pb.HostMessage{
		Payload: &pb.HostMessage_ExecuteTask{ExecuteTask: task},
	}

	c.mu.Lock()
	err := sendMessage(c.conn, hostMsg)
	c.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("send ExecuteTask: %w", err)
	}

	// Collect output until TaskComplete.
	result := &TaskResult{}
	for {
		select {
		case <-ctx.Done():
			// Try to cancel the task before returning.
			_ = c.cancelTask(task.GetTaskId())
			return nil, fmt.Errorf("execute task: %w", ctx.Err())
		default:
		}

		// Set a per-read deadline so we can check ctx cancellation periodically.
		_ = c.conn.SetReadDeadline(time.Now().Add(1 * time.Second))

		msg := new(pb.GuestMessage)
		if err := recvMessage(c.conn, msg); err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue // retry after checking context
			}
			return nil, fmt.Errorf("receive guest message: %w", err)
		}

		// Reset the deadline after a successful read.
		_ = c.conn.SetReadDeadline(time.Time{})

		switch p := msg.GetPayload().(type) {
		case *pb.GuestMessage_TaskOutput:
			if p.TaskOutput.GetTaskId() != task.GetTaskId() {
				continue // ignore output for other tasks
			}
			switch pb.OutputStream(p.TaskOutput.GetStream()) {
			case pb.OutputStream_STDOUT:
				result.Stdout = append(result.Stdout, p.TaskOutput.GetData()...)
			case pb.OutputStream_STDERR:
				result.Stderr = append(result.Stderr, p.TaskOutput.GetData()...)
			}
			if onOutput != nil {
				onOutput(p.TaskOutput)
			}

		case *pb.GuestMessage_TaskComplete:
			if p.TaskComplete.GetTaskId() != task.GetTaskId() {
				continue
			}
			result.ExitCode = p.TaskComplete.GetExitCode()
			result.Error = p.TaskComplete.GetError()
			result.DurationMS = p.TaskComplete.GetDurationMs()
			result.CPUTimeMS = p.TaskComplete.GetCpuTimeMs()
			result.MemoryPeakKB = p.TaskComplete.GetMemoryPeakKb()
			return result, nil

		case *pb.GuestMessage_Metrics:
			// Ignore periodic metrics during task execution.
			continue

		case *pb.GuestMessage_Pong:
			continue

		default:
			log.Printf("vmcomm: unexpected guest message type %T", p)
		}
	}
}

// CancelTask sends a CancelTask message for the given task ID.
func (c *Client) CancelTask(taskID string) error {
	return c.cancelTask(taskID)
}

func (c *Client) cancelTask(taskID string) error {
	msg := &pb.HostMessage{
		Payload: &pb.HostMessage_CancelTask{
			CancelTask: &pb.CancelTask{TaskId: taskID},
		},
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return sendMessage(c.conn, msg)
}

// Ping sends a Ping and waits for the Pong response.
func (c *Client) Ping(ctx context.Context) (string, error) {
	now := time.Now().UnixMilli()
	msg := &pb.HostMessage{
		Payload: &pb.HostMessage_Ping{
			Ping: &pb.Ping{Timestamp: now},
		},
	}

	c.mu.Lock()
	err := sendMessage(c.conn, msg)
	c.mu.Unlock()
	if err != nil {
		return "", fmt.Errorf("send ping: %w", err)
	}

	// Wait for pong with a deadline.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	_ = c.conn.SetReadDeadline(deadline)
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()

	for {
		resp := new(pb.GuestMessage)
		if err := recvMessage(c.conn, resp); err != nil {
			return "", fmt.Errorf("receive pong: %w", err)
		}
		if pong := resp.GetPong(); pong != nil {
			return pong.GetStatus(), nil
		}
		// Skip non-Pong messages (metrics, etc.).
	}
}

// Shutdown sends a graceful shutdown request to the guest agent.
func (c *Client) Shutdown() error {
	msg := &pb.HostMessage{
		Payload: &pb.HostMessage_Shutdown{
			Shutdown: &pb.Shutdown{},
		},
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return sendMessage(c.conn, msg)
}

// Close shuts down the connection.
func (c *Client) Close() error {
	select {
	case <-c.closed:
		return nil // already closed
	default:
	}
	close(c.closed)
	return c.conn.Close()
}

// VMID returns the VM ID associated with this client.
func (c *Client) VMID() uuid.UUID {
	return c.vmID
}

// CID returns the vsock CID of the connected guest.
func (c *Client) CID() uint32 {
	return c.cid
}
