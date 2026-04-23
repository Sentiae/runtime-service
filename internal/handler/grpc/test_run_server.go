package grpc

import (
	"context"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// TestRunServerDeps bundles the dependencies the RuntimeService RPCs need
// beyond the core ExecutionUseCase. Any of these may be nil — the matching
// RPC returns Unavailable when a required dep is missing.
type TestRunServerDeps struct {
	TestRunRepo      *postgres.TestRunRepo
	TestRunDispatch  TestRunDispatcher
	VMUC             usecase.VMUseCase
	ExecutionsLister ExecutionsLister
}

// TestRunDispatcher is the minimal contract the gRPC handler calls to
// boot a test in a Firecracker VM. It is a superset of the HTTP layer's
// DispatchInVM interface so we can share implementations.
type TestRunDispatcher interface {
	DispatchInVM(ctx context.Context, run *domain.TestRun, profile usecase.TestRunnerProfile) error
}

// ExecutionsLister is the narrow slice of ExecutionUseCase GetVMUsage
// uses when scoped to an organization window. Declared here so the
// test double can implement only what it needs.
type ExecutionsLister interface {
	ListExecutions(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]domain.Execution, int64, error)
	GetExecutionMetrics(ctx context.Context, executionID uuid.UUID) (*domain.ExecutionMetrics, error)
}

// WithTestRunDeps adds the test-run / VM dependencies to an ExecutionServer.
// Safe to call before or after NewExecutionServer; subsequent calls
// overwrite the previous value (last-write-wins).
func (s *ExecutionServer) WithTestRunDeps(deps TestRunServerDeps) *ExecutionServer {
	s.testRunRepo = deps.TestRunRepo
	s.testRunDispatch = deps.TestRunDispatch
	s.vmUC = deps.VMUC
	s.executionsLister = deps.ExecutionsLister
	return s
}

// ─────────────────────────────────────────────────────────────────────
// Execute / ExecuteAsync / GetExecutionStatus / GetExecutionResult.
//
// These are the ergonomic wrappers over the legacy CreateExecution +
// GetExecution RPCs: foundry-service and canvas-service want to say
// "run this code" without managing the CreateExecutionRequest shape.
// ─────────────────────────────────────────────────────────────────────

// Execute runs code synchronously inside a microVM. Used by canvas
// run-code-from-node for the caller-initiated path.
func (s *ExecutionServer) Execute(ctx context.Context, req *runtimev1.ExecuteRequest) (*runtimev1.ExecuteResponse, error) {
	input, err := buildExecuteInput(req)
	if err != nil {
		return nil, err
	}
	execution, err := s.executionUC.ExecuteSync(ctx, input)
	if err != nil {
		return nil, handleDomainError(err)
	}
	return &runtimev1.ExecuteResponse{Execution: domainExecutionToProto(execution)}, nil
}

// ExecuteAsync enqueues an execution. The returned job_id is the
// execution row id; callers poll with GetExecutionStatus /
// GetExecutionResult until the exec completes.
func (s *ExecutionServer) ExecuteAsync(ctx context.Context, req *runtimev1.ExecuteRequest) (*runtimev1.ExecuteAsyncResponse, error) {
	input, err := buildExecuteInput(req)
	if err != nil {
		return nil, err
	}
	execution, err := s.executionUC.CreateExecution(ctx, input)
	if err != nil {
		return nil, handleDomainError(err)
	}
	return &runtimev1.ExecuteAsyncResponse{
		JobId:  execution.ID.String(),
		Status: string(execution.Status),
	}, nil
}

func (s *ExecutionServer) GetExecutionStatus(ctx context.Context, req *runtimev1.GetExecutionStatusRequest) (*runtimev1.GetExecutionStatusResponse, error) {
	id, err := uuid.Parse(req.GetJobId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid job_id")
	}
	exec, err := s.executionUC.GetExecution(ctx, id)
	if err != nil {
		return nil, handleDomainError(err)
	}
	resp := &runtimev1.GetExecutionStatusResponse{
		JobId:     exec.ID.String(),
		Status:    string(exec.Status),
		CreatedAt: exec.CreatedAt.UTC().Format(time.RFC3339),
	}
	if exec.StartedAt != nil {
		resp.StartedAt = exec.StartedAt.UTC().Format(time.RFC3339)
	}
	if exec.CompletedAt != nil {
		resp.CompletedAt = exec.CompletedAt.UTC().Format(time.RFC3339)
	}
	return resp, nil
}

func (s *ExecutionServer) GetExecutionResult(ctx context.Context, req *runtimev1.GetExecutionResultRequest) (*runtimev1.GetExecutionResultResponse, error) {
	id, err := uuid.Parse(req.GetJobId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid job_id")
	}
	exec, err := s.executionUC.GetExecution(ctx, id)
	if err != nil {
		return nil, handleDomainError(err)
	}
	resp := &runtimev1.GetExecutionResultResponse{
		JobId:  exec.ID.String(),
		Status: string(exec.Status),
		Stdout: exec.Stdout,
		Stderr: exec.Stderr,
		Error:  exec.Error,
	}
	if exec.ExitCode != nil {
		resp.ExitCode = int32(*exec.ExitCode)
	}
	if exec.DurationMS != nil {
		resp.DurationMs = *exec.DurationMS
	}
	return resp, nil
}

// buildExecuteInput normalises ExecuteRequest → CreateExecutionInput and
// validates the required fields. Returns an InvalidArgument gRPC error
// when the caller didn't supply a minimum viable request.
func buildExecuteInput(req *runtimev1.ExecuteRequest) (usecase.CreateExecutionInput, error) {
	if req == nil {
		return usecase.CreateExecutionInput{}, status.Error(codes.InvalidArgument, "request is required")
	}
	orgID, err := uuid.Parse(req.GetOrganizationId())
	if err != nil {
		return usecase.CreateExecutionInput{}, status.Error(codes.InvalidArgument, "invalid organization_id")
	}
	requestedBy, err := uuid.Parse(req.GetRequestedBy())
	if err != nil {
		return usecase.CreateExecutionInput{}, status.Error(codes.InvalidArgument, "invalid requested_by")
	}
	lang := domain.Language(req.GetLanguage())
	if !lang.IsValid() {
		return usecase.CreateExecutionInput{}, status.Errorf(codes.InvalidArgument, "unsupported language: %s", req.GetLanguage())
	}
	if req.GetCode() == "" {
		return usecase.CreateExecutionInput{}, status.Error(codes.InvalidArgument, "code cannot be empty")
	}
	input := usecase.CreateExecutionInput{
		OrganizationID: orgID,
		RequestedBy:    requestedBy,
		Language:       lang,
		Code:           req.GetCode(),
		Stdin:          req.GetStdin(),
	}
	if req.GetCanvasNodeId() != "" {
		if id, err := uuid.Parse(req.GetCanvasNodeId()); err == nil {
			input.NodeID = &id
		}
	}
	if req.GetVcpu() > 0 || req.GetMemoryMb() > 0 || req.GetTimeoutSec() > 0 {
		input.Resources = &domain.ResourceLimit{
			VCPU:       int(req.GetVcpu()),
			MemoryMB:   int(req.GetMemoryMb()),
			TimeoutSec: int(req.GetTimeoutSec()),
		}
	}
	return input, nil
}

// ─────────────────────────────────────────────────────────────────────
// DispatchTestRun — mirrors HTTP POST /api/v1/test-runs/{id}/dispatch.
//
// The caller supplies the scoping + classification; we create the
// TestRun row, resolve the runner profile, and optionally kick off the
// microVM dispatch in a goroutine. Returns a handle the caller uses to
// poll coverage via GetTestCoverage once the run completes.
// ─────────────────────────────────────────────────────────────────────

func (s *ExecutionServer) DispatchTestRun(ctx context.Context, req *runtimev1.DispatchTestRunRequest) (*runtimev1.TestRunHandle, error) {
	if s.testRunRepo == nil {
		return nil, status.Error(codes.Unavailable, "test-run repository not configured")
	}
	orgID, err := uuid.Parse(req.GetOrganizationId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid organization_id")
	}
	lang := domain.Language(req.GetLanguage())
	if !lang.IsValid() {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported language: %s", req.GetLanguage())
	}
	testType := domain.TestType(req.GetTestType()).Normalize()

	run := &domain.TestRun{
		ID:             uuid.New(),
		OrganizationID: orgID,
		Language:       lang,
		TestType:       testType,
		MaxRetries:     int(req.GetMaxRetries()),
		Status:         domain.TestRunStatusRunning,
	}
	if run.MaxRetries <= 0 {
		run.MaxRetries = domain.DefaultMaxTestRetries
	}
	if req.GetTimeoutSec() > 0 {
		run.TimeoutMS = int64(req.GetTimeoutSec()) * 1000
	}
	if req.GetExecutionId() != "" {
		if id, err := uuid.Parse(req.GetExecutionId()); err == nil {
			run.ExecutionID = id
		}
	}
	if req.GetTestNodeId() != "" {
		if id, err := uuid.Parse(req.GetTestNodeId()); err == nil {
			run.TestNodeID = id
		}
	}
	if req.GetCodeNodeId() != "" {
		if id, err := uuid.Parse(req.GetCodeNodeId()); err == nil {
			run.CodeNodeID = &id
		}
	}
	if req.GetCanvasId() != "" {
		if id, err := uuid.Parse(req.GetCanvasId()); err == nil {
			run.CanvasID = &id
		}
	}

	if err := s.testRunRepo.Create(ctx, run); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create test run: %v", err)
	}

	profile, matched := usecase.ResolveTestRunner(run.Language, run.TestType)
	runStatus := "queued"
	if s.testRunDispatch != nil {
		// Fire-and-forget — mirror the HTTP handler's 10-minute bound.
		runCopy := *run
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			_ = s.testRunDispatch.DispatchInVM(ctx, &runCopy, profile)
		}()
		runStatus = "dispatched"
	}

	return &runtimev1.TestRunHandle{
		TestRunId:     run.ID.String(),
		ExecutionId:   run.ExecutionID.String(),
		Status:        runStatus,
		RunnerMatched: matched,
		Command:       profile.Command,
		VmProfile:     profile.VMProfile,
		Network:       boolStr(profile.Network),
	}, nil
}

// ─────────────────────────────────────────────────────────────────────
// GetTestCoverage / GetTestCoverageDelta.
// ─────────────────────────────────────────────────────────────────────

func (s *ExecutionServer) GetTestCoverage(ctx context.Context, req *runtimev1.GetTestCoverageRequest) (*runtimev1.TestCoverage, error) {
	run, err := s.loadTestRun(ctx, req.GetTestRunId())
	if err != nil {
		return nil, err
	}
	return testCoverageFromRun(run), nil
}

func (s *ExecutionServer) GetTestCoverageDelta(ctx context.Context, req *runtimev1.GetTestCoverageDeltaRequest) (*runtimev1.TestCoverageDelta, error) {
	base, err := s.loadTestRun(ctx, req.GetBaseTestRunId())
	if err != nil {
		return nil, err
	}
	head, err := s.loadTestRun(ctx, req.GetHeadTestRunId())
	if err != nil {
		return nil, err
	}
	basePct := coverageOrZero(base.CoveragePC)
	headPct := coverageOrZero(head.CoveragePC)

	// new_failures / fixed_failures are approximated from aggregate
	// counts — per-test diff lives in the ResultJSON payload and isn't
	// exposed on the gRPC surface yet.
	newFailures := head.Failed - base.Failed
	if newFailures < 0 {
		newFailures = 0
	}
	fixedFailures := base.Failed - head.Failed
	if fixedFailures < 0 {
		fixedFailures = 0
	}

	return &runtimev1.TestCoverageDelta{
		BasePct:       basePct,
		HeadPct:       headPct,
		DeltaPct:      headPct - basePct,
		BasePassed:    int32(base.Passed),
		HeadPassed:    int32(head.Passed),
		BaseFailed:    int32(base.Failed),
		HeadFailed:    int32(head.Failed),
		NewFailures:   int32(newFailures),
		FixedFailures: int32(fixedFailures),
	}, nil
}

// loadTestRun resolves a UUID string + returns the TestRun, converting
// errors into gRPC status codes.
func (s *ExecutionServer) loadTestRun(ctx context.Context, raw string) (*domain.TestRun, error) {
	if s.testRunRepo == nil {
		return nil, status.Error(codes.Unavailable, "test-run repository not configured")
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid test_run_id")
	}
	run, err := s.testRunRepo.FindByID(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load test run: %v", err)
	}
	if run == nil {
		return nil, status.Error(codes.NotFound, "test run not found")
	}
	return run, nil
}

func testCoverageFromRun(run *domain.TestRun) *runtimev1.TestCoverage {
	cov := &runtimev1.TestCoverage{
		TestRunId: run.ID.String(),
		Passed:    int32(run.Passed),
		Failed:    int32(run.Failed),
		Skipped:   int32(run.Skipped),
		Status:    string(run.Status),
	}
	if run.CoveragePC != nil {
		cov.CoveragePct = *run.CoveragePC
		cov.HasCoverage = true
	}
	if run.DurationMS != nil {
		cov.DurationMs = *run.DurationMS
	}
	return cov
}

func coverageOrZero(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// ─────────────────────────────────────────────────────────────────────
// GetVMUsage — session cost preview.
//
// Scope is either a single execution_id (fast path: pull the metrics row
// directly) or an organization window (aggregate the executions inside
// [since, until]).
// ─────────────────────────────────────────────────────────────────────

func (s *ExecutionServer) GetVMUsage(ctx context.Context, req *runtimev1.GetVMUsageRequest) (*runtimev1.VMUsage, error) {
	if req.GetExecutionId() != "" {
		id, err := uuid.Parse(req.GetExecutionId())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid execution_id")
		}
		metrics, err := s.executionUC.GetExecutionMetrics(ctx, id)
		if err != nil {
			return nil, handleDomainError(err)
		}
		if metrics == nil {
			return nil, status.Error(codes.NotFound, "no metrics recorded for execution")
		}
		activeVMs := 0
		if s.vmUC != nil {
			if vms, err := s.vmUC.ListActiveVMs(ctx); err == nil {
				activeVMs = len(vms)
			}
		}
		return &runtimev1.VMUsage{
			Scope:                "execution",
			TotalCpuTimeMs:       metrics.CPUTimeMS,
			TotalMemoryMbSeconds: metrics.MemoryAvgMB * float64(metrics.TotalTimeMS) / 1000.0,
			TotalBootTimeMs:      metrics.BootTimeMS,
			ExecutionCount:       1,
			ActiveVms:            int32(activeVMs),
		}, nil
	}

	// Organization window.
	if req.GetOrganizationId() == "" {
		return nil, status.Error(codes.InvalidArgument, "execution_id or organization_id is required")
	}
	orgID, err := uuid.Parse(req.GetOrganizationId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid organization_id")
	}
	if s.executionsLister == nil {
		return nil, status.Error(codes.Unavailable, "executions lister not configured")
	}
	since := parseRFC3339Or(req.GetSince(), time.Now().Add(-24*time.Hour))
	until := parseRFC3339Or(req.GetUntil(), time.Now())

	// Walk pages up to a generous cap so we don't blow the gRPC deadline
	// on huge orgs. 500 executions should comfortably cover a 24h window.
	const cap = 500
	pageSize := 100
	var (
		totalCPU, totalBoot int64
		totalMemSec         float64
		count               int32
	)
	fetched := 0
	for offset := 0; fetched < cap; offset += pageSize {
		execs, _, err := s.executionsLister.ListExecutions(ctx, orgID, pageSize, offset)
		if err != nil {
			return nil, handleDomainError(err)
		}
		if len(execs) == 0 {
			break
		}
		for i := range execs {
			e := &execs[i]
			if e.CreatedAt.Before(since) || e.CreatedAt.After(until) {
				continue
			}
			metrics, err := s.executionsLister.GetExecutionMetrics(ctx, e.ID)
			if err != nil || metrics == nil {
				continue
			}
			totalCPU += metrics.CPUTimeMS
			totalBoot += metrics.BootTimeMS
			totalMemSec += metrics.MemoryAvgMB * float64(metrics.TotalTimeMS) / 1000.0
			count++
		}
		fetched += len(execs)
		if len(execs) < pageSize {
			break
		}
	}
	activeVMs := 0
	if s.vmUC != nil {
		if vms, err := s.vmUC.ListActiveVMs(ctx); err == nil {
			activeVMs = len(vms)
		}
	}
	return &runtimev1.VMUsage{
		Scope:                "organization",
		TotalCpuTimeMs:       totalCPU,
		TotalMemoryMbSeconds: totalMemSec,
		TotalBootTimeMs:      totalBoot,
		ExecutionCount:       count,
		ActiveVms:            int32(activeVMs),
	}, nil
}

func parseRFC3339Or(raw string, fallback time.Time) time.Time {
	if raw == "" {
		return fallback
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return fallback
	}
	return t
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
