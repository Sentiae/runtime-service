package usecase

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ExecutionUseCase defines the interface for execution business logic
type ExecutionUseCase interface {
	// CreateExecution creates a new code execution request
	CreateExecution(ctx context.Context, input CreateExecutionInput) (*domain.Execution, error)

	// GetExecution returns an execution by ID
	GetExecution(ctx context.Context, id uuid.UUID) (*domain.Execution, error)

	// ListExecutions returns executions for an organization
	ListExecutions(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]domain.Execution, int64, error)

	// CancelExecution cancels a running or pending execution
	CancelExecution(ctx context.Context, id uuid.UUID) error

	// GetExecutionMetrics returns metrics for an execution
	GetExecutionMetrics(ctx context.Context, executionID uuid.UUID) (*domain.ExecutionMetrics, error)

	// ProcessPending picks up pending executions and runs them
	ProcessPending(ctx context.Context, limit int) (int, error)

	// ExecuteSync creates and synchronously executes a code execution,
	// returning the completed execution. Used by the graph engine for
	// code-type nodes.
	ExecuteSync(ctx context.Context, input CreateExecutionInput) (*domain.Execution, error)
}

// VMUseCase defines the interface for microVM management
type VMUseCase interface {
	// CreateVM creates a new microVM with specified resources
	CreateVM(ctx context.Context, language domain.Language, vcpu, memMB int) (*domain.MicroVM, error)

	// AcquireVM gets an available VM from the pool or creates one
	AcquireVM(ctx context.Context, language domain.Language, resources domain.ResourceLimit) (*domain.MicroVM, error)

	// ReleaseVM returns a VM to the ready pool
	ReleaseVM(ctx context.Context, vmID uuid.UUID) error

	// TerminateVM terminates a microVM
	TerminateVM(ctx context.Context, vmID uuid.UUID) error

	// GetVM returns a VM by ID
	GetVM(ctx context.Context, id uuid.UUID) (*domain.MicroVM, error)

	// ListActiveVMs returns all active VMs
	ListActiveVMs(ctx context.Context) ([]domain.MicroVM, error)

	// EnsurePoolSize ensures the warm pool has enough VMs
	EnsurePoolSize(ctx context.Context, language domain.Language, targetSize int) error
}

// SnapshotUseCase defines the interface for snapshot management
type SnapshotUseCase interface {
	// CreateSnapshot creates a snapshot of a running VM
	CreateSnapshot(ctx context.Context, vmID uuid.UUID, description string) (*domain.Snapshot, error)

	// RestoreSnapshot restores a VM from a snapshot
	RestoreSnapshot(ctx context.Context, snapshotID uuid.UUID) (*domain.MicroVM, error)

	// GetSnapshot returns a snapshot by ID
	GetSnapshot(ctx context.Context, id uuid.UUID) (*domain.Snapshot, error)

	// ListSnapshots returns snapshots for an execution
	ListSnapshotsByExecution(ctx context.Context, executionID uuid.UUID) ([]domain.Snapshot, error)

	// GetBaseSnapshot returns the base snapshot for a language
	GetBaseSnapshot(ctx context.Context, language domain.Language) (*domain.Snapshot, error)

	// DeleteSnapshot deletes a snapshot
	DeleteSnapshot(ctx context.Context, id uuid.UUID) error
}

// SchedulerUseCase defines the interface for VM placement scheduling
type SchedulerUseCase interface {
	// SelectHost picks the best host for a new VM with the given resource requirements
	SelectHost(ctx context.Context, vcpu int, memoryMB int) (*HostInfo, error)

	// RegisterHost adds a host to the scheduler registry
	RegisterHost(ctx context.Context, host HostInfo) error

	// DeregisterHost removes a host from the scheduler registry
	DeregisterHost(ctx context.Context, hostID string) error

	// ListHosts returns all registered hosts with current capacity
	ListHosts(ctx context.Context) ([]HostInfo, error)

	// UpdateHostUsage updates the resource usage for a host
	UpdateHostUsage(ctx context.Context, hostID string, usedVCPU int, usedMemoryMB int) error

	// ReleaseHostResources releases resources on a host when a VM is terminated
	ReleaseHostResources(ctx context.Context, hostID string, vcpu int, memoryMB int) error
}

// VMInstanceUseCase defines the interface for desired-state VM management
type VMInstanceUseCase interface {
	// GetVMInstance returns a VM instance by ID
	GetVMInstance(ctx context.Context, id uuid.UUID) (*domain.VMInstance, error)

	// ListVMInstances returns all VM instances with optional status filter
	ListVMInstances(ctx context.Context, statusFilter *domain.VMInstanceState) ([]domain.VMInstance, error)

	// SetDesiredState sets the desired state for a VM instance
	SetDesiredState(ctx context.Context, id uuid.UUID, desiredState domain.VMInstanceState) error

	// RequestTermination requests termination of a VM instance
	RequestTermination(ctx context.Context, id uuid.UUID) error

	// CreateVMInstance creates a new VM instance record
	CreateVMInstance(ctx context.Context, input CreateVMInstanceInput) (*domain.VMInstance, error)

	// Reconcile runs a single reconciliation pass
	Reconcile(ctx context.Context) error
}

// CreateVMInstanceInput represents the input for creating a VM instance
type CreateVMInstanceInput struct {
	ExecutionID  *uuid.UUID             `json:"execution_id,omitempty"`
	Language     domain.Language        `json:"language"`
	BaseImage    string                 `json:"base_image,omitempty"`
	DesiredState domain.VMInstanceState `json:"desired_state"`
	VCPU         int                    `json:"vcpu"`
	MemoryMB     int                    `json:"memory_mb"`
	DiskMB       int                    `json:"disk_mb"`
}

// HostInfo describes a host machine available for VM placement
type HostInfo struct {
	ID            string            `json:"id"`
	Address       string            `json:"address"`
	TotalVCPU     int               `json:"total_vcpu"`
	TotalMemMB    int               `json:"total_mem_mb"`
	UsedVCPU      int               `json:"used_vcpu"`
	UsedMemMB     int               `json:"used_mem_mb"`
	Labels        map[string]string `json:"labels,omitempty"`
	Available     bool              `json:"available"`
	LastHeartbeat time.Time         `json:"last_heartbeat"`
}

// AvailableVCPU returns the number of unused vCPUs on this host.
func (h HostInfo) AvailableVCPU() int {
	return h.TotalVCPU - h.UsedVCPU
}

// AvailableMemMB returns the unused memory in MB on this host.
func (h HostInfo) AvailableMemMB() int {
	return h.TotalMemMB - h.UsedMemMB
}

// CanFit returns true if the host has enough resources for the given requirements.
func (h HostInfo) CanFit(vcpu, memMB int) bool {
	return h.Available && h.AvailableVCPU() >= vcpu && h.AvailableMemMB() >= memMB
}

// SnapshotResult holds the result of a Firecracker snapshot creation
type SnapshotResult struct {
	MemoryFilePath string
	StateFilePath  string
	SizeBytes      int64
}

// VMMetrics holds resource usage metrics collected from a VM
type VMMetrics struct {
	CPUTimeMS    int64
	MemoryPeakMB float64
	MemoryAvgMB  float64
	IOReadBytes  int64
	IOWriteBytes int64
	NetBytesIn   int64
	NetBytesOut  int64
}

// CreateExecutionInput represents the input for creating an execution
type CreateExecutionInput struct {
	OrganizationID uuid.UUID             `json:"organization_id"`
	RequestedBy    uuid.UUID             `json:"requested_by"`
	NodeID         *uuid.UUID            `json:"node_id,omitempty"`
	WorkflowID     *uuid.UUID            `json:"workflow_id,omitempty"`
	Language       domain.Language       `json:"language"`
	Code           string                `json:"code"`
	Stdin          string                `json:"stdin,omitempty"`
	Args           domain.JSONMap        `json:"args,omitempty"`
	EnvVars        domain.JSONMap        `json:"env_vars,omitempty"`
	Resources      *domain.ResourceLimit `json:"resources,omitempty"`
}
