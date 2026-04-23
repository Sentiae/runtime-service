package usecase

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/domain"
)

// CustomerFirecrackerClient is the minimal shape VMRouterProvider
// needs to drive a customer-hosted Firecracker HTTP API. The
// concrete mTLS implementation lives at
// `internal/infrastructure/firecracker_customer`; the DI container
// wires that adapter in when the three APP_FIRECRACKER_CUSTOMER_*
// env vars are populated. Tests and local dev can inject any value
// that satisfies this interface. §9.4 (C15 landed 2026-04-17).
type CustomerFirecrackerClient interface {
	Boot(ctx context.Context, apiURL string, config VMBootConfig) (*VMBootResult, error)
	Terminate(ctx context.Context, apiURL, socketPath string, pid int) error
	Pause(ctx context.Context, apiURL, socketPath string) error
	Resume(ctx context.Context, apiURL, socketPath string) error
	CreateSnapshot(ctx context.Context, apiURL, socketPath string, snapshotID uuid.UUID) (*SnapshotResult, error)
	RestoreSnapshot(ctx context.Context, apiURL, socketPath, memPath, statePath string) error
	CollectMetrics(ctx context.Context, apiURL, ip string) (*VMMetrics, error)
	DeleteSnapshotFiles(apiURL, memPath, statePath string) error
}

// DeploymentTargetResolver resolves the deployment target for a VM
// when only a lightweight handle (socket path or IP) is available on
// the call — i.e. every post-boot lifecycle op. Implementations
// typically join on VMInstance → DeploymentTarget. Returning nil
// signals "no target" and the router falls back to local.
//
// BySocket is used by Terminate/Pause/Resume/CreateSnapshot/
// RestoreSnapshot/DeleteSnapshotFiles. ByIP is used by CollectMetrics.
type DeploymentTargetResolver interface {
	TargetBySocketPath(ctx context.Context, socketPath string) (*domain.DeploymentTarget, error)
	TargetByIP(ctx context.Context, ip string) (*domain.DeploymentTarget, error)
}

// VMRouterProvider inspects VMBootConfig.DeploymentTarget and routes
// the call to the local (sentiae_hosted) provider, a customer-hosted
// Firecracker HTTP API, or a customer-hosted vm-agent. If no target
// is set the local provider handles the call, matching the pre-§9.4
// behaviour.
//
// Post-boot lifecycle calls (Terminate/Pause/Resume/Snapshot/Restore/
// Metrics) consult the optional targetResolver to look up a
// DeploymentTarget by socketPath or IP; when no resolver is wired the
// router keeps the pre-§9.4 local-only behaviour.
type VMRouterProvider struct {
	local          VMProvider
	customer       CustomerFirecrackerClient
	agent          CustomerFirecrackerClient
	targetResolver DeploymentTargetResolver
}

// NewVMRouterProvider wires the router. Either customer or agent may
// be nil when the corresponding feature is disabled — the router
// surfaces a typed error when a target asks for a missing adapter so
// the caller sees why the boot failed.
func NewVMRouterProvider(local VMProvider, customer CustomerFirecrackerClient) *VMRouterProvider {
	return &VMRouterProvider{local: local, customer: customer}
}

// WithAgentClient injects the vm-agent adapter. Returns the same
// router so DI code reads linearly. Passing a nil client leaves the
// router in agent-disabled mode.
func (r *VMRouterProvider) WithAgentClient(agent CustomerFirecrackerClient) *VMRouterProvider {
	r.agent = agent
	return r
}

// WithTargetResolver wires the deployment-target resolver used by
// post-boot lifecycle calls (where only socketPath/IP is available).
// Passing nil leaves the router in local-only mode, matching the
// original behaviour.
func (r *VMRouterProvider) WithTargetResolver(resolver DeploymentTargetResolver) *VMRouterProvider {
	r.targetResolver = resolver
	return r
}

// clientForTarget picks the active client + URL for a resolved
// target. Mirrors resolveTarget but skips the nil-config branch; the
// caller first checks that a target exists.
func (r *VMRouterProvider) clientForTarget(t *domain.DeploymentTarget) (CustomerFirecrackerClient, string, error) {
	if t == nil {
		return nil, "", nil
	}
	switch t.Mode {
	case domain.DeploymentTargetSentiaeHosted, "":
		return nil, "", nil
	case domain.DeploymentTargetCustomerHosted:
		if r.customer == nil {
			return nil, "", fmt.Errorf("customer firecracker client not configured")
		}
		if t.CustomerAPIURL == "" {
			return nil, "", fmt.Errorf("customer_hosted target missing CustomerAPIURL")
		}
		return r.customer, t.CustomerAPIURL, nil
	case domain.DeploymentTargetAgent:
		if r.agent == nil {
			return nil, "", fmt.Errorf("vm-agent client not configured")
		}
		if t.AgentAPIURL == "" {
			return nil, "", fmt.Errorf("agent target missing AgentAPIURL")
		}
		return r.agent, t.AgentAPIURL, nil
	}
	return nil, "", fmt.Errorf("unknown deployment target mode %q", t.Mode)
}

// lookupBySocket resolves a target using the configured resolver. A
// nil resolver (tests, local-only deployments) returns nil so the
// caller falls back to r.local.
func (r *VMRouterProvider) lookupBySocket(ctx context.Context, socketPath string) *domain.DeploymentTarget {
	if r.targetResolver == nil || socketPath == "" {
		return nil
	}
	t, err := r.targetResolver.TargetBySocketPath(ctx, socketPath)
	if err != nil || t == nil {
		return nil
	}
	return t
}

// lookupByIP is the CollectMetrics counterpart.
func (r *VMRouterProvider) lookupByIP(ctx context.Context, ip string) *domain.DeploymentTarget {
	if r.targetResolver == nil || ip == "" {
		return nil
	}
	t, err := r.targetResolver.TargetByIP(ctx, ip)
	if err != nil || t == nil {
		return nil
	}
	return t
}

// resolveTarget picks the active client + URL. Returns (nil, "", nil)
// for the local path so the caller knows to use r.local.
func (r *VMRouterProvider) resolveTarget(cfg *VMBootConfig) (CustomerFirecrackerClient, string, error) {
	if cfg == nil || cfg.DeploymentTarget == nil {
		return nil, "", nil
	}
	t := cfg.DeploymentTarget
	switch t.Mode {
	case domain.DeploymentTargetSentiaeHosted, "":
		return nil, "", nil
	case domain.DeploymentTargetCustomerHosted:
		if r.customer == nil {
			return nil, "", fmt.Errorf("customer firecracker client not configured")
		}
		if t.CustomerAPIURL == "" {
			return nil, "", fmt.Errorf("customer_hosted target missing CustomerAPIURL")
		}
		return r.customer, t.CustomerAPIURL, nil
	case domain.DeploymentTargetAgent:
		if r.agent == nil {
			return nil, "", fmt.Errorf("vm-agent client not configured")
		}
		if t.AgentAPIURL == "" {
			return nil, "", fmt.Errorf("agent target missing AgentAPIURL")
		}
		return r.agent, t.AgentAPIURL, nil
	}
	return nil, "", fmt.Errorf("unknown deployment target mode %q", t.Mode)
}

func (r *VMRouterProvider) Boot(ctx context.Context, config VMBootConfig) (*VMBootResult, error) {
	client, apiURL, err := r.resolveTarget(&config)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return r.local.Boot(ctx, config)
	}
	return client.Boot(ctx, apiURL, config)
}

func (r *VMRouterProvider) Terminate(ctx context.Context, socketPath string, pid int) error {
	target := r.lookupBySocket(ctx, socketPath)
	client, apiURL, err := r.clientForTarget(target)
	if err != nil {
		// Fail-safe: fall through to local when resolver disagrees
		// with the configured clients. A stricter posture would
		// surface the error; the current contract prefers availability.
		return r.local.Terminate(ctx, socketPath, pid)
	}
	if client == nil {
		return r.local.Terminate(ctx, socketPath, pid)
	}
	return client.Terminate(ctx, apiURL, socketPath, pid)
}

func (r *VMRouterProvider) Pause(ctx context.Context, socketPath string) error {
	target := r.lookupBySocket(ctx, socketPath)
	client, apiURL, err := r.clientForTarget(target)
	if err != nil {
		return r.local.Pause(ctx, socketPath)
	}
	if client == nil {
		return r.local.Pause(ctx, socketPath)
	}
	return client.Pause(ctx, apiURL, socketPath)
}

func (r *VMRouterProvider) Resume(ctx context.Context, socketPath string) error {
	target := r.lookupBySocket(ctx, socketPath)
	client, apiURL, err := r.clientForTarget(target)
	if err != nil {
		return r.local.Resume(ctx, socketPath)
	}
	if client == nil {
		return r.local.Resume(ctx, socketPath)
	}
	return client.Resume(ctx, apiURL, socketPath)
}

func (r *VMRouterProvider) CreateSnapshot(ctx context.Context, socketPath string, snapshotID uuid.UUID) (*SnapshotResult, error) {
	target := r.lookupBySocket(ctx, socketPath)
	client, apiURL, err := r.clientForTarget(target)
	if err != nil {
		return r.local.CreateSnapshot(ctx, socketPath, snapshotID)
	}
	if client == nil {
		return r.local.CreateSnapshot(ctx, socketPath, snapshotID)
	}
	return client.CreateSnapshot(ctx, apiURL, socketPath, snapshotID)
}

func (r *VMRouterProvider) RestoreSnapshot(ctx context.Context, socketPath, memPath, statePath string) error {
	target := r.lookupBySocket(ctx, socketPath)
	client, apiURL, err := r.clientForTarget(target)
	if err != nil {
		return r.local.RestoreSnapshot(ctx, socketPath, memPath, statePath)
	}
	if client == nil {
		return r.local.RestoreSnapshot(ctx, socketPath, memPath, statePath)
	}
	return client.RestoreSnapshot(ctx, apiURL, socketPath, memPath, statePath)
}

func (r *VMRouterProvider) CollectMetrics(ctx context.Context, ip string) (*VMMetrics, error) {
	target := r.lookupByIP(ctx, ip)
	client, apiURL, err := r.clientForTarget(target)
	if err != nil {
		return r.local.CollectMetrics(ctx, ip)
	}
	if client == nil {
		return r.local.CollectMetrics(ctx, ip)
	}
	return client.CollectMetrics(ctx, apiURL, ip)
}

func (r *VMRouterProvider) DeleteSnapshotFiles(memPath, statePath string) error {
	// DeleteSnapshotFiles takes only raw file paths — no socket/IP
	// handle to key a target lookup on. On customer-hosted or agent
	// targets the files live on the remote host, not ours. Until the
	// interface surfaces a VM handle, continue delegating to the
	// local provider here; the reconciler will issue a remote delete
	// on the next pass when the VMInstance row carries the target.
	return r.local.DeleteSnapshotFiles(memPath, statePath)
}

// Compile-time check: VMRouterProvider satisfies the provider
// interface, so the DI container can swap it in transparently.
var _ VMProvider = (*VMRouterProvider)(nil)
