package usecase

import (
	"encoding/binary"
	"net/netip"
	"sync"

	"github.com/sentiae/runtime-service/internal/domain"
)

// SubnetPool hands out the small, disjoint IPv4 slices the per-invocation
// egress bridges are built on. It is pure: no docker, no I/O, no clock — the
// allocation rule is the whole thing, so it is testable to exhaustion.
//
// Slices are handed out in ascending order and returned to a free set on
// Release, so a long-lived process reuses the low end of the range rather than
// walking off it.
type SubnetPool struct {
	mu     sync.Mutex
	base   netip.Prefix
	prefix int
	// next is the index of the lowest slice never yet handed out.
	next uint64
	// total is how many slices the base range holds at this prefix length.
	total uint64
	// free holds released slices, keyed by their rendered CIDR.
	free map[string]bool
	// taken is every slice currently out on loan.
	taken map[string]bool
}

// NewSubnetPool slices cidr into /prefixLen blocks. It refuses a range it
// cannot slice — a pool that silently held zero subnets would turn every egress
// invocation into ErrNodeRunnerBusy with no way to tell that from exhaustion.
func NewSubnetPool(cidr string, prefixLen int) (*SubnetPool, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, domain.ErrInvalidData
	}
	p = p.Masked()
	if !p.Addr().Is4() {
		return nil, domain.ErrInvalidData
	}
	if prefixLen < p.Bits() || prefixLen > 32 {
		return nil, domain.ErrInvalidData
	}
	return &SubnetPool{
		base:   p,
		prefix: prefixLen,
		total:  uint64(1) << uint(prefixLen-p.Bits()),
		free:   map[string]bool{},
		taken:  map[string]bool{},
	}, nil
}

// Acquire hands out the next free slice, or ErrNodeRunnerBusy when the range is
// exhausted. Exhaustion is a REFUSAL, never a wrap-around: two live invocations
// sharing a subnet is the one failure this pool exists to make impossible.
func (p *SubnetPool) Acquire() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for cidr := range p.free {
		delete(p.free, cidr)
		p.taken[cidr] = true
		return cidr, nil
	}
	if p.next >= p.total {
		return "", domain.ErrNodeRunnerBusy
	}
	cidr := p.slice(p.next)
	p.next++
	p.taken[cidr] = true
	return cidr, nil
}

// Release returns a slice to the pool. Releasing something never acquired, or
// releasing twice, is a no-op rather than an error: teardown runs on the
// failure path too, and it must not need to know how far setup got.
func (p *SubnetPool) Release(subnet string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.taken[subnet] {
		return
	}
	delete(p.taken, subnet)
	p.free[subnet] = true
}

// Overlaps reports whether another range intersects this pool's whole address
// space. The boot probe uses it against every existing docker network: an
// invocation bridge that collides with a real network would route a sandbox's
// traffic somewhere it was never allowed to reach.
func (p *SubnetPool) Overlaps(other string) bool {
	o, err := netip.ParsePrefix(other)
	if err != nil {
		return false
	}
	o = o.Masked()
	if !o.Addr().Is4() {
		return false
	}
	return p.base.Overlaps(o)
}

// slice renders the i-th /prefix block of the base range.
func (p *SubnetPool) slice(i uint64) string {
	b := p.base.Addr().As4()
	start := binary.BigEndian.Uint32(b[:])
	addr := start + uint32(i)<<uint(32-p.prefix)
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], addr)
	return netip.PrefixFrom(netip.AddrFrom4(out), p.prefix).String()
}
