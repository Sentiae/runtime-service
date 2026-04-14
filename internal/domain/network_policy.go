package domain

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net"
	"strings"
)

// NetworkPolicyMode controls the egress posture of a VM at boot time.
//
//   - NetworkPolicyIsolated  — no TAP device is attached. The guest sees
//     no network. Default for test execution.
//   - NetworkPolicyAllowHost — TAP attached to the standard bridge,
//     full host-routed connectivity (legacy behaviour).
//   - NetworkPolicyEgressList — TAP attached, but iptables drops every
//     egress packet that does not match an entry in AllowedHosts.
type NetworkPolicyMode string

const (
	NetworkPolicyIsolated   NetworkPolicyMode = "isolated"
	NetworkPolicyAllowHost  NetworkPolicyMode = "allow_host"
	NetworkPolicyEgressList NetworkPolicyMode = "allow_egress_list"
)

// IsValid reports whether the mode is one of the known constants.
func (m NetworkPolicyMode) IsValid() bool {
	switch m {
	case NetworkPolicyIsolated, NetworkPolicyAllowHost, NetworkPolicyEgressList:
		return true
	}
	return false
}

// NetworkPolicy is the per-VM network configuration carried through
// VMBootConfig. Stored on disk via a JSONB column when persisted.
type NetworkPolicy struct {
	Mode         NetworkPolicyMode `json:"mode"`
	AllowedHosts []string          `json:"allowed_hosts,omitempty"`
}

// DefaultTestNetworkPolicy returns the policy applied when a TestRun does
// not explicitly opt into networking. The default is "no network at all"
// so flaky external dependencies cannot reach into the test VM.
func DefaultTestNetworkPolicy() NetworkPolicy {
	return NetworkPolicy{Mode: NetworkPolicyIsolated}
}

// Validate checks that the mode is known and that AllowedHosts only
// contains parseable hostnames or CIDR blocks. We deliberately accept
// either form so callers can express both "github.com" and "10.0.0.0/8".
func (p NetworkPolicy) Validate() error {
	if !p.Mode.IsValid() {
		return errors.New("invalid network policy mode")
	}
	if p.Mode != NetworkPolicyEgressList && len(p.AllowedHosts) > 0 {
		return errors.New("allowed_hosts only meaningful for allow_egress_list mode")
	}
	for _, h := range p.AllowedHosts {
		h = strings.TrimSpace(h)
		if h == "" {
			return errors.New("allowed_hosts contains empty entry")
		}
		// CIDR form is preferred — fall back to single-host parse.
		if _, _, err := net.ParseCIDR(h); err == nil {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			continue
		}
		// Hostname: rudimentary check (must not contain spaces, must not
		// contain protocol). We don't attempt DNS resolution here — the
		// firecracker provider is responsible for resolving + adding
		// iptables rules at boot time.
		if strings.ContainsAny(h, " \t/") {
			return errors.New("allowed_hosts entry contains invalid characters: " + h)
		}
	}
	return nil
}

// Value implements driver.Valuer so the policy can sit in a JSONB column.
func (p NetworkPolicy) Value() (driver.Value, error) {
	return json.Marshal(p)
}

// Scan implements sql.Scanner so the policy round-trips via gorm.
func (p *NetworkPolicy) Scan(value any) error {
	if value == nil {
		*p = NetworkPolicy{}
		return nil
	}
	b, ok := value.([]byte)
	if !ok {
		return errors.New("network policy: unsupported scan source")
	}
	return json.Unmarshal(b, p)
}
