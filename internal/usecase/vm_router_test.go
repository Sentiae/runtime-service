package usecase

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

type routerMockCustomer struct {
	bootCalls      int
	lastBootURL    string
	terminateCalls int
}

func (m *routerMockCustomer) Boot(_ context.Context, apiURL string, _ VMBootConfig) (*VMBootResult, error) {
	m.bootCalls++
	m.lastBootURL = apiURL
	return &VMBootResult{PID: 42}, nil
}

func (m *routerMockCustomer) Terminate(context.Context, string, string, int) error {
	m.terminateCalls++
	return nil
}

func (m *routerMockCustomer) Pause(context.Context, string, string) error  { return nil }
func (m *routerMockCustomer) Resume(context.Context, string, string) error { return nil }

func (m *routerMockCustomer) CreateSnapshot(context.Context, string, string, uuid.UUID) (*SnapshotResult, error) {
	return &SnapshotResult{}, nil
}
func (m *routerMockCustomer) RestoreSnapshot(context.Context, string, string, string, string) error {
	return nil
}
func (m *routerMockCustomer) CollectMetrics(context.Context, string, string) (*VMMetrics, error) {
	return &VMMetrics{}, nil
}
func (m *routerMockCustomer) DeleteSnapshotFiles(string, string, string) error { return nil }

type routerMockLocal struct {
	bootCalls int
}

func (m *routerMockLocal) Boot(_ context.Context, _ VMBootConfig) (*VMBootResult, error) {
	m.bootCalls++
	return &VMBootResult{PID: 1}, nil
}
func (m *routerMockLocal) Terminate(context.Context, string, int) error            { return nil }
func (m *routerMockLocal) Pause(context.Context, string) error                     { return nil }
func (m *routerMockLocal) Resume(context.Context, string) error                    { return nil }
func (m *routerMockLocal) DeleteSnapshotFiles(string, string) error                { return nil }
func (m *routerMockLocal) CollectMetrics(context.Context, string) (*VMMetrics, error) {
	return &VMMetrics{}, nil
}
func (m *routerMockLocal) CreateSnapshot(context.Context, string, uuid.UUID) (*SnapshotResult, error) {
	return &SnapshotResult{}, nil
}
func (m *routerMockLocal) RestoreSnapshot(context.Context, string, string, string) error {
	return nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestVMRouter_LocalWhenNoTarget(t *testing.T) {
	t.Parallel()

	local := &routerMockLocal{}
	r := NewVMRouterProvider(local, nil)
	_, err := r.Boot(context.Background(), VMBootConfig{})
	require.NoError(t, err)
	assert.Equal(t, 1, local.bootCalls)
}

func TestVMRouter_DispatchesAgentWhenModeAgent(t *testing.T) {
	t.Parallel()

	local := &routerMockLocal{}
	customer := &routerMockCustomer{}
	agent := &routerMockCustomer{}

	r := NewVMRouterProvider(local, customer).WithAgentClient(agent)

	_, err := r.Boot(context.Background(), VMBootConfig{
		DeploymentTarget: &domain.DeploymentTarget{
			Mode:        domain.DeploymentTargetAgent,
			AgentAPIURL: "https://agent.example.com",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, local.bootCalls)
	assert.Equal(t, 0, customer.bootCalls)
	assert.Equal(t, 1, agent.bootCalls)
	assert.Equal(t, "https://agent.example.com", agent.lastBootURL)
}

func TestVMRouter_AgentModeRequiresClient(t *testing.T) {
	t.Parallel()

	local := &routerMockLocal{}
	customer := &routerMockCustomer{}

	// Agent client explicitly NOT injected.
	r := NewVMRouterProvider(local, customer)

	_, err := r.Boot(context.Background(), VMBootConfig{
		DeploymentTarget: &domain.DeploymentTarget{
			Mode:        domain.DeploymentTargetAgent,
			AgentAPIURL: "https://agent.example.com",
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vm-agent client not configured")
}

func TestVMRouter_AgentModeRequiresURL(t *testing.T) {
	t.Parallel()

	local := &routerMockLocal{}
	agent := &routerMockCustomer{}
	r := NewVMRouterProvider(local, nil).WithAgentClient(agent)

	_, err := r.Boot(context.Background(), VMBootConfig{
		DeploymentTarget: &domain.DeploymentTarget{
			Mode: domain.DeploymentTargetAgent,
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent target missing AgentAPIURL")
}

func TestVMRouter_CustomerHostedStillWorks(t *testing.T) {
	t.Parallel()

	local := &routerMockLocal{}
	customer := &routerMockCustomer{}
	agent := &routerMockCustomer{}
	r := NewVMRouterProvider(local, customer).WithAgentClient(agent)

	_, err := r.Boot(context.Background(), VMBootConfig{
		DeploymentTarget: &domain.DeploymentTarget{
			Mode:           domain.DeploymentTargetCustomerHosted,
			CustomerAPIURL: "https://cust.example.com",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, customer.bootCalls)
	assert.Equal(t, 0, agent.bootCalls)
}
