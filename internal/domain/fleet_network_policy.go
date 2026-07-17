package domain

import (
	"time"

	"github.com/google/uuid"
)

// PolicyProtocolTCP is the only transport the fleet compiles this slice. An
// unrecognized (or empty) protocol is REJECTED, never defaulted to it.
const PolicyProtocolTCP = "tcp"

// FleetNetworkPolicy is ONE compiled arch edge: "component From may reach
// component To on Protocol/Port, inside NetworkID". It is DESIRED state keyed on
// COMPONENT IDs, never on IP addresses — a replica's guest IP is per-boot
// (image_boot.go allocIndex), so IPs are re-resolved from live replicas on every
// reconcile tick and never stored. It doubles as the GORM model; DDL is owned by
// golang-migrate (migrations/), NOT AutoMigrate.
type FleetNetworkPolicy struct {
	ID              uuid.UUID `json:"id" gorm:"type:uuid;primary_key"`
	NetworkID       uuid.UUID `json:"network_id" gorm:"type:uuid;not null;index"`
	FromComponentID string    `json:"from_component_id" gorm:"type:varchar(255);not null"`
	ToComponentID   string    `json:"to_component_id" gorm:"type:varchar(255);not null"`
	Protocol        string    `json:"protocol" gorm:"type:varchar(10);not null"`
	Port            int       `json:"port" gorm:"not null"`
	// DerivedFromEdgeID carries the arch edge this policy was compiled from (I13
	// provenance). Empty is tolerated — provenance is a trace, not a control.
	DerivedFromEdgeID string    `json:"derived_from_edge_id" gorm:"type:varchar(255);not null;default:''"`
	CreatedAt         time.Time `json:"created_at" gorm:"not null"`
}

// TableName specifies the GORM table name.
func (FleetNetworkPolicy) TableName() string {
	return "fleet_network_policies"
}

// Validate fails closed on every under-specified field. A policy that cannot be
// compiled into an EXACT rule is REJECTED — never widened, never defaulted.
//
// The Port bound is load-bearing: proto int32's zero value is 0, and the
// intuitive-looking permissive reading ("0 means any port") would turn an ABSENT
// field into a wildcard allow. It is an error here and a CHECK constraint in the
// migration, so it cannot exist at either layer.
func (p FleetNetworkPolicy) Validate() error {
	if p.FromComponentID == "" || p.ToComponentID == "" {
		return ErrInvalidNetworkPolicy
	}
	if p.Protocol != PolicyProtocolTCP {
		return ErrUnsupportedPolicyProtocol
	}
	if p.Port <= 0 || p.Port > 65535 {
		return ErrInvalidNetworkPolicy
	}
	return nil
}
