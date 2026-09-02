//go:build unit

package main

import (
	"net"
	"net/netip"
	"testing"
)

// T4.1 — TestNetGuard_Disallowed proves the address classifier names every
// range a node must never reach through the proxy, and — the positive anchor —
// still admits ordinary public addresses. A classifier that denied everything
// would satisfy the deny rows and make the whole proxy useless, so the allow
// rows are part of the proof.
//
// CONTROL: delete the IsLinkLocalUnicast() term in disallowedAddr — the cloud
// metadata row (169.254.169.254) and the v4-mapped metadata row answer false
// and this test goes red.
func TestNetGuard_Disallowed(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want bool
	}{
		{"cloud metadata", "169.254.169.254", true},
		{"link-local v4", "169.254.1.1", true},
		{"v4-mapped metadata is judged as v4", "::ffff:169.254.169.254", true},
		{"loopback v4", "127.0.0.1", true},
		{"loopback v6", "::1", true},
		{"unspecified", "0.0.0.0", true},
		{"this network", "0.1.2.3", true},
		{"rfc1918 ten", "10.0.10.20", true},
		{"rfc1918 172", "172.17.0.1", true},
		{"rfc1918 192.168", "192.168.1.1", true},
		{"invocation bridge range", "10.201.0.2", true},
		{"carrier grade nat", "100.64.0.1", true},
		{"ietf protocol assignments", "192.0.0.8", true},
		{"benchmarking", "198.18.0.1", true},
		{"reserved", "240.0.0.1", true},
		{"multicast", "224.0.0.1", true},
		{"unique local v6", "fd00::1", true},
		{"link-local v6", "fe80::1", true},
		{"public v4 anchor", "93.184.216.34", false},
		{"public v4 resolver anchor", "1.1.1.1", false},
		{"public v6 anchor", "2606:4700:4700::1111", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, err := netip.ParseAddr(tt.addr)
			if err != nil {
				t.Fatalf("parse %s: %v", tt.addr, err)
			}
			if got := disallowedAddr(addr); got != tt.want {
				t.Fatalf("disallowedAddr(%s) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

// TestNetGuard_InvalidAddressIsDisallowed pins the fail-closed default: an
// address that could not be parsed is not "unclassified", it is refused.
func TestNetGuard_InvalidAddressIsDisallowed(t *testing.T) {
	if !disallowedAddr(netip.Addr{}) {
		t.Fatal("the zero address must be disallowed")
	}
}

// TestLocalAddrInSubnet is the pure half of T4.9: which of this container's
// addresses the proxy binds to. It must be the one inside the INVOCATION
// subnet, never the first one, and the failure must carry the verbatim refusal.
//
// CONTROL: return the first address instead of the matching one — the
// "picks the invocation address" row returns the uplink address and goes red.
func TestLocalAddrInSubnet(t *testing.T) {
	addrs := []net.Addr{
		mustCIDR(t, "127.0.0.1/8"),
		mustCIDR(t, "172.20.0.5/16"), // the uplink side
		mustCIDR(t, "10.201.0.2/29"), // the invocation bridge
	}

	got, err := localAddrInSubnet(addrs, "10.201.0.0/29")
	if err != nil {
		t.Fatalf("localAddrInSubnet: %v", err)
	}
	if got.String() != "10.201.0.2" {
		t.Fatalf("bound address: got %s, want 10.201.0.2", got)
	}

	_, err = localAddrInSubnet(addrs, "10.202.0.0/29")
	if err == nil {
		t.Fatal("an address outside every local range must refuse")
	}
	if err.Error() != "node sidecar: no local address in subnet 10.202.0.0/29" {
		t.Fatalf("refusal text: got %q", err.Error())
	}
}

// TestOwnAddresses pins that the sidecar knows its own addresses, which is what
// makes own_address distinguishable from any other private answer.
func TestOwnAddresses(t *testing.T) {
	own := ownAddresses([]net.Addr{mustCIDR(t, "10.201.0.2/29"), mustCIDR(t, "172.20.0.5/16")})
	if !own[netip.MustParseAddr("10.201.0.2")] {
		t.Fatal("the invocation address must be in the own set")
	}
	if own[netip.MustParseAddr("93.184.216.34")] {
		t.Fatal("a public address must not be in the own set")
	}
}

func mustCIDR(t *testing.T, cidr string) net.Addr {
	t.Helper()
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("parse %s: %v", cidr, err)
	}
	return &net.IPNet{IP: ip, Mask: ipnet.Mask}
}
