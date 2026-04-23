package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	kafka "github.com/sentiae/platform-kit/kafka"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// fakeExecutionUC captures CreateExecution calls so tests can assert the
// consumer wired the payload through. It stays deliberately minimal — the
// canvas-executed consumer only touches CreateExecution; the other methods
// just satisfy the interface.
type fakeExecutionUC struct {
	createCalls []usecase.CreateExecutionInput
	createErr   error
}

func (f *fakeExecutionUC) CreateExecution(_ context.Context, input usecase.CreateExecutionInput) (*domain.Execution, error) {
	f.createCalls = append(f.createCalls, input)
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &domain.Execution{ID: uuid.New()}, nil
}

func (f *fakeExecutionUC) GetExecution(context.Context, uuid.UUID) (*domain.Execution, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeExecutionUC) ListExecutions(context.Context, uuid.UUID, int, int) ([]domain.Execution, int64, error) {
	return nil, 0, errors.New("not implemented")
}

func (f *fakeExecutionUC) CancelExecution(context.Context, uuid.UUID) error {
	return errors.New("not implemented")
}

func (f *fakeExecutionUC) GetExecutionMetrics(context.Context, uuid.UUID) (*domain.ExecutionMetrics, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeExecutionUC) ProcessPending(context.Context, int) (int, error) {
	return 0, errors.New("not implemented")
}

func (f *fakeExecutionUC) ExecuteSync(context.Context, usecase.CreateExecutionInput) (*domain.Execution, error) {
	return nil, errors.New("not implemented")
}

// TestHandle_CodeNodeDispatches verifies that a canvas.node.executed event
// carrying a code snapshot lands in ExecutionUseCase.CreateExecution with the
// right language, code, and correlation IDs.
func TestHandle_CodeNodeDispatches(t *testing.T) {
	uc := &fakeExecutionUC{}
	consumer := NewCanvasExecutedConsumer(uc)

	canvasNodeID := uuid.New().String()
	orgID := uuid.New().String()
	actorID := uuid.New().String()

	metadata := map[string]any{
		"canvas_id":       uuid.New().String(),
		"node_id":         canvasNodeID,
		"node_type":       "code",
		"session_id":      uuid.New().String(),
		"actor_id":        actorID,
		"started_at":      "2026-04-17T00:00:00Z",
		"execution_id":    uuid.New().String(),
		"organization_id": orgID,
		"requested_by":    actorID,
		"canvas_node_id":  canvasNodeID,
		"language":        "python",
		"code":            "print('hi')",
		"stdin":           "",
		"vcpu":            2,
		"memory_mb":       512,
		"timeout_sec":     30,
	}
	rawData, err := json.Marshal(map[string]any{
		"resource_id":   canvasNodeID,
		"resource_type": "node",
		"metadata":      metadata,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	event := kafka.CloudEvent{
		SpecVersion: "1.0",
		Type:        CanvasNodeExecutedEventType,
		Data:        rawData,
	}

	if err := consumer.Handle(context.Background(), event); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}

	if len(uc.createCalls) != 1 {
		t.Fatalf("expected 1 CreateExecution call, got %d", len(uc.createCalls))
	}
	got := uc.createCalls[0]
	if got.Language != domain.LanguagePython {
		t.Errorf("language: want python, got %q", got.Language)
	}
	if got.Code != "print('hi')" {
		t.Errorf("code: unexpected %q", got.Code)
	}
	if got.OrganizationID.String() != orgID {
		t.Errorf("organization_id: want %s, got %s", orgID, got.OrganizationID)
	}
	if got.RequestedBy.String() != actorID {
		t.Errorf("requested_by: want %s, got %s", actorID, got.RequestedBy)
	}
	if got.NodeID == nil || got.NodeID.String() != canvasNodeID {
		t.Errorf("node_id: want %s, got %v", canvasNodeID, got.NodeID)
	}
	if got.Resources == nil || got.Resources.VCPU != 2 || got.Resources.MemoryMB != 512 || got.Resources.TimeoutSec != 30 {
		t.Errorf("resources: unexpected %+v", got.Resources)
	}
}

// TestHandle_NonCodeNodeIsObservationOnly makes sure the consumer doesn't
// DLQ events that arrive without a code snapshot (dashboards, transforms,
// AI nodes). It should log and return nil — runtime has nothing to run.
func TestHandle_NonCodeNodeIsObservationOnly(t *testing.T) {
	uc := &fakeExecutionUC{}
	consumer := NewCanvasExecutedConsumer(uc)

	metadata := map[string]any{
		"canvas_id":    uuid.New().String(),
		"node_id":      uuid.New().String(),
		"node_type":    "dashboard",
		"session_id":   uuid.New().String(),
		"actor_id":     uuid.New().String(),
		"execution_id": uuid.New().String(),
		"started_at":   "2026-04-17T00:00:00Z",
	}
	rawData, err := json.Marshal(map[string]any{"metadata": metadata})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	event := kafka.CloudEvent{Type: CanvasNodeExecutedEventType, Data: rawData}
	if err := consumer.Handle(context.Background(), event); err != nil {
		t.Fatalf("Handle returned error for observation-only event: %v", err)
	}
	if len(uc.createCalls) != 0 {
		t.Fatalf("expected 0 CreateExecution calls, got %d", len(uc.createCalls))
	}
}

// TestHandle_DirectPayloadForm accepts payloads that did not go through the
// platform-kit EventData envelope (older publishers, test fixtures). The
// consumer should fall back to decoding the root object directly.
func TestHandle_DirectPayloadForm(t *testing.T) {
	uc := &fakeExecutionUC{}
	consumer := NewCanvasExecutedConsumer(uc)

	nodeID := uuid.New().String()
	rawData, err := json.Marshal(map[string]any{
		"node_id":   nodeID,
		"node_type": "code",
		"language":  "go",
		"code":      "package main\nfunc main(){}\n",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	event := kafka.CloudEvent{Type: CanvasNodeExecutedEventType, Data: rawData}
	if err := consumer.Handle(context.Background(), event); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(uc.createCalls) != 1 {
		t.Fatalf("expected 1 CreateExecution call, got %d", len(uc.createCalls))
	}
	if uc.createCalls[0].Language != domain.LanguageGo {
		t.Errorf("language: want go, got %q", uc.createCalls[0].Language)
	}
	if uc.createCalls[0].NodeID == nil || uc.createCalls[0].NodeID.String() != nodeID {
		t.Errorf("node_id fallback failed: got %v", uc.createCalls[0].NodeID)
	}
}

// TestHandle_NilUseCaseReturnsError ensures we surface a wiring bug instead
// of silently swallowing events when the DI container forgot to inject the
// execution use case.
func TestHandle_NilUseCaseReturnsError(t *testing.T) {
	consumer := NewCanvasExecutedConsumer(nil)
	event := kafka.CloudEvent{Type: CanvasNodeExecutedEventType, Data: json.RawMessage(`{}`)}
	if err := consumer.Handle(context.Background(), event); err == nil {
		t.Fatal("expected error when executionUC is nil, got nil")
	}
}

// TestRegister_NilConsumerIsNoop mirrors the Kafka-disabled dev path: the
// Register helper must not panic when the shared EventConsumer is nil.
func TestRegister_NilConsumerIsNoop(t *testing.T) {
	consumer := NewCanvasExecutedConsumer(&fakeExecutionUC{})
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Register(nil) panicked: %v", r)
		}
	}()
	consumer.Register(nil)
}
