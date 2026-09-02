package usecase

import (
	"errors"
	"testing"

	"github.com/sentiae/runtime-service/internal/domain"
)

// T1.9 — the pool hands out disjoint slices, refuses rather than wrapping when
// the range is exhausted, reuses what is released, and knows whether its whole
// range collides with an existing network.
//
// Control (the bound): drop the `p.next >= p.total` refusal from Acquire ⇒ the
// exhaustion row hands out an 8193rd subnet outside 10.201.0.0/16, i.e. two
// invocations could be given overlapping or foreign address space.
func TestSubnetPool(t *testing.T) {
	t.Run("exhausts at the bound then refuses", func(t *testing.T) {
		pool, err := NewSubnetPool("10.201.0.0/16", 29)
		if err != nil {
			t.Fatalf("NewSubnetPool: %v", err)
		}

		const want = 8192 // /16 sliced into /29 blocks
		seen := make(map[string]bool, want)
		var last string
		for i := 0; i < want; i++ {
			cidr, err := pool.Acquire()
			if err != nil {
				t.Fatalf("Acquire #%d: %v", i+1, err)
			}
			if seen[cidr] {
				t.Fatalf("Acquire #%d handed out %s twice", i+1, cidr)
			}
			seen[cidr] = true
			last = cidr
		}
		if first := "10.201.0.0/29"; !seen[first] {
			t.Fatalf("pool never handed out %s", first)
		}
		if want := "10.201.255.248/29"; last != want {
			t.Fatalf("last subnet = %s, want %s", last, want)
		}

		if _, err := pool.Acquire(); !errors.Is(err, domain.ErrNodeRunnerBusy) {
			t.Fatalf("Acquire past the bound error = %v, want ErrNodeRunnerBusy", err)
		}

		// Release returns capacity, and only what was actually taken.
		pool.Release(last)
		got, err := pool.Acquire()
		if err != nil {
			t.Fatalf("Acquire after Release: %v", err)
		}
		if got != last {
			t.Fatalf("Acquire after Release = %s, want the released %s", got, last)
		}
		if _, err := pool.Acquire(); !errors.Is(err, domain.ErrNodeRunnerBusy) {
			t.Fatalf("Acquire after re-taking the released slice = %v, want ErrNodeRunnerBusy", err)
		}
	})

	t.Run("release is idempotent and ignores foreign subnets", func(t *testing.T) {
		pool, err := NewSubnetPool("10.201.0.0/24", 29)
		if err != nil {
			t.Fatalf("NewSubnetPool: %v", err)
		}
		first, err := pool.Acquire()
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		pool.Release(first)
		pool.Release(first)            // second release must not double-credit
		pool.Release("192.168.0.0/29") // never handed out by this pool
		pool.Release("not a cidr")     // teardown must tolerate garbage

		seen := map[string]bool{}
		for i := 0; i < 32; i++ { // /24 into /29 = 32 slices
			cidr, err := pool.Acquire()
			if err != nil {
				t.Fatalf("Acquire #%d after releases: %v", i+1, err)
			}
			if seen[cidr] {
				t.Fatalf("double-credited release handed out %s twice", cidr)
			}
			seen[cidr] = true
		}
		if _, err := pool.Acquire(); !errors.Is(err, domain.ErrNodeRunnerBusy) {
			t.Fatalf("Acquire past the bound error = %v, want ErrNodeRunnerBusy", err)
		}
	})

	t.Run("overlaps", func(t *testing.T) {
		pool, err := NewSubnetPool("10.201.0.0/16", 29)
		if err != nil {
			t.Fatalf("NewSubnetPool: %v", err)
		}
		tests := []struct {
			other string
			want  bool
		}{
			{"10.201.0.0/16", true},
			{"10.201.7.0/24", true},
			{"10.201.255.248/29", true},
			{"10.200.0.0/16", false},
			{"10.202.0.0/16", false},
			{"172.17.0.0/16", false},
			{"10.0.0.0/8", true},
			{"fd00::/8", false},
			{"garbage", false},
		}
		for _, tt := range tests {
			if got := pool.Overlaps(tt.other); got != tt.want {
				t.Fatalf("Overlaps(%q) = %v, want %v", tt.other, got, tt.want)
			}
		}
	})

	t.Run("refuses a range it cannot slice", func(t *testing.T) {
		for _, tt := range []struct {
			cidr   string
			prefix int
		}{
			{"10.201.0.0/16", 8},  // prefix wider than the base
			{"10.201.0.0/16", 33}, // not an IPv4 prefix length
			{"fd00::/16", 29},     // IPv6
			{"10.201.0.0", 29},    // not a prefix
			{"", 29},
		} {
			if _, err := NewSubnetPool(tt.cidr, tt.prefix); err == nil {
				t.Fatalf("NewSubnetPool(%q, %d) accepted an unusable range", tt.cidr, tt.prefix)
			}
		}
	})
}
