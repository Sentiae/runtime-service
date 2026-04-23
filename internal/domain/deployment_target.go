package domain

import (
	"time"

	"github.com/google/uuid"
)

// DeploymentTargetMode identifies where a Firecracker VM actually
// runs. (§9.4)
//
// The runtime started as a "Sentiae runs Firecracker on its own
// hosts" system; the customer-hosted + agent modes let enterprise
// tenants keep their sandbox VMs entirely on their infrastructure
// while still driving them via this service's API.
type DeploymentTargetMode string

const (
	// DeploymentTargetSentiaeHosted is the default — VMs run on
	// Sentiae-managed Firecracker hosts.
	DeploymentTargetSentiaeHosted DeploymentTargetMode = "sentiae_hosted"
	// DeploymentTargetCustomerHosted means the customer runs
	// Firecracker on their own infrastructure; VMService proxies
	// lifecycle calls to their HTTP API endpoint.
	DeploymentTargetCustomerHosted DeploymentTargetMode = "customer_hosted"
	// DeploymentTargetAgent uses an in-guest agent for long-running
	// sandboxes (e.g. customer laptops, air-gapped build hosts).
	// Placeholder for the follow-up wave of work.
	DeploymentTargetAgent DeploymentTargetMode = "agent"
)

// IsValid reports whether the mode string is recognised.
func (m DeploymentTargetMode) IsValid() bool {
	switch m {
	case DeploymentTargetSentiaeHosted,
		DeploymentTargetCustomerHosted,
		DeploymentTargetAgent:
		return true
	}
	return false
}

// DeploymentTarget binds an organization (or environment) to a
// specific execution surface. Minimal first pass — enough to let
// VMService route to a customer Firecracker endpoint based on the
// tenant. Real provisioning (TAP orchestration, jailer install,
// mTLS cert exchange) is a cross-cutting effort tracked in the
// §9.4 follow-up plan.
type DeploymentTarget struct {
	ID             uuid.UUID            `json:"id" gorm:"type:uuid;primary_key"`
	OrganizationID uuid.UUID            `json:"organization_id" gorm:"type:uuid;not null;index"`
	Name           string               `json:"name" gorm:"type:varchar(200);not null"`
	Mode           DeploymentTargetMode `json:"mode" gorm:"type:varchar(40);not null;default:'sentiae_hosted';index"`

	// CustomerAPIURL is the base URL of the customer-hosted
	// Firecracker HTTP API. Required when Mode == customer_hosted.
	// TODO(§9.4): swap the raw URL for a typed adapter config once
	// auth + cert rotation land. See platform-kit/customer-firecracker.
	CustomerAPIURL string `json:"customer_api_url,omitempty" gorm:"type:varchar(500)"`

	// CustomerAuthMode + CustomerCredentialRef are the minimal
	// auth hooks. Everything else (mTLS, per-call signing) plugs
	// in via Metadata until the full adapter exists.
	CustomerAuthMode      string     `json:"customer_auth_mode,omitempty" gorm:"column:customer_auth_mode;type:varchar(40)"`
	CustomerCredentialRef *uuid.UUID `json:"customer_credential_ref,omitempty" gorm:"column:customer_credential_ref;type:uuid"`

	// AgentAPIURL is the base URL of the customer-hosted vm-agent's
	// HTTP control-plane wrapper. Required when Mode == agent. The
	// agent is expected to expose POST /boot, /exec, /snapshot,
	// /terminate, /pause, /resume, /restore — runtime-service's
	// vmagent client speaks that contract and does nothing else.
	AgentAPIURL string `json:"agent_api_url,omitempty" gorm:"column:agent_api_url;type:varchar(500)"`
	// AgentAuthToken is the bearer token that runtime-service sends
	// on every call to the agent. Stored as opaque string here; the
	// credential manager is expected to inject the plaintext via the
	// DI container at startup (same pattern as the RemoteExecutor
	// TokenProvider). When empty the client fails closed.
	AgentAuthToken string `json:"-" gorm:"column:agent_auth_token;type:varchar(500)"`

	// Metadata is free-form config for the target (e.g. AWS
	// region, kernel override, pool size). Narrow schema today so
	// the domain stays forgiving as the feature grows.
	Metadata JSONMap `json:"metadata,omitempty" gorm:"type:jsonb"`

	CreatedAt time.Time `json:"created_at" gorm:"not null"`
	UpdatedAt time.Time `json:"updated_at" gorm:"not null"`
}

// TableName pins the GORM table.
func (DeploymentTarget) TableName() string { return "deployment_targets" }

// Validate performs minimal validation: the mode must be a known
// enum and customer_hosted targets must have an API URL.
func (t *DeploymentTarget) Validate() error {
	if t.ID == uuid.Nil {
		return ErrInvalidID
	}
	if t.OrganizationID == uuid.Nil {
		return ErrInvalidID
	}
	if !t.Mode.IsValid() {
		return ErrInvalidData
	}
	if t.Mode == DeploymentTargetCustomerHosted && t.CustomerAPIURL == "" {
		return ErrInvalidData
	}
	if t.Mode == DeploymentTargetAgent && t.AgentAPIURL == "" {
		return ErrInvalidData
	}
	return nil
}
