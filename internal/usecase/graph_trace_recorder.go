package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// GraphTraceRecorder provides trace capture functionality for graph executions.
type GraphTraceRecorder struct {
	traceRepo    repository.GraphTraceRepository
	snapshotRepo repository.GraphTraceSnapshotRepository
}

// NewGraphTraceRecorder creates a new GraphTraceRecorder
func NewGraphTraceRecorder(
	traceRepo repository.GraphTraceRepository,
	snapshotRepo repository.GraphTraceSnapshotRepository,
) *GraphTraceRecorder {
	return &GraphTraceRecorder{
		traceRepo:    traceRepo,
		snapshotRepo: snapshotRepo,
	}
}

// StartTrace creates a new execution trace for a graph execution
func (r *GraphTraceRecorder) StartTrace(ctx context.Context, graphExecID, graphID, orgID uuid.UUID, triggerData domain.JSONMap) (*domain.GraphExecutionTrace, error) {
	trace := &domain.GraphExecutionTrace{
		ID:               uuid.New(),
		GraphExecutionID: graphExecID,
		GraphID:          graphID,
		OrganizationID:   orgID,
		Status:           "recording",
		TotalNodes:       0,
		TriggerData:      triggerData,
		CreatedAt:        time.Now().UTC(),
	}

	if err := r.traceRepo.Create(ctx, trace); err != nil {
		return nil, fmt.Errorf("failed to create graph execution trace: %w", err)
	}

	return trace, nil
}

// RecordNode records a node snapshot into a graph execution trace
func (r *GraphTraceRecorder) RecordNode(
	ctx context.Context,
	trace *domain.GraphExecutionTrace,
	nodeID uuid.UUID,
	nodeName, nodeType string,
	seqNum int,
	input, output, config domain.JSONMap,
	status, errMsg string,
	startedAt, completedAt time.Time,
) error {
	durationMs := float64(completedAt.Sub(startedAt).Microseconds()) / 1000.0

	inputBytes, _ := json.Marshal(input)
	inputSize := int64(len(inputBytes))
	outputBytes, _ := json.Marshal(output)
	outputSize := int64(len(outputBytes))

	snapshot := &domain.GraphTraceNodeSnapshot{
		ID:              uuid.New(),
		TraceID:         trace.ID,
		GraphNodeID:     nodeID,
		NodeName:        nodeName,
		NodeType:        nodeType,
		SequenceNumber:  seqNum,
		Input:           input,
		Output:          output,
		Config:          config,
		Status:          status,
		Error:           errMsg,
		DurationMS:      &durationMs,
		InputSizeBytes:  &inputSize,
		OutputSizeBytes: &outputSize,
		StartedAt:       startedAt,
		CompletedAt:     completedAt,
	}

	if err := r.snapshotRepo.Create(ctx, snapshot); err != nil {
		return fmt.Errorf("failed to record trace node snapshot: %w", err)
	}

	trace.TotalNodes++
	return nil
}

// CompleteTrace finalizes a graph execution trace
func (r *GraphTraceRecorder) CompleteTrace(ctx context.Context, trace *domain.GraphExecutionTrace, status string) error {
	now := time.Now().UTC()
	trace.Status = status
	trace.CompletedAt = &now

	if trace.CreatedAt.Before(now) {
		durationMs := float64(now.Sub(trace.CreatedAt).Microseconds()) / 1000.0
		trace.TotalDurationMS = &durationMs
	}

	if err := r.traceRepo.Update(ctx, trace); err != nil {
		return fmt.Errorf("failed to complete graph execution trace: %w", err)
	}

	return nil
}
