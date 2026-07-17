package domain

import (
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// FleetSubnetCIDR is the /16 the fleet's image-boot path carves a per-workload
// /30 out of on every replica boot. It is the fleet's whole address space: the
// network model is a POLICY SCOPE over these addresses, not a CIDR per system
// (D-164).
const FleetSubnetCIDR = "10.201.0.0/16"

// Parsed once at init. A parse failure cannot happen for a constant, but it is
// not asserted with a panic either: fleetSubnetErr makes InFleetSubnet answer
// false for everything, which emits no rules — the fail-closed direction.
var fleetSubnet, fleetSubnetErr = netip.ParsePrefix(FleetSubnetCIDR)

// InFleetSubnet reports whether ip is a bare IPv4 address inside the fleet
// subnet. Empty, malformed, and out-of-range all report false, so a poisoned or
// absent guest_ip can never be compiled into a rule.
func InFleetSubnet(ip string) bool {
	if fleetSubnetErr != nil {
		return false
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is4() {
		return false
	}
	return fleetSubnet.Contains(addr)
}

// CIDRWithinFleetSubnet reports whether a CIDR entry names fleet-internal
// address space — i.e. the fleet subnet itself ("10.201.0.0/16") or a slice of it
// ("10.201.5.0/24").
//
// A SUPERNET ("10.0.0.0/8", "0.0.0.0/0") reports false and is therefore allowed.
// That is deliberate and narrow: a supernet is a legitimate external destination
// (a customer's own 10/8), and it cannot buy inter-VM reach anyway — SNT-XVM is
// terminal for any flow with both ends in the fleet subnet and is evaluated
// before SNT-EGRESS, so a `-d 10.0.0.0/8 -j ACCEPT` in a per-tap egress chain is
// never reached by inter-VM traffic. The structural guard, not this check, is
// what makes that safe. (See the report: whether a supernet should ALSO be
// refused at the seam is an open question, not something to decide by widening a
// validator.)
//
// A non-CIDR entry (a hostname, a bare IP) reports false: this answers one
// question only, and its caller asks the bare-IP question separately.
func CIDRWithinFleetSubnet(cidr string) bool {
	if fleetSubnetErr != nil {
		return false
	}
	p, err := netip.ParsePrefix(cidr)
	if err != nil || !p.Addr().Is4() {
		return false
	}
	return p.Bits() >= fleetSubnet.Bits() && fleetSubnet.Contains(p.Addr())
}

// FleetNetworkStatus is the lifecycle state of a fleet network.
type FleetNetworkStatus string

const (
	FleetNetworkActive        FleetNetworkStatus = "active"
	FleetNetworkDeprovisioned FleetNetworkStatus = "deprovisioned"
)

// IsValid reports whether the status is one the fleet recognizes.
func (s FleetNetworkStatus) IsValid() bool {
	switch s {
	case FleetNetworkActive, FleetNetworkDeprovisioned:
		return true
	}
	return false
}

// FleetNetwork is the durable per-(system, env) policy scope on the fleet (P21
// EnsureNetwork). It is a LOGICAL scope, not a CIDR: the fleet's addressing is
// host-global (10.201.0.0/16, one /30 per replica boot — see image_boot.go), so
// a per-system CIDR would be a field the substrate does not implement (D-164).
//
// SystemID is an OPAQUE SCOPE KEY (the catalog Product ID, D-146/D-164): the
// fleet stores and compares it and NEVER dereferences it — there is no `systems`
// table anywhere. It doubles as the GORM model; DDL is owned by golang-migrate
// (migrations/), NOT AutoMigrate.
type FleetNetwork struct {
	ID       uuid.UUID `json:"id" gorm:"type:uuid;primary_key"`
	SystemID string    `json:"system_id" gorm:"type:varchar(255);not null;uniqueIndex:uq_fleet_networks_system_env"`
	Env      string    `json:"env" gorm:"type:varchar(64);not null;uniqueIndex:uq_fleet_networks_system_env"`
	// OwnerOrg is the attested tenant (D-069/I28) — the same anchor as
	// FleetApp.OwnerOrg. Unlike FleetApp it is REQUIRED: a network is a net-new
	// surface with no legacy caller to accommodate, so there is no reason to
	// accept an unattested tenant (strict from birth).
	OwnerOrg  string             `json:"owner_org" gorm:"type:text;not null;default:''"`
	Status    FleetNetworkStatus `json:"status" gorm:"type:varchar(20);not null;default:'active';index"`
	CreatedAt time.Time          `json:"created_at" gorm:"not null"`
	UpdatedAt time.Time          `json:"updated_at" gorm:"not null"`
}

// TableName specifies the GORM table name.
func (FleetNetwork) TableName() string {
	return "fleet_networks"
}
