// Package usecase — RuntimeAgent registry + dispatcher for customer-
// hosted Firecracker agents (vm-agent).
//
// Architecture:
//
//	orchestrator ─▶ Dispatcher.Execute(orgID, req)
//	                     │
//	                     ├─ lookup AgentRoutingPolicy(orgID)
//	                     │   └─ preferred_agent_id? → remote path
//	                     │
//	                     ├─ remote path: AgentRegistry.Resolve(agentID)
//	                     │   └─ RemoteExecutor.Dispatch(agent, req)
//	                     │
//	                     └─ local path: LocalExecutor.Execute(req)
//
// The registry is the single source of truth for agent metadata. The
// dispatcher stays ignorant of how remote agents are talked to —
// RemoteExecutor is an interface so tests inject fakes and a real
// gRPC-over-mTLS client plugs in without touching routing logic.
package usecase

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ErrNoPreferredAgent is returned when the org has a routing policy
// that forbids local fallback but no online agent is available.
var ErrNoPreferredAgent = errors.New("preferred runtime agent is offline and fallback_to_local is disabled")

// AgentRepository is the storage contract the registry needs. The real
// implementation is a GORM repo; tests use an in-memory map.
type AgentRepository interface {
	Create(ctx context.Context, a *domain.RuntimeAgent) error
	Update(ctx context.Context, a *domain.RuntimeAgent) error
	Get(ctx context.Context, id uuid.UUID) (*domain.RuntimeAgent, error)
	GetPolicy(ctx context.Context, orgID uuid.UUID) (*domain.AgentRoutingPolicy, error)
}

// RemoteExecutor is implemented by the gRPC client that forwards an
// execution request to a registered vm-agent. Keeping this an
// interface in the usecase layer means the dispatcher can be unit
// tested without a live gRPC stack.
type RemoteExecutor interface {
	Dispatch(ctx context.Context, agent *domain.RuntimeAgent, payload ExecutionPayload) (ExecutionResult, error)
}

// LocalExecutor mirrors the contract for the Sentiae-hosted Firecracker
// provider. The existing firecracker.Provider is wrapped to satisfy
// this interface in wiring.
type LocalExecutor interface {
	Execute(ctx context.Context, payload ExecutionPayload) (ExecutionResult, error)
}

// ExecutionPayload is the language-agnostic subset the dispatcher
// needs to route. Higher layers (graph executor, test runner) build
// this from their richer domain types.
type ExecutionPayload struct {
	Language  string
	Code      string
	Args      []string
	Env       map[string]string
	TimeoutMS int
}

// ExecutionResult carries the stdout/stderr/exit-code shape every
// executor returns. Adapters marshal their own protocols into this
// before returning.
type ExecutionResult struct {
	ExitCode   int
	Stdout     string
	Stderr     string
	DurationMS int64
	AgentID    *uuid.UUID // set when the call was served by a remote agent
}

// AgentRegistry owns registration, heartbeat, and status transitions.
// The dispatcher reads from it; HTTP handlers write to it.
type AgentRegistry struct {
	repo AgentRepository
}

func NewAgentRegistry(repo AgentRepository) *AgentRegistry {
	return &AgentRegistry{repo: repo}
}

// RegisterInput captures the operator-visible fields; TokenHash is
// derived inside Register so the plaintext never leaves the function.
type RegisterInput struct {
	OrganizationID uuid.UUID
	Name           string
	Endpoint       string
	Capabilities   []string
	Labels         map[string]string
}

// RegisterOutput returns the newly-minted agent plus the plaintext
// bearer token (shown ONCE to the operator for injection into the
// agent's startup config). The caller MUST NOT log this.
type RegisterOutput struct {
	Agent *domain.RuntimeAgent
	Token string
}

// Register creates a pending agent row and returns the secret token
// the agent will use for its first /heartbeat call. Status stays
// "pending" until that first successful heartbeat flips it to online.
func (r *AgentRegistry) Register(ctx context.Context, in RegisterInput) (*RegisterOutput, error) {
	if in.Endpoint == "" {
		return nil, fmt.Errorf("endpoint is required")
	}
	token, err := generateToken()
	if err != nil {
		return nil, err
	}
	hash := hashToken(token)
	agent := &domain.RuntimeAgent{
		ID:             uuid.New(),
		OrganizationID: in.OrganizationID,
		Name:           in.Name,
		Endpoint:       in.Endpoint,
		TokenHash:      hash,
		Capabilities:   in.Capabilities,
		Labels:         in.Labels,
		Status:         domain.AgentStatusPending,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}
	if err := r.repo.Create(ctx, agent); err != nil {
		return nil, err
	}
	return &RegisterOutput{Agent: agent, Token: token}, nil
}

// Heartbeat validates the token against the stored hash and flips the
// agent's status to online. Called on every agent check-in (typically
// every 15 seconds). Token mismatch returns an error and does NOT
// update last_seen, so a leaked-then-rotated token cannot keep the
// agent alive after revocation.
func (r *AgentRegistry) Heartbeat(ctx context.Context, agentID uuid.UUID, token string, fingerprint string) error {
	agent, err := r.repo.Get(ctx, agentID)
	if err != nil {
		return err
	}
	if agent == nil || agent.TokenHash != hashToken(token) {
		return fmt.Errorf("agent auth failed")
	}
	// First-seen fingerprint is pinned (trust on first use). A mismatch
	// after pinning indicates either legitimate rotation (operator must
	// clear the fingerprint) or a hijack; either way we refuse.
	if agent.Fingerprint == "" {
		agent.Fingerprint = fingerprint
	} else if fingerprint != "" && fingerprint != agent.Fingerprint {
		return fmt.Errorf("agent fingerprint mismatch")
	}
	now := time.Now().UTC()
	agent.LastSeenAt = &now
	agent.Status = domain.AgentStatusOnline
	agent.LastError = ""
	agent.UpdatedAt = now
	return r.repo.Update(ctx, agent)
}

// Resolve returns an online agent that matches the org's routing
// policy, or nil if no remote dispatch is configured. A nil return
// value with nil error means "run locally".
func (r *AgentRegistry) Resolve(ctx context.Context, orgID uuid.UUID) (*domain.RuntimeAgent, error) {
	policy, err := r.repo.GetPolicy(ctx, orgID)
	if err != nil || policy == nil || policy.PreferredAgentID == nil {
		return nil, nil
	}
	agent, err := r.repo.Get(ctx, *policy.PreferredAgentID)
	if err != nil || agent == nil {
		if policy.FallbackToLocal {
			return nil, nil
		}
		return nil, ErrNoPreferredAgent
	}
	if !isOnline(agent) {
		if policy.FallbackToLocal {
			return nil, nil
		}
		return nil, ErrNoPreferredAgent
	}
	return agent, nil
}

func isOnline(a *domain.RuntimeAgent) bool {
	if a.Status != domain.AgentStatusOnline || a.LastSeenAt == nil {
		return false
	}
	return time.Since(*a.LastSeenAt) < domain.AgentHeartbeatTimeout
}

// Dispatcher is the top-level entrypoint for executions that have not
// yet been routed. It consults the registry, then delegates to either
// the remote executor or the local one. Orchestrators should hold a
// *Dispatcher rather than talking to firecracker.Provider directly.
type Dispatcher struct {
	registry *AgentRegistry
	remote   RemoteExecutor
	local    LocalExecutor
}

func NewDispatcher(reg *AgentRegistry, remote RemoteExecutor, local LocalExecutor) *Dispatcher {
	return &Dispatcher{registry: reg, remote: remote, local: local}
}

// Execute picks a target and runs the payload. Returns the result
// plus the agent id (nil when executed locally) so higher layers can
// record "this run was served by agent X" for audit trails.
func (d *Dispatcher) Execute(ctx context.Context, orgID uuid.UUID, payload ExecutionPayload) (ExecutionResult, error) {
	agent, err := d.registry.Resolve(ctx, orgID)
	if err != nil {
		return ExecutionResult{}, err
	}
	if agent != nil && d.remote != nil {
		return d.remote.Dispatch(ctx, agent, payload)
	}
	if d.local == nil {
		return ExecutionResult{}, fmt.Errorf("no local executor configured and no agent available")
	}
	return d.local.Execute(ctx, payload)
}

// generateToken returns 32 bytes of random data as a hex string.
// 256 bits of entropy is enough for a bearer token; we avoid base64
// so tokens can be trivially put in YAML/env without escaping.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashToken returns the SHA-256 of the token. Bcrypt/argon2 would be
// preferable for user passwords, but agent tokens are machine-generated
// with full entropy — rainbow tables do not apply and SHA-256 gives us
// constant-time compare via subtle when needed.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
