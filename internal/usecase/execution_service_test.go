package usecase

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// --- Mock Repositories ---

type mockExecutionRepo struct {
	executions map[uuid.UUID]*domain.Execution
}

func newMockExecRepo() *mockExecutionRepo {
	return &mockExecutionRepo{executions: make(map[uuid.UUID]*domain.Execution)}
}

func (m *mockExecutionRepo) Create(ctx context.Context, exec *domain.Execution) error {
	m.executions[exec.ID] = exec
	return nil
}

func (m *mockExecutionRepo) Update(ctx context.Context, exec *domain.Execution) error {
	m.executions[exec.ID] = exec
	return nil
}

func (m *mockExecutionRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.Execution, error) {
	if e, ok := m.executions[id]; ok {
		return e, nil
	}
	return nil, domain.ErrExecutionNotFound
}

func (m *mockExecutionRepo) FindByOrganization(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]domain.Execution, int64, error) {
	var results []domain.Execution
	for _, e := range m.executions {
		if e.OrganizationID == orgID {
			results = append(results, *e)
		}
	}
	return results, int64(len(results)), nil
}

func (m *mockExecutionRepo) FindPending(ctx context.Context, limit int) ([]domain.Execution, error) {
	var results []domain.Execution
	for _, e := range m.executions {
		if e.Status == domain.ExecutionStatusPending {
			results = append(results, *e)
		}
	}
	return results, nil
}

func (m *mockExecutionRepo) FindRunning(ctx context.Context) ([]domain.Execution, error) {
	var results []domain.Execution
	for _, e := range m.executions {
		if e.Status == domain.ExecutionStatusRunning {
			results = append(results, *e)
		}
	}
	return results, nil
}

type mockMetricsRepo struct {
	metrics map[uuid.UUID]*domain.ExecutionMetrics
}

func newMockMetricsRepo() *mockMetricsRepo {
	return &mockMetricsRepo{metrics: make(map[uuid.UUID]*domain.ExecutionMetrics)}
}

func (m *mockMetricsRepo) Create(ctx context.Context, metrics *domain.ExecutionMetrics) error {
	m.metrics[metrics.ExecutionID] = metrics
	return nil
}

func (m *mockMetricsRepo) FindByExecution(ctx context.Context, executionID uuid.UUID) (*domain.ExecutionMetrics, error) {
	if met, ok := m.metrics[executionID]; ok {
		return met, nil
	}
	return nil, nil
}

func (m *mockMetricsRepo) FindByVM(ctx context.Context, vmID uuid.UUID, limit int) ([]domain.ExecutionMetrics, error) {
	return nil, nil
}

type mockVMUC struct {
	vms map[uuid.UUID]*domain.MicroVM
}

func newMockVMUC() *mockVMUC {
	return &mockVMUC{vms: make(map[uuid.UUID]*domain.MicroVM)}
}

func (m *mockVMUC) CreateVM(ctx context.Context, lang domain.Language, vcpu, memMB int) (*domain.MicroVM, error) {
	vm := &domain.MicroVM{ID: uuid.New(), Status: domain.VMStatusReady, Language: lang}
	m.vms[vm.ID] = vm
	return vm, nil
}

func (m *mockVMUC) AcquireVM(ctx context.Context, lang domain.Language, res domain.ResourceLimit) (*domain.MicroVM, error) {
	return m.CreateVM(ctx, lang, res.VCPU, res.MemoryMB)
}

func (m *mockVMUC) ReleaseVM(ctx context.Context, vmID uuid.UUID) error {
	if vm, ok := m.vms[vmID]; ok {
		vm.Release()
	}
	return nil
}

func (m *mockVMUC) TerminateVM(ctx context.Context, vmID uuid.UUID) error {
	return nil
}

func (m *mockVMUC) GetVM(ctx context.Context, id uuid.UUID) (*domain.MicroVM, error) {
	if vm, ok := m.vms[id]; ok {
		return vm, nil
	}
	return nil, domain.ErrVMNotFound
}

func (m *mockVMUC) ListActiveVMs(ctx context.Context) ([]domain.MicroVM, error) {
	return nil, nil
}

func (m *mockVMUC) EnsurePoolSize(ctx context.Context, lang domain.Language, target int) error {
	return nil
}

type mockRunner struct{}

func (m *mockRunner) Run(ctx context.Context, vm *domain.MicroVM, exec *domain.Execution) (*RunResult, error) {
	return &RunResult{
		ExitCode: 0,
		Stdout:   "Hello, World!\n",
		Stderr:   "",
	}, nil
}

// --- Tests ---

func TestCreateExecution_Success(t *testing.T) {
	svc := NewExecutionService(newMockExecRepo(), newMockMetricsRepo(), newMockVMUC(), &mockRunner{})

	exec, err := svc.CreateExecution(context.Background(), CreateExecutionInput{
		OrganizationID: uuid.New(),
		RequestedBy:    uuid.New(),
		Language:       domain.LanguagePython,
		Code:           "print('hello')",
	})
	if err != nil {
		t.Fatalf("CreateExecution() error: %v", err)
	}
	if exec.ID == uuid.Nil {
		t.Error("Expected non-nil execution ID")
	}
	if exec.Status != domain.ExecutionStatusPending {
		t.Errorf("Expected pending status, got %s", exec.Status)
	}
	if exec.Resources.VCPU != 1 {
		t.Errorf("Expected default VCPU 1, got %d", exec.Resources.VCPU)
	}
}

func TestCreateExecution_InvalidLanguage(t *testing.T) {
	svc := NewExecutionService(newMockExecRepo(), newMockMetricsRepo(), newMockVMUC(), &mockRunner{})

	_, err := svc.CreateExecution(context.Background(), CreateExecutionInput{
		OrganizationID: uuid.New(),
		RequestedBy:    uuid.New(),
		Language:       domain.Language("cobol"),
		Code:           "code",
	})
	if err != domain.ErrInvalidLanguage {
		t.Errorf("Expected ErrInvalidLanguage, got %v", err)
	}
}

func TestCreateExecution_EmptyCode(t *testing.T) {
	svc := NewExecutionService(newMockExecRepo(), newMockMetricsRepo(), newMockVMUC(), &mockRunner{})

	_, err := svc.CreateExecution(context.Background(), CreateExecutionInput{
		OrganizationID: uuid.New(),
		RequestedBy:    uuid.New(),
		Language:       domain.LanguagePython,
		Code:           "",
	})
	if err != domain.ErrEmptyCode {
		t.Errorf("Expected ErrEmptyCode, got %v", err)
	}
}

func TestCreateExecution_CustomResources(t *testing.T) {
	svc := NewExecutionService(newMockExecRepo(), newMockMetricsRepo(), newMockVMUC(), &mockRunner{})

	exec, err := svc.CreateExecution(context.Background(), CreateExecutionInput{
		OrganizationID: uuid.New(),
		RequestedBy:    uuid.New(),
		Language:       domain.LanguageGo,
		Code:           "package main",
		Resources:      &domain.ResourceLimit{VCPU: 2, MemoryMB: 512, TimeoutSec: 60},
	})
	if err != nil {
		t.Fatalf("CreateExecution() error: %v", err)
	}
	if exec.Resources.VCPU != 2 {
		t.Errorf("Expected VCPU 2, got %d", exec.Resources.VCPU)
	}
	if exec.Resources.MemoryMB != 512 {
		t.Errorf("Expected Memory 512, got %d", exec.Resources.MemoryMB)
	}
}

func TestGetExecution_NotFound(t *testing.T) {
	svc := NewExecutionService(newMockExecRepo(), newMockMetricsRepo(), newMockVMUC(), &mockRunner{})

	_, err := svc.GetExecution(context.Background(), uuid.New())
	if err != domain.ErrExecutionNotFound {
		t.Errorf("Expected ErrExecutionNotFound, got %v", err)
	}
}

func TestCancelExecution_Success(t *testing.T) {
	repo := newMockExecRepo()
	svc := NewExecutionService(repo, newMockMetricsRepo(), newMockVMUC(), &mockRunner{})

	exec, _ := svc.CreateExecution(context.Background(), CreateExecutionInput{
		OrganizationID: uuid.New(),
		RequestedBy:    uuid.New(),
		Language:       domain.LanguagePython,
		Code:           "print('test')",
	})

	err := svc.CancelExecution(context.Background(), exec.ID)
	if err != nil {
		t.Fatalf("CancelExecution() error: %v", err)
	}

	cancelled, _ := svc.GetExecution(context.Background(), exec.ID)
	if cancelled.Status != domain.ExecutionStatusCancelled {
		t.Errorf("Expected cancelled status, got %s", cancelled.Status)
	}
}

func TestCancelExecution_AlreadyDone(t *testing.T) {
	repo := newMockExecRepo()
	svc := NewExecutionService(repo, newMockMetricsRepo(), newMockVMUC(), &mockRunner{})

	exec, _ := svc.CreateExecution(context.Background(), CreateExecutionInput{
		OrganizationID: uuid.New(),
		RequestedBy:    uuid.New(),
		Language:       domain.LanguagePython,
		Code:           "print('test')",
	})

	// First cancel
	_ = svc.CancelExecution(context.Background(), exec.ID)

	// Second cancel should fail
	err := svc.CancelExecution(context.Background(), exec.ID)
	if err != domain.ErrExecutionAlreadyDone {
		t.Errorf("Expected ErrExecutionAlreadyDone, got %v", err)
	}
}

func TestListExecutions(t *testing.T) {
	svc := NewExecutionService(newMockExecRepo(), newMockMetricsRepo(), newMockVMUC(), &mockRunner{})
	orgID := uuid.New()

	for i := 0; i < 3; i++ {
		_, _ = svc.CreateExecution(context.Background(), CreateExecutionInput{
			OrganizationID: orgID,
			RequestedBy:    uuid.New(),
			Language:       domain.LanguagePython,
			Code:           "print('hello')",
		})
	}

	execs, total, err := svc.ListExecutions(context.Background(), orgID, 10, 0)
	if err != nil {
		t.Fatalf("ListExecutions() error: %v", err)
	}
	if len(execs) != 3 || total != 3 {
		t.Errorf("Expected 3 executions, got %d (total %d)", len(execs), total)
	}
}

func TestProcessPending(t *testing.T) {
	repo := newMockExecRepo()
	svc := NewExecutionService(repo, newMockMetricsRepo(), newMockVMUC(), &mockRunner{})

	for i := 0; i < 2; i++ {
		_, _ = svc.CreateExecution(context.Background(), CreateExecutionInput{
			OrganizationID: uuid.New(),
			RequestedBy:    uuid.New(),
			Language:       domain.LanguagePython,
			Code:           "print('hello')",
		})
	}

	processed, err := svc.ProcessPending(context.Background(), 10)
	if err != nil {
		t.Fatalf("ProcessPending() error: %v", err)
	}
	if processed != 2 {
		t.Errorf("Expected 2 processed, got %d", processed)
	}
}
