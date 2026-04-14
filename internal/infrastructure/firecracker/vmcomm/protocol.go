// Package vmcomm implements the host-side vsock communication with the
// Sentiae guest agent running inside Firecracker microVMs. It speaks the
// same length-prefixed protobuf framing as the Rust guest agent.
package vmcomm

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"google.golang.org/protobuf/proto"
)

const (
	// maxMessageSize is the maximum accepted message size (16 MiB), matching
	// the guest agent's limit.
	maxMessageSize = 16 * 1024 * 1024

	// AgentPort is the vsock port the guest agent connects to on the host.
	AgentPort = 52
)

// sendMessage writes a length-prefixed protobuf message to conn.
// Framing: [4-byte big-endian length][protobuf bytes]
func sendMessage(conn net.Conn, msg proto.Message) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))

	if _, err := conn.Write(lenBuf); err != nil {
		return fmt.Errorf("write length prefix: %w", err)
	}
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	return nil
}

// recvMessage reads a length-prefixed protobuf message from conn.
func recvMessage(conn net.Conn, msg proto.Message) error {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return fmt.Errorf("read length prefix: %w", err)
	}

	length := binary.BigEndian.Uint32(lenBuf)
	if length > maxMessageSize {
		return fmt.Errorf("message too large: %d bytes (max %d)", length, maxMessageSize)
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(conn, data); err != nil {
		return fmt.Errorf("read payload (%d bytes): %w", length, err)
	}

	if err := proto.Unmarshal(data, msg); err != nil {
		return fmt.Errorf("unmarshal message: %w", err)
	}
	return nil
}
