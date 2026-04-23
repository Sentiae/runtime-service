// Package messaging — canvas_executed_consumer wires the runtime-service onto
// the canvas-service `sentiae.canvas.node.executed` event so "Run on canvas"
// is event-driven end-to-end (GAP_MAP_2026_04_17_AUDIT A1 / VERTICAL_INTEGRATION
// Tier 1A).
//
// The canvas-service publisher
// (canvas-service/internal/usecase/runtime_deploy_usecase.go:299) emits the
// event right after it dispatches `sentiae.canvas.code.execute_requested`.
// For code-type nodes the payload carries the full executable snapshot
// (language, code, stdin, resource limits) so runtime-service can spin up a
// Firecracker job directly from this event. Non-code nodes (dashboards, AI
// nodes, transforms) arrive with only the observation fields — we log and
// skip so Pulse and Eve still see the fact without runtime trying to run
// something it can't.
//
// On completion the existing executionService publishes
// `sentiae.runtime.execution.completed` via the injected EventPublisher, which
// canvas consumes to render the result badge on the node. We do not republish
// here; the execution use case owns the outbound runtime lifecycle contract.
package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/google/uuid"

	kafka "github.com/sentiae/platform-kit/kafka"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// CanvasNodeExecutedEventType is the CloudEvent type emitted by canvas-service
// when a node run is initiated. Topic derived by platform-kit:
// {prefix}.{domain}.{resource} → "sentiae.canvas.node".
const CanvasNodeExecutedEventType = "sentiae.canvas.node.executed"

// canvasNodeExecutedPayload mirrors the fields canvas-service sets on
// `sentiae.canvas.node.executed`. The first six fields
// (canvas_id/node_id/node_type/session_id/actor_id/execution_id) are always
// present; the remaining `language`/`code`/`stdin`/resource-limit fields are
// included for code-type nodes so runtime-service can dispatch the job
// directly from this event.
type canvasNodeExecutedPayload struct {
	CanvasID    string `json:"canvas_id"`
	NodeID      string `json:"node_id"`
	NodeType    string `json:"node_type"`
	SessionID   string `json:"session_id"`
	ActorID     string `json:"actor_id"`
	ExecutionID string `json:"execution_id"`
	StartedAt   string `json:"started_at"`

	// Optional — present when canvas-service attaches the code snapshot for
	// code-type nodes. Matches canvasExecutePayload in event_handlers.go.
	OrganizationID string `json:"organization_id"`
	RequestedBy    string `json:"requested_by"`
	CanvasNodeID   string `json:"canvas_node_id"`
	Language       string `json:"language"`
	Code           string `json:"code"`
	Stdin          string `json:"stdin"`
	VCPU           int    `json:"vcpu"`
	MemoryMB       int    `json:"memory_mb"`
	TimeoutSec     int    `json:"timeout_sec"`
}

// CanvasExecutedConsumer dispatches `sentiae.canvas.node.executed` events into
// the runtime execution use case. It is registered on the shared EventConsumer
// at DI wiring time and does not own the consumer lifecycle itself.
type CanvasExecutedConsumer struct {
	executionUC usecase.ExecutionUseCase
}

// NewCanvasExecutedConsumer constructs the consumer. executionUC must be
// non-nil — without it the handler fails every event.
func NewCanvasExecutedConsumer(executionUC usecase.ExecutionUseCase) *CanvasExecutedConsumer {
	return &CanvasExecutedConsumer{executionUC: executionUC}
}

// Register subscribes the handler on the given EventConsumer. Safe to call
// even when the consumer is nil (Kafka disabled in dev) — becomes a no-op.
func (c *CanvasExecutedConsumer) Register(consumer *EventConsumer) {
	if consumer == nil || c == nil {
		return
	}
	consumer.Handle(CanvasNodeExecutedEventType, c.Handle)
	log.Printf("[RUNTIME-EVENTS] canvas.node.executed consumer registered")
}

// Handle is the kafka.EventHandler entry point. Exported so tests can feed
// synthetic CloudEvents without running the Kafka reader.
func (c *CanvasExecutedConsumer) Handle(ctx context.Context, event kafka.CloudEvent) error {
	if c == nil || c.executionUC == nil {
		return fmt.Errorf("canvas-executed consumer not wired")
	}

	payload, err := parseCanvasNodeExecutedPayload(event)
	if err != nil {
		return fmt.Errorf("decode canvas.node.executed: %w", err)
	}

	// Non-code nodes are observation-only: dashboards, transforms, AI nodes
	// don't need a Firecracker job. Log so Pulse still sees the event path
	// and return nil so we don't spam the DLQ.
	if payload.Code == "" || payload.Language == "" {
		log.Printf("[RUNTIME-EVENTS] canvas.node.executed observed (node_id=%s node_type=%s execution_id=%s) — no code snapshot, skipping dispatch",
			payload.NodeID, payload.NodeType, payload.ExecutionID)
		return nil
	}

	input := usecase.CreateExecutionInput{
		Language: domain.Language(payload.Language),
		Code:     payload.Code,
		Stdin:    payload.Stdin,
	}
	if payload.OrganizationID != "" {
		if id, err := uuid.Parse(payload.OrganizationID); err == nil {
			input.OrganizationID = id
		}
	}
	if payload.RequestedBy != "" {
		if id, err := uuid.Parse(payload.RequestedBy); err == nil {
			input.RequestedBy = id
		}
	} else if payload.ActorID != "" {
		if id, err := uuid.Parse(payload.ActorID); err == nil {
			input.RequestedBy = id
		}
	}
	// Prefer canvas_node_id when present (matches the placement ID
	// convention used by the existing execute_requested handler); fall
	// back to the top-level node_id the canvas.node.executed event always
	// carries so downstream joins still work.
	if payload.CanvasNodeID != "" {
		if id, err := uuid.Parse(payload.CanvasNodeID); err == nil {
			input.NodeID = &id
		}
	} else if payload.NodeID != "" {
		if id, err := uuid.Parse(payload.NodeID); err == nil {
			input.NodeID = &id
		}
	}

	if payload.VCPU > 0 || payload.MemoryMB > 0 || payload.TimeoutSec > 0 {
		input.Resources = &domain.ResourceLimit{
			VCPU:       payload.VCPU,
			MemoryMB:   payload.MemoryMB,
			TimeoutSec: payload.TimeoutSec,
		}
	}

	if _, err := c.executionUC.CreateExecution(ctx, input); err != nil {
		return fmt.Errorf("create execution from canvas.node.executed: %w", err)
	}

	log.Printf("[RUNTIME-EVENTS] canvas.node.executed dispatched (node_id=%s execution_id=%s lang=%s)",
		payload.NodeID, payload.ExecutionID, payload.Language)
	return nil
}

// parseCanvasNodeExecutedPayload handles both the CloudEvent envelope form
// (EventData{metadata:{...}}) that platform-kit wraps around publisher
// payloads, and the direct-payload form used by older publishers or tests.
func parseCanvasNodeExecutedPayload(event kafka.CloudEvent) (canvasNodeExecutedPayload, error) {
	var payload canvasNodeExecutedPayload
	raw, err := json.Marshal(event.Data)
	if err != nil {
		return payload, fmt.Errorf("marshal event data: %w", err)
	}

	var envelope struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &envelope)
	if len(envelope.Metadata) > 0 {
		if err := json.Unmarshal(envelope.Metadata, &payload); err != nil {
			return payload, fmt.Errorf("unmarshal envelope metadata: %w", err)
		}
		return payload, nil
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, fmt.Errorf("unmarshal payload: %w", err)
	}
	return payload, nil
}
