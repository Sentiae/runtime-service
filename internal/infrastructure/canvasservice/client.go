// Package canvasservice is a gRPC client that runtime-service uses to
// push node-scoped state changes into canvas-service alongside the
// durable Kafka fan-out. Closes §19.1 flow 1E.
//
// Platform rule: service↔service = gRPC. The legacy HTTP client was
// removed in Foxtrot.
package canvasservice

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	canvasv1 "github.com/sentiae/canvas-service/gen/proto/canvas/v1"
)

// Client posts node-scoped state changes to canvas-service via
// CanvasService.UpdateNodeTestBadge.
type Client struct {
	conn    *grpc.ClientConn
	client  canvasv1.CanvasServiceClient
	timeout time.Duration

	ServiceToken  string
	ServiceUserID string
}

// NewClient dials canvas-service's gRPC listener. An empty grpcAddr
// disables the push path.
func NewClient(grpcAddr string, timeout time.Duration) *Client {
	if grpcAddr == "" {
		return &Client{}
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	conn, err := grpc.NewClient(grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return &Client{timeout: timeout}
	}
	return &Client{
		conn:    conn,
		client:  canvasv1.NewCanvasServiceClient(conn),
		timeout: timeout,
	}
}

// TestResultPayload mirrors canvas-service's test-badge input.
type TestResultPayload struct {
	RunID       string   `json:"run_id"`
	Status      string   `json:"status"`
	Passing     int      `json:"passing"`
	Failed      int      `json:"failed,omitempty"`
	Skipped     int      `json:"skipped,omitempty"`
	Total       int      `json:"total"`
	CoveragePct *float64 `json:"coverage_pct,omitempty"`
	DurationMS  int64    `json:"duration_ms,omitempty"`
}

func (c *Client) outCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	md := metadata.MD{}
	if c.ServiceToken != "" {
		md.Set("authorization", "Bearer "+c.ServiceToken)
	}
	if c.ServiceUserID != "" {
		md.Set("x-user-id", c.ServiceUserID)
	}
	if len(md) > 0 {
		ctx = metadata.NewOutgoingContext(ctx, md)
	}
	return context.WithTimeout(ctx, c.timeout)
}

// ApplyTestResult pushes a terminal test-run verdict onto the target
// canvas node. Best-effort — transport errors do not affect the
// authoritative Kafka path.
func (c *Client) ApplyTestResult(ctx context.Context, canvasID, nodeID uuid.UUID, payload TestResultPayload) error {
	if c == nil || c.client == nil {
		return nil
	}
	req := &canvasv1.UpdateNodeTestBadgeRequest{
		CanvasId:   canvasID.String(),
		NodeId:     nodeID.String(),
		RunId:      payload.RunID,
		Status:     payload.Status,
		Passing:    int32(payload.Passing),
		Total:      int32(payload.Total),
		Failed:     int32(payload.Failed),
		Skipped:    int32(payload.Skipped),
		DurationMs: payload.DurationMS,
	}
	if payload.CoveragePct != nil {
		req.HasCoverage = true
		req.CoveragePct = *payload.CoveragePct
	}
	out, cancel := c.outCtx(ctx)
	defer cancel()
	if _, err := c.client.UpdateNodeTestBadge(out, req); err != nil {
		return fmt.Errorf("canvasservice: UpdateNodeTestBadge: %w", err)
	}
	return nil
}

// Close releases the underlying gRPC channel.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}
