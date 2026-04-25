package grpc

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// ─────────────────────────────────────────────────────────────────────
// Fakes
// ─────────────────────────────────────────────────────────────────────

type fakeExecutionUC struct {
	executions map[uuid.UUID]*domain.Execution
	metrics    map[uuid.UUID]*domain.ExecutionMetrics
}

func newFakeExecUC() *fakeExecutionUC {
	return &fakeExecutionUC{
		executions: map[uuid.UUID]*domain.Execution{},
		metrics:    map[uuid.UUID]*domain.ExecutionMetrics{},
	}
}

func (f *fakeExecutionUC) CreateExecution(_ context.Context, in usecase.CreateExecutionInput) (*domain.Execution, error) {
	e := &domain.Execution{
		ID:             uuid.New(),
		OrganizationID: in.OrganizationID,
		RequestedBy:    in.RequestedBy,
		Language:       in.Language,
		Code:           in.Code,
		Stdin:          in.Stdin,
		Status:         domain.ExecutionStatusPending,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if in.Resources != nil {
		e.Resources = *in.Resources
	}
	f.executions[e.ID] = e
	return e, nil
}

func (f *fakeExecutionUC) GetExecution(_ context.Context, id uuid.UUID) (*domain.Execution, error) {
	e, ok := f.executions[id]
	if !ok {
		return nil, domain.ErrExecutionNotFound
	}
	return e, nil
}

func (f *fakeExecutionUC) ListExecutions(_ context.Context, orgID uuid.UUID, limit, offset int) ([]domain.Execution, int64, error) {
	out := []domain.Execution{}
	for _, e := range f.executions {
		if e.OrganizationID == orgID {
			out = append(out, *e)
		}
	}
	return out, int64(len(out)), nil
}

func (f *fakeExecutionUC) CancelExecution(_ context.Context, id uuid.UUID) error {
	if e, ok := f.executions[id]; ok {
		e.Status = domain.ExecutionStatusCancelled
	}
	return nil
}

func (f *fakeExecutionUC) GetExecutionMetrics(_ context.Context, executionID uuid.UUID) (*domain.ExecutionMetrics, error) {
	if m, ok := f.metrics[executionID]; ok {
		return m, nil
	}
	return nil, domain.ErrExecutionNotFound
}

func (f *fakeExecutionUC) ProcessPending(_ context.Context, _ int) (int, error) {
	return 0, nil
}

func (f *fakeExecutionUC) ExecuteSync(ctx context.Context, in usecase.CreateExecutionInput) (*domain.Execution, error) {
	e, err := f.CreateExecution(ctx, in)
	if err != nil {
		return nil, err
	}
	// Simulate completion.
	now := time.Now()
	exit := 0
	dur := int64(5)
	e.Status = domain.ExecutionStatusCompleted
	e.Stdout = "ok"
	e.ExitCode = &exit
	e.CompletedAt = &now
	e.DurationMS = &dur
	return e, nil
}

// newTestServer wires a minimal ExecutionServer over an ephemeral
// listener and returns an RPC client. No test-run deps are attached by
// default — tests that need them call WithTestRunDeps via the returned
// *ExecutionServer.
func newTestServerExec(t *testing.T) (runtimev1.RuntimeServiceClient, *fakeExecutionUC, *Server, func()) {
	t.Helper()
	uc := newFakeExecUC()
	srv := NewServer(ServerConfig{EnableRecovery: true}, uc, nil, nil)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.GetGRPCServer().Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cleanup := func() {
		_ = conn.Close()
		srv.Shutdown()
	}
	return runtimev1.NewRuntimeServiceClient(conn), uc, srv, cleanup
}

// ─────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────

func TestRuntime_ExecuteSync(t *testing.T) {
	client, _, _, cleanup := newTestServerExec(t)
	defer cleanup()

	resp, err := client.Execute(context.Background(), &runtimev1.ExecuteRequest{
		OrganizationId: uuid.New().String(),
		RequestedBy:    uuid.New().String(),
		Language:       string(domain.LanguagePython),
		Code:           "print('hi')",
		TimeoutSec:     10,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.GetExecution().GetStatus() == "" {
		t.Fatalf("expected status on execution")
	}
	if resp.GetExecution().GetStdout() != "ok" {
		t.Fatalf("expected stdout=ok, got %q", resp.GetExecution().GetStdout())
	}
}

func TestRuntime_ExecuteAsyncThenStatusAndResult(t *testing.T) {
	client, _, _, cleanup := newTestServerExec(t)
	defer cleanup()
	ctx := context.Background()

	async, err := client.ExecuteAsync(ctx, &runtimev1.ExecuteRequest{
		OrganizationId: uuid.New().String(),
		RequestedBy:    uuid.New().String(),
		Language:       string(domain.LanguageGo),
		Code:           "package main\nfunc main(){}",
	})
	if err != nil {
		t.Fatalf("ExecuteAsync: %v", err)
	}
	if async.GetJobId() == "" {
		t.Fatalf("expected job_id")
	}

	st, err := client.GetExecutionStatus(ctx, &runtimev1.GetExecutionStatusRequest{JobId: async.GetJobId()})
	if err != nil {
		t.Fatalf("GetExecutionStatus: %v", err)
	}
	if st.GetJobId() != async.GetJobId() {
		t.Fatalf("job_id mismatch")
	}

	res, err := client.GetExecutionResult(ctx, &runtimev1.GetExecutionResultRequest{JobId: async.GetJobId()})
	if err != nil {
		t.Fatalf("GetExecutionResult: %v", err)
	}
	if res.GetStatus() == "" {
		t.Fatalf("expected status on result")
	}
}

func TestRuntime_ExecuteInvalidLanguage(t *testing.T) {
	client, _, _, cleanup := newTestServerExec(t)
	defer cleanup()

	_, err := client.Execute(context.Background(), &runtimev1.ExecuteRequest{
		OrganizationId: uuid.New().String(),
		RequestedBy:    uuid.New().String(),
		Language:       "cobol",
		Code:           "x",
	})
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s", code)
	}
}

func TestRuntime_DispatchTestRun_WithoutRepo_Unavailable(t *testing.T) {
	client, _, _, cleanup := newTestServerExec(t)
	defer cleanup()

	_, err := client.DispatchTestRun(context.Background(), &runtimev1.DispatchTestRunRequest{
		OrganizationId: uuid.New().String(),
		Language:       string(domain.LanguagePython),
		TestType:       "unit",
	})
	if code := status.Code(err); code != codes.Unavailable {
		t.Fatalf("expected Unavailable without TestRunRepo, got %s", code)
	}
}

func TestRuntime_GetVMUsageRequiresScope(t *testing.T) {
	client, _, _, cleanup := newTestServerExec(t)
	defer cleanup()

	_, err := client.GetVMUsage(context.Background(), &runtimev1.GetVMUsageRequest{})
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s", code)
	}
}

func TestRuntime_GetVMUsage_ByExecution(t *testing.T) {
	client, uc, srv, cleanup := newTestServerExec(t)
	defer cleanup()

	// Seed a metric row attached to an execution.
	execID := uuid.New()
	uc.metrics[execID] = &domain.ExecutionMetrics{
		ID:           uuid.New(),
		ExecutionID:  execID,
		CPUTimeMS:    500,
		MemoryAvgMB:  256,
		TotalTimeMS:  2000,
		BootTimeMS:   80,
	}
	// Allow the GetVMUsage code path to look up metrics via the lister
	// seam so we don't need to attach a TestRunRepo.
	srv.ExecutionServer().WithTestRunDeps(TestRunServerDeps{ExecutionsLister: uc})

	resp, err := client.GetVMUsage(context.Background(), &runtimev1.GetVMUsageRequest{
		ExecutionId: execID.String(),
	})
	if err != nil {
		t.Fatalf("GetVMUsage: %v", err)
	}
	if resp.GetScope() != "execution" {
		t.Fatalf("expected scope=execution, got %q", resp.GetScope())
	}
	if resp.GetTotalCpuTimeMs() != 500 {
		t.Fatalf("expected cpu=500, got %d", resp.GetTotalCpuTimeMs())
	}
	if resp.GetExecutionCount() != 1 {
		t.Fatalf("expected execution_count=1, got %d", resp.GetExecutionCount())
	}
}

func TestRuntime_Health(t *testing.T) {
	_, _, srv, cleanup := newTestServerExec(t)
	defer cleanup()
	_ = srv

	// Open a second connection to ping the Health service.
	lis, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = srv.GetGRPCServer().Serve(lis) }()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	hc := grpc_health_v1.NewHealthClient(conn)
	resp, err := hc.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{Service: "runtime.v1.RuntimeService"})
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	if resp.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("expected SERVING, got %s", resp.GetStatus())
	}
}

// handleDomainError already maps ErrExecutionNotFound → NotFound.
func TestHandleDomainError_NotFound(t *testing.T) {
	if code := status.Code(handleDomainError(domain.ErrExecutionNotFound)); code != codes.NotFound {
		t.Fatalf("expected NotFound, got %s", code)
	}
	if code := status.Code(handleDomainError(errors.New("boom"))); code != codes.Internal {
		t.Fatalf("expected Internal, got %s", code)
	}
}
