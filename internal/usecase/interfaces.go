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

// ─────────────────────────────────────────────────────────────────────
// NetworkEnforcer port — the host-level realization seam for P21 on the
// fleet (CP4.5 §9 #5). Implemented by internal/infrastructure/netfabric
// (iptables); fail-loud off the firecracker host.
// ─────────────────────────────────────────────────────────────────────

// NetworkEnforcer realizes fleet network policy on the host. The real
// implementation owns the whole FORWARD program (see the netfabric package doc);
// FailLoudNetworkEnforcer rejects every call so a workload is NEVER booted into
// an unenforced network on a host that cannot enforce.
type NetworkEnforcer interface {
	// InstallSkeleton writes the complete, ordered chain program. It must converge
	// from any starting state — kernel rules survive a process restart, so a clean
	// slate is never assumed.
	InstallSkeleton(ctx context.Context) error
	// AssertPosture PROVES the installed program matches the intended one exactly.
	// Called at DI init before anything serves, and again before any provision that
	// depends on it. An error must prevent this host from serving system-scoped
	// workloads — never a warning, never a metric. A refusal.
	AssertPosture(ctx context.Context) error
	// SyncSystem atomically rebuilds ONE system's allow chain from the resolved
	// rules. Whole-chain flush+refill, never incremental — idempotent + self-healing.
	SyncSystem(ctx context.Context, networkID uuid.UUID, rules []ResolvedRule) error
	// DropSystem removes a system's chain entirely (Deprovision / teardown).
	DropSystem(ctx context.Context, networkID uuid.UUID) error
}

// ResolvedRule is one compiled policy with live guest IPs substituted in. Empty
// SrcIP or DstIP is impossible by construction: the resolver EMITS NO RULE for a
// policy whose endpoint has no live replica — there is no wildcard substitute.
type ResolvedRule struct {
	SrcIP    string // a live replica guest IP (rendered /32 by the enforcer)
	DstIP    string // a live replica guest IP (rendered /32 by the enforcer)
	Protocol string // "tcp" — never defaulted; an empty value is rejected
	Port     int    // 1..65535 — 0 is an error, never "any"
}

// FailLoudNetworkEnforcer is wired when the executor is not firecracker, or when
// the real enforcer could not install/prove its program at boot. Every call fails
// with ErrNetworkEnforcerUnavailable so the fleet never silently serves an
// unenforced network (mirrors FailLoudImageBooter / FailLoudVolumeBackend).
//
// The forbidden shape here is `if !firecracker { skip policies; boot anyway }` —
// that is a control degrading to permissive because its dependency is absent,
// which is the platform's default failure mode and the reason this type exists.
type FailLoudNetworkEnforcer struct{}

var _ NetworkEnforcer = FailLoudNetworkEnforcer{}

func (FailLoudNetworkEnforcer) InstallSkeleton(context.Context) error {
	return domain.ErrNetworkEnforcerUnavailable
}
func (FailLoudNetworkEnforcer) AssertPosture(context.Context) error {
	return domain.ErrNetworkEnforcerUnavailable
}
func (FailLoudNetworkEnforcer) SyncSystem(context.Context, uuid.UUID, []ResolvedRule) error {
	return domain.ErrNetworkEnforcerUnavailable
}
func (FailLoudNetworkEnforcer) DropSystem(context.Context, uuid.UUID) error {
	return domain.ErrNetworkEnforcerUnavailable
}
