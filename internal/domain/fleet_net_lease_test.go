package domain

import (
	"errors"
	"testing"
)

// TestDeriveNetLease pins the addressing arithmetic, including the LIVE
// CONTINUITY case.
//
// ⚠ (ordinal 0, slot 1) MUST keep producing net_index 1 / 10.201.0.5 /
// 10.201.0.6 / img1 / uid 100001. That is the addressing the resident replica on
// the live fleet host is running on right now: it was allocated by the old
// in-memory scheme and adopted by the lease plane, so if this derivation ever
// moved, the reconcile would either fail closed on a mismatch or (worse) a future
// boot would take the address a running customer VM holds.
func TestDeriveNetLease(t *testing.T) {
	const uidBase = 100000

	tests := []struct {
		name          string
		ordinal, slot int
		wantIndex     int
		wantHost      string
		wantGuest     string
		wantTap       string
		wantUID       int
	}{
		{
			name:    "live continuity: the existing host's first slot",
			ordinal: 0, slot: 1,
			wantIndex: 1, wantHost: "10.201.0.5", wantGuest: "10.201.0.6",
			wantTap: "img1", wantUID: 100001,
		},
		{
			name: "second slot", ordinal: 0, slot: 2,
			wantIndex: 2, wantHost: "10.201.0.9", wantGuest: "10.201.0.10",
			wantTap: "img2", wantUID: 100002,
		},
		{
			name: "octet-3 roll", ordinal: 0, slot: 64,
			wantIndex: 64, wantHost: "10.201.1.1", wantGuest: "10.201.1.2",
			wantTap: "img64", wantUID: 100064,
		},
		{
			name: "last slot of the first host", ordinal: 0, slot: NetMaxSlot,
			wantIndex: 1023, wantHost: "10.201.15.253", wantGuest: "10.201.15.254",
			wantTap: "img1023", wantUID: 101023,
		},
		{
			name: "first slot of the last host", ordinal: NetMaxOrdinal, slot: 1,
			wantIndex: 15361, wantHost: "10.201.240.5", wantGuest: "10.201.240.6",
			wantTap: "img1", wantUID: 100001,
		},
		{
			name:    "the very last allocatable /30 in the fleet subnet",
			ordinal: NetMaxOrdinal, slot: NetMaxSlot,
			wantIndex: NetMaxIndex, wantHost: "10.201.255.253", wantGuest: "10.201.255.254",
			wantTap: "img1023", wantUID: 101023,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DeriveNetLease(tt.ordinal, tt.slot, uidBase)
			if err != nil {
				t.Fatalf("DeriveNetLease(%d,%d): %v", tt.ordinal, tt.slot, err)
			}
			if got.NetIndex != tt.wantIndex {
				t.Errorf("net index = %d, want %d", got.NetIndex, tt.wantIndex)
			}
			if got.GuestIP != tt.wantGuest {
				t.Errorf("guest ip = %s, want %s", got.GuestIP, tt.wantGuest)
			}
			if got.HostIP != tt.wantHost {
				t.Errorf("host ip = %s, want %s", got.HostIP, tt.wantHost)
			}
			if got.TapName != tt.wantTap {
				t.Errorf("tap name = %s, want %s", got.TapName, tt.wantTap)
			}
			if got.VMUID != tt.wantUID {
				t.Errorf("vm uid = %d, want %d", got.VMUID, tt.wantUID)
			}
			// Whatever the coordinates, the pair must be a valid aligned /30 inside
			// the fleet subnet: host = network+1, guest = network+2.
			if !InFleetSubnet(got.HostIP) || !InFleetSubnet(got.GuestIP) {
				t.Errorf("derived pair %s/%s is outside %s", got.HostIP, got.GuestIP, FleetSubnetCIDR)
			}
		})
	}
}

// TestDeriveNetLeaseRefusesOutOfRange pins that every bound is a REFUSAL, not a
// clamp. A clamped coordinate lands on a /30, uid and chroot another live VM
// already holds — the exact cross-tenant failure this plane exists to prevent.
func TestDeriveNetLeaseRefusesOutOfRange(t *testing.T) {
	tests := []struct {
		name          string
		ordinal, slot int
		uidBase       int
	}{
		{"slot 0 is not allocatable", 0, 0, 100000},
		{"slot above the stride", 0, NetSlotStride, 100000},
		{"slot far above the stride", 0, 9999, 100000},
		{"negative slot", 0, -1, 100000},
		{"ordinal above the last host", NetMaxOrdinal + 1, 1, 100000},
		{"negative ordinal", -1, 1, 100000},
		{"zero uid base would collide with root-adjacent uids", 0, 1, 0},
		{"negative uid base", 0, 1, -100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DeriveNetLease(tt.ordinal, tt.slot, tt.uidBase)
			if !errors.Is(err, ErrNetCoordinateOutOfRange) {
				t.Fatalf("DeriveNetLease(%d,%d,%d) = %+v, %v; want ErrNetCoordinateOutOfRange",
					tt.ordinal, tt.slot, tt.uidBase, got, err)
			}
		})
	}
}

// TestDeriveNetLeaseIsInjective proves the whole coordinate space is collision
// free: every (ordinal, slot) pair yields a distinct index, guest address and
// (host-scoped) uid/tap. This is the property the unique indexes ENFORCE and the
// arithmetic must not be able to violate in the first place.
func TestDeriveNetLeaseIsInjective(t *testing.T) {
	const uidBase = 100000
	seenIndex := map[int]bool{}
	seenGuest := map[string]bool{}
	for ordinal := 0; ordinal <= NetMaxOrdinal; ordinal++ {
		seenSlotUID := map[int]bool{}
		seenSlotTap := map[string]bool{}
		for slot := 1; slot <= NetMaxSlot; slot++ {
			lease, err := DeriveNetLease(ordinal, slot, uidBase)
			if err != nil {
				t.Fatalf("DeriveNetLease(%d,%d): %v", ordinal, slot, err)
			}
			if seenIndex[lease.NetIndex] {
				t.Fatalf("duplicate net index %d at (%d,%d)", lease.NetIndex, ordinal, slot)
			}
			seenIndex[lease.NetIndex] = true
			if seenGuest[lease.GuestIP] {
				t.Fatalf("duplicate guest ip %s at (%d,%d)", lease.GuestIP, ordinal, slot)
			}
			seenGuest[lease.GuestIP] = true
			// uid and tap are HOST-local, so they may (and do) repeat across hosts —
			// they must not repeat within one.
			if seenSlotUID[lease.VMUID] {
				t.Fatalf("duplicate vm uid %d within host ordinal %d", lease.VMUID, ordinal)
			}
			seenSlotUID[lease.VMUID] = true
			if seenSlotTap[lease.TapName] {
				t.Fatalf("duplicate tap %s within host ordinal %d", lease.TapName, ordinal)
			}
			seenSlotTap[lease.TapName] = true
			// IFNAMSIZ: the kernel refuses an interface name over 15 bytes, and the DDL
			// column is varchar(15).
			if len(lease.TapName) > 15 {
				t.Fatalf("tap name %q is %d bytes, over the 15-byte interface-name limit", lease.TapName, len(lease.TapName))
			}
		}
	}
	if len(seenIndex) != NetMaxIndex-NetMaxOrdinal {
		// (NetMaxOrdinal+1) hosts × NetMaxSlot slots — each host's slot-0 index is
		// deliberately not allocatable.
		t.Fatalf("derived %d distinct indices, want %d", len(seenIndex), NetMaxIndex-NetMaxOrdinal)
	}
}

// TestNetLeaseOwnerKindIsValid pins that only the two real owner tables are
// accepted — an unrecognized kind names a lease nothing can ever reclaim.
func TestNetLeaseOwnerKindIsValid(t *testing.T) {
	for _, k := range []NetLeaseOwnerKind{NetLeaseOwnerReplica, NetLeaseOwnerWorkload} {
		if !k.IsValid() {
			t.Errorf("%q reported invalid", k)
		}
	}
	for _, k := range []NetLeaseOwnerKind{"", "Replica", "vm", "workloads"} {
		if k.IsValid() {
			t.Errorf("%q reported valid", k)
		}
	}
}

// TestNetLeaseMatchesAddresses pins the adoption test: a live VM keeps its lease
// only when the row the fleet operates it through names the SAME guest ip and tap.
func TestNetLeaseMatchesAddresses(t *testing.T) {
	lease, err := DeriveNetLease(0, 1, 100000)
	if err != nil {
		t.Fatalf("DeriveNetLease: %v", err)
	}
	if !lease.MatchesAddresses("10.201.0.6", "img1") {
		t.Error("exact match reported as a mismatch")
	}
	for _, tt := range []struct{ guest, tap string }{
		{"10.201.0.10", "img1"}, // right tap, wrong /30
		{"10.201.0.6", "img2"},  // right /30, wrong device
		{"", ""},                // a row that recorded nothing
	} {
		if lease.MatchesAddresses(tt.guest, tt.tap) {
			t.Errorf("mismatch (%q,%q) reported as a match", tt.guest, tt.tap)
		}
	}
}
