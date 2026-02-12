package grpc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// ExecutionServer implements the gRPC RuntimeServiceServer interface.
type ExecutionServer struct {
	runtimev1.UnimplementedRuntimeServiceServer
	executionUC usecase.ExecutionUseCase
}

// NewExecutionServer creates a new ExecutionServer.
func NewExecutionServer(executionUC usecase.ExecutionUseCase) *ExecutionServer {
	return &ExecutionServer{
		executionUC: executionUC,
	}
}

// CreateExecution creates a new code execution request.
func (s *ExecutionServer) CreateExecution(ctx context.Context, req *runtimev1.CreateExecutionRequest) (*runtimev1.CreateExecutionResponse, error) {
	// Parse organization ID
	orgID, err := uuid.Parse(req.GetOrganizationId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid organization_id")
	}

	// Parse requested_by user ID
	requestedBy, err := uuid.Parse(req.GetRequestedBy())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid requested_by")
	}

	// Validate language
	language := domain.Language(req.GetLanguage())
	if !language.IsValid() {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported language: %s", req.GetLanguage())
	}

	// Validate code
	if req.GetCode() == "" {
		return nil, status.Error(codes.InvalidArgument, "code cannot be empty")
	}

	// Build resource limits from request fields
	var resources *domain.ResourceLimit
	if req.GetVcpu() > 0 || req.GetMemoryMb() > 0 || req.GetTimeoutSec() > 0 {
		resources = &domain.ResourceLimit{
			VCPU:       int(req.GetVcpu()),
			MemoryMB:   int(req.GetMemoryMb()),
			TimeoutSec: int(req.GetTimeoutSec()),
		}
	}

	// Build use case input
	input := usecase.CreateExecutionInput{
		OrganizationID: orgID,
		RequestedBy:    requestedBy,
		Language:       language,
		Code:           req.GetCode(),
		Stdin:          req.GetStdin(),
		Resources:      resources,
	}

	// Call use case
	execution, err := s.executionUC.CreateExecution(ctx, input)
	if err != nil {
		return nil, handleDomainError(err)
	}

	return &runtimev1.CreateExecutionResponse{
		Execution: domainExecutionToProto(execution),
	}, nil
}

// GetExecution retrieves an execution by ID.
func (s *ExecutionServer) GetExecution(ctx context.Context, req *runtimev1.GetExecutionRequest) (*runtimev1.GetExecutionResponse, error) {
	execID, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid execution id")
	}

	execution, err := s.executionUC.GetExecution(ctx, execID)
	if err != nil {
		return nil, handleDomainError(err)
	}

	return &runtimev1.GetExecutionResponse{
		Execution: domainExecutionToProto(execution),
	}, nil
}

// ListExecutions lists executions for an organization with pagination.
func (s *ExecutionServer) ListExecutions(ctx context.Context, req *runtimev1.ListExecutionsRequest) (*runtimev1.ListExecutionsResponse, error) {
	orgID, err := uuid.Parse(req.GetOrganizationId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid organization_id")
	}

	page := int(req.GetPage())
	if page < 1 {
		page = 1
	}

	pageSize := int(req.GetPageSize())
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	offset := (page - 1) * pageSize

	executions, total, err := s.executionUC.ListExecutions(ctx, orgID, pageSize, offset)
	if err != nil {
		return nil, handleDomainError(err)
	}

	protoExecutions := make([]*runtimev1.Execution, len(executions))
	for i := range executions {
		protoExecutions[i] = domainExecutionToProto(&executions[i])
	}

	return &runtimev1.ListExecutionsResponse{
		Executions: protoExecutions,
		Total:      int32(total),
	}, nil
}

// CancelExecution cancels a running or pending execution.
func (s *ExecutionServer) CancelExecution(ctx context.Context, req *runtimev1.CancelExecutionRequest) (*runtimev1.CancelExecutionResponse, error) {
	execID, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid execution id")
	}

	if err := s.executionUC.CancelExecution(ctx, execID); err != nil {
		return nil, handleDomainError(err)
	}

	return &runtimev1.CancelExecutionResponse{
		Success: true,
	}, nil
}

// GetExecutionMetrics retrieves resource usage metrics for an execution.
func (s *ExecutionServer) GetExecutionMetrics(ctx context.Context, req *runtimev1.GetExecutionMetricsRequest) (*runtimev1.GetExecutionMetricsResponse, error) {
	execID, err := uuid.Parse(req.GetExecutionId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid execution_id")
	}

	metrics, err := s.executionUC.GetExecutionMetrics(ctx, execID)
	if err != nil {
		return nil, handleDomainError(err)
	}

	return &runtimev1.GetExecutionMetricsResponse{
		Metrics: domainMetricsToProto(metrics),
	}, nil
}

// domainExecutionToProto converts a domain Execution to proto Execution.
func domainExecutionToProto(e *domain.Execution) *runtimev1.Execution {
	if e == nil {
		return nil
	}

	proto := &runtimev1.Execution{
		Id:             e.ID.String(),
		OrganizationId: e.OrganizationID.String(),
		RequestedBy:    e.RequestedBy.String(),
		Language:       string(e.Language),
		Code:           e.Code,
		Stdin:          e.Stdin,
		Status:         string(e.Status),
		Stdout:         e.Stdout,
		Stderr:         e.Stderr,
		Error:          e.Error,
		Vcpu:           int32(e.Resources.VCPU),
		MemoryMb:       int32(e.Resources.MemoryMB),
		TimeoutSec:     int32(e.Resources.TimeoutSec),
		CreatedAt:      e.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}

	if e.ExitCode != nil {
		proto.ExitCode = int32(*e.ExitCode)
	}

	if e.DurationMS != nil {
		proto.DurationMs = *e.DurationMS
	}

	if e.StartedAt != nil {
		proto.StartedAt = e.StartedAt.UTC().Format("2006-01-02T15:04:05Z")
	}

	if e.CompletedAt != nil {
		proto.CompletedAt = e.CompletedAt.UTC().Format("2006-01-02T15:04:05Z")
	}

	return proto
}

// domainMetricsToProto converts a domain ExecutionMetrics to proto ExecutionMetrics.
func domainMetricsToProto(m *domain.ExecutionMetrics) *runtimev1.ExecutionMetrics {
	if m == nil {
		return nil
	}

	proto := &runtimev1.ExecutionMetrics{
		Id:           m.ID.String(),
		ExecutionId:  m.ExecutionID.String(),
		VmId:         m.VMID.String(),
		CpuTimeMs:    m.CPUTimeMS,
		MemoryPeakMb: m.MemoryPeakMB,
		MemoryAvgMb:  m.MemoryAvgMB,
		IoReadBytes:  m.IOReadBytes,
		IoWriteBytes: m.IOWriteBytes,
		BootTimeMs:   m.BootTimeMS,
		ExecTimeMs:   m.ExecTimeMS,
		TotalTimeMs:  m.TotalTimeMS,
	}

	if m.CompileTimeMS != nil {
		proto.CompileTimeMs = *m.CompileTimeMS
	}

	return proto
}

// handleDomainError converts domain errors to gRPC status errors.
func handleDomainError(err error) error {
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, domain.ErrExecutionNotFound):
		return status.Error(codes.NotFound, "execution not found")
	case errors.Is(err, domain.ErrVMNotFound):
		return status.Error(codes.NotFound, "microVM not found")
	case errors.Is(err, domain.ErrSnapshotNotFound):
		return status.Error(codes.NotFound, "snapshot not found")
	case errors.Is(err, domain.ErrExecutionAlreadyDone):
		return status.Error(codes.FailedPrecondition, "execution already in terminal state")
	case errors.Is(err, domain.ErrInvalidLanguage):
		return status.Error(codes.InvalidArgument, "unsupported programming language")
	case errors.Is(err, domain.ErrEmptyCode):
		return status.Error(codes.InvalidArgument, "code cannot be empty")
	case errors.Is(err, domain.ErrInvalidID):
		return status.Error(codes.InvalidArgument, "invalid ID")
	case errors.Is(err, domain.ErrInvalidData):
		return status.Error(codes.InvalidArgument, "invalid data")
	case errors.Is(err, domain.ErrInvalidVCPU):
		return status.Error(codes.InvalidArgument, "invalid vCPU count (must be 1-8)")
	case errors.Is(err, domain.ErrInvalidMemory):
		return status.Error(codes.InvalidArgument, "invalid memory size (must be 32-4096 MB)")
	case errors.Is(err, domain.ErrInvalidTimeout):
		return status.Error(codes.InvalidArgument, "invalid timeout (must be 1-600 seconds)")
	case errors.Is(err, domain.ErrResourceLimitExceeded):
		return status.Error(codes.ResourceExhausted, "resource limit exceeded")
	case errors.Is(err, domain.ErrVMPoolExhausted):
		return status.Error(codes.ResourceExhausted, "no available microVMs in pool")
	case errors.Is(err, domain.ErrTimeoutExceeded):
		return status.Error(codes.DeadlineExceeded, "execution timeout exceeded")
	case errors.Is(err, domain.ErrVMNotReady):
		return status.Error(codes.FailedPrecondition, "microVM is not in ready state")
	case errors.Is(err, domain.ErrVMAlreadyTerminated):
		return status.Error(codes.FailedPrecondition, "microVM is already terminated")
	default:
		return status.Error(codes.Internal, "internal server error")
	}
}
