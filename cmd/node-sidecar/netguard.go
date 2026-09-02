package main

import (
	"fmt"
	"net"
	"net/netip"
)

// The ranges an egress request may NEVER reach. They are listed explicitly
// rather than derived from "is it routable" because the danger here is precise:
// the cloud metadata endpoint, the docker networks this daemon runs, the host
// itself, and the sidecar's own interfaces. A node's declared egress is for the
// public internet; everything below is infrastructure the node must not see.
var disallowedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("10.0.0.0/8"),     // RFC1918 — the homelab and every invocation bridge
	netip.MustParsePrefix("100.64.0.0/10"),  // RFC6598 carrier-grade NAT
	netip.MustParsePrefix("172.16.0.0/12"),  // RFC1918 — docker's default pools
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("192.168.0.0/16"), // RFC1918
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved
	netip.MustParsePrefix("fc00::/7"),       // unique local addresses
}

// disallowedAddr classifies ONE address. Every answer a resolver returns is put
// through it, not just the first: a name that resolves to one public and one
// private address is a DNS-rebinding attempt, and taking the public one would
// be exactly the hole the resolve-once-then-pin rule exists to close.
//
// v4-mapped v6 is unmapped first, so ::ffff:169.254.169.254 is judged as the
// v4 metadata address it is rather than as an unremarkable v6 address.
func disallowedAddr(ip netip.Addr) bool {
	if !ip.IsValid() {
		return true
	}
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		// Link-local covers 169.254.0.0/16, and therefore the cloud metadata
		// address 169.254.169.254 — the single most valuable target a hostile
		// node could ask a proxy to fetch.
		return true
	}
	for _, p := range disallowedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// ownAddresses is the set of addresses this container itself answers on. They
// are denied separately (own_address) because reaching them is not a private
// network reach — it is the node asking the proxy to talk to the proxy, which
// is how an attacker would try to loop back into the trusted half.
func ownAddresses(addrs []net.Addr) map[netip.Addr]bool {
	own := map[netip.Addr]bool{}
	for _, a := range addrs {
		if ip, ok := addrOf(a); ok {
			own[ip.Unmap()] = true
		}
	}
	return own
}

// localAddrInSubnet picks the address this sidecar holds INSIDE the invocation
// bridge. The proxy binds there and nowhere else: the sidecar is also attached
// to the egress uplink, so a wildcard bind would publish the proxy onto a
// network shared with every other sidecar.
func localAddrInSubnet(addrs []net.Addr, subnet string) (netip.Addr, error) {
	p, err := netip.ParsePrefix(subnet)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("node sidecar: invalid subnet %s", subnet)
	}
	for _, a := range addrs {
		ip, ok := addrOf(a)
		if !ok {
			continue
		}
		if p.Contains(ip.Unmap()) {
			return ip.Unmap(), nil
		}
	}
	return netip.Addr{}, fmt.Errorf("node sidecar: no local address in subnet %s", subnet)
}

// addrOf extracts the address from an interface address of either shape.
func addrOf(a net.Addr) (netip.Addr, bool) {
	switch v := a.(type) {
	case *net.IPNet:
		return netip.AddrFromSlice(v.IP)
	case *net.IPAddr:
		return netip.AddrFromSlice(v.IP)
	}
	ip, err := netip.ParseAddr(a.String())
	if err != nil {
		return netip.Addr{}, false
	}
	return ip, true
}
