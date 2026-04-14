package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/google/uuid"
	kafka "github.com/sentiae/platform-kit/kafka"
	"github.com/sentiae/runtime-service/internal/domain"
)

// InboundEventHandlerService registers handlers for inbound CloudEvents
// consumed from Kafka. Each handler translates a domain event from another
// service (typically canvas-service) into a runtime-service use-case call.
type InboundEventHandlerService struct {
	executionUC ExecutionUseCase
}

// NewInboundEventHandlerService constructs the handler service.
func NewInboundEventHandlerService(executionUC ExecutionUseCase) *InboundEventHandlerService {
	return &InboundEventHandlerService{executionUC: executionUC}
}

// EventConsumer is the minimal interface the handler service needs. This keeps
// the coupling loose and lets tests supply a fake.
type EventConsumer interface {
	Handle(eventType string, handler kafka.EventHandler)
}

// RegisterHandlers wires all inbound handlers onto the provided consumer.
func (s *InboundEventHandlerService) RegisterHandlers(consumer EventConsumer) {
	consumer.Handle("sentiae.canvas.code.execute_requested", s.handleCanvasCodeExecute)
	log.Println("[RUNTIME-EVENTS] Inbound handlers registered")
}

// canvasExecutePayload mirrors the fields produced by canvas-service when it
// asks the runtime to execute a code node. All fields are optional except
// language and code.
type canvasExecutePayload struct {
	OrganizationID string `json:"organization_id"`
	RequestedBy    string `json:"requested_by"`
	CanvasNodeID   string `json:"canvas_node_id"`
	NodeID         string `json:"node_id"`
	WorkflowID     string `json:"workflow_id"`
	Language       string `json:"language"`
	Code           string `json:"code"`
	Stdin          string `json:"stdin"`
	VCPU           int    `json:"vcpu"`
	MemoryMB       int    `json:"memory_mb"`
	TimeoutSec     int    `json:"timeout_sec"`
}

func (s *InboundEventHandlerService) handleCanvasCodeExecute(ctx context.Context, event kafka.CloudEvent) error {
	if s.executionUC == nil {
		return fmt.Errorf("execution use case not wired")
	}

	raw, err := json.Marshal(event.Data)
	if err != nil {
		return fmt.Errorf("marshal event data: %w", err)
	}

	// Events from canvas-service wrap their payload under an EventData
	// envelope ({resource_id, resource_type, metadata}). Try the envelope
	// first, fall back to a direct payload.
	var envelope struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	var payload canvasExecutePayload
	_ = json.Unmarshal(raw, &envelope)
	if len(envelope.Metadata) > 0 {
		if err := json.Unmarshal(envelope.Metadata, &payload); err != nil {
			return fmt.Errorf("unmarshal envelope metadata: %w", err)
		}
	} else {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("unmarshal payload: %w", err)
		}
	}

	if payload.Language == "" || payload.Code == "" {
		return fmt.Errorf("language and code are required")
	}

	input := CreateExecutionInput{
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
	}
	if payload.NodeID != "" {
		if id, err := uuid.Parse(payload.NodeID); err == nil {
			input.NodeID = &id
		}
	} else if payload.CanvasNodeID != "" {
		// Fall back to canvas_node_id so downstream can correlate results
		// back to the originating canvas node even when no canonical NodeID
		// is provided.
		if id, err := uuid.Parse(payload.CanvasNodeID); err == nil {
			input.NodeID = &id
		}
	}
	if payload.WorkflowID != "" {
		if id, err := uuid.Parse(payload.WorkflowID); err == nil {
			input.WorkflowID = &id
		}
	}

	if payload.VCPU > 0 || payload.MemoryMB > 0 || payload.TimeoutSec > 0 {
		input.Resources = &domain.ResourceLimit{
			VCPU:       payload.VCPU,
			MemoryMB:   payload.MemoryMB,
			TimeoutSec: payload.TimeoutSec,
		}
	}

	_, err = s.executionUC.CreateExecution(ctx, input)
	if err != nil {
		return fmt.Errorf("create execution: %w", err)
	}
	log.Printf("[RUNTIME-EVENTS] Accepted canvas execute request (canvas_node_id=%s)", payload.CanvasNodeID)
	return nil
}
