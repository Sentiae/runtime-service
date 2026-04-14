package domain

import (
	"time"

	"github.com/google/uuid"
)

// RuntimeAgent is a customer-hosted executor that a Sentiae-deployed
// runtime-service can dispatch work to. Each record is one registered
// vm-agent instance — typically one per customer VPC. The org chooses
// which agents receive which executions via AgentRoutingPolicy.
type RuntimeAgent struct {
	ID             uuid.UUID `json:"id" gorm:"type:uuid;primary_key"`
	OrganizationID uuid.UUID `json:"organization_id" gorm:"type:uuid;not null;index"`
	Name           string    `json:"name" gorm:"type:varchar(200);not null"`
	Endpoint       string    `json:"endpoint" gorm:"type:varchar(500);not null"` // e.g. "grpcs://agent.customer.example:8443"
	// TokenHash is the argon2/bcrypt hash of the bearer token the
	// agent presents on every gRPC call. The plaintext is shown to the
	// operator exactly once at registration time and never stored.
	TokenHash string `json:"-" gorm:"type:varchar(200);not null"`
	// Fingerprint is the hex-encoded SHA-256 of the agent's TLS leaf
	// certificate. Set on first successful connection (TOFU) and
	// verified on every subsequent connection to pin the identity.
	Fingerprint  string            `json:"fingerprint,omitempty" gorm:"type:varchar(100)"`
	Status       AgentStatus       `json:"status" gorm:"type:varchar(20);not null;default:'pending'"`
	Capabilities []string          `json:"capabilities" gorm:"type:jsonb;serializer:json"` // e.g. ["go","python","gpu"]
	Labels       map[string]string `json:"labels" gorm:"type:jsonb;serializer:json"`
	LastSeenAt   *time.Time        `json:"last_seen_at,omitempty"`
	LastError    string            `json:"last_error,omitempty" gorm:"type:text"`
	CreatedAt    time.Time         `json:"created_at" gorm:"not null"`
	UpdatedAt    time.Time         `json:"updated_at" gorm:"not null"`
}

func (RuntimeAgent) TableName() string { return "runtime_agents" }

// AgentStatus is the connection state visible to operators. Online
// means the agent has checked in within the heartbeat window; stale
// means it was online but missed heartbeats; pending is the window
// between registration and first connect.
type AgentStatus string

const (
	AgentStatusPending AgentStatus = "pending"
	AgentStatusOnline  AgentStatus = "online"
	AgentStatusStale   AgentStatus = "stale"
	AgentStatusOffline AgentStatus = "offline"
)

// AgentHeartbeatTimeout is the window after the last heartbeat before
// an agent is demoted from online → stale. Two missed heartbeats flip
// stale → offline. Keeping both transitions explicit lets operators
// distinguish "networks flaking" from "agent gone".
const AgentHeartbeatTimeout = 60 * time.Second

// AgentRoutingPolicy is per-org configuration of which agent (if any)
// receives a given execution. Kept minimal: a single PreferredAgentID
// per organization is the common case (a customer wants their work on
// their VPC agent, not ours). Label selectors are the future extension
// point when orgs need multi-region or workload-class routing.
type AgentRoutingPolicy struct {
	OrganizationID   uuid.UUID  `json:"organization_id" gorm:"type:uuid;primary_key"`
	PreferredAgentID *uuid.UUID `json:"preferred_agent_id,omitempty" gorm:"type:uuid"`
	// FallbackToLocal controls what happens when the preferred agent
	// is offline: true → execute on Sentiae-hosted Firecracker; false
	// → return error so the caller can retry later. Customers with
	// strict data-residency requirements set this to false.
	FallbackToLocal bool              `json:"fallback_to_local" gorm:"default:true"`
	LabelSelectors  map[string]string `json:"label_selectors,omitempty" gorm:"type:jsonb;serializer:json"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

func (AgentRoutingPolicy) TableName() string { return "agent_routing_policies" }
