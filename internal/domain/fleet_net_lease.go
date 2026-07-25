package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// The fleet's microVM addressing plane, in one place.
//
// ⚠ WHY THIS IS SECURITY-CRITICAL. One integer — the net index — keys the TAP
// device name, the /30 the guest is configured with, the jailer chroot id AND the
// per-VM uid/gid. Two live microVMs that ever derive the same index do not get a
// networking bug: they get the same unprivileged identity and the same chroot,
// which is cross-tenant read/write on customer data. Every allocation therefore
// has to be serialized by something stronger than a process-local map — it is
// serialized by a UNIQUE INDEX on fleet_net_leases (the lease row IS the
// allocation), and every derived coordinate is RECORDED on that row rather than
// recomputed on read.
//
// The coordinates:
//
//	net_ordinal ∈ [0,NetMaxOrdinal]  the HOST term, assigned once per fleet host
//	local_slot  ∈ [1,NetMaxSlot]     host-local; keys vm_uid, tap name, jail id
//	net_index   = net_ordinal*NetSlotStride + local_slot ∈ [1,NetMaxIndex]
//
// net_index indexes the /30 inside FleetSubnetCIDR (10.201.0.0/16):
//
//	network = 10.201.0.0 + net_index*4
//	host    = network + 1   (the gateway the guest sees)
//	guest   = network + 2
//
// Splitting the index into (host, slot) is what makes the plane multi-host: two
// hosts can never collide, because their ordinals differ, and no cross-host
// coordination is needed to hand out a slot. Recording the addresses (rather than
// recomputing them from the index) is what makes a future re-split
// migration-free: changing the stride cannot move an address a live VM is
// already configured with.
const (
	// NetSlotStride is how many index values each host owns. 1024 slots × 16
	// hosts = the whole [1,16383] index space (D-187 session decision).
	NetSlotStride = 1024
	// NetMaxSlot is the highest host-local slot. Slot 0 is not allocatable: it
	// would make net_index == net_ordinal*stride, and index 0's /30 (10.201.0.0)
	// has no valid host/guest pair.
	NetMaxSlot = NetSlotStride - 1
	// NetMaxOrdinal is the highest host ordinal. 16 hosts is the ceiling the /16
	// admits at this stride, and it is a hard fence rather than a soft limit: an
	// ordinal above it would derive an index outside the subnet.
	NetMaxOrdinal = 15
	// NetMaxIndex is the highest derivable net index — its /30 is based at
	// 65532, whose guest address is the last usable one in the /16
	// (10.201.255.254).
	NetMaxIndex = (NetMaxOrdinal+1)*NetSlotStride - 1

	// netIndexIPStride is the size of each workload's /30.
	netIndexIPStride = 4
	// netSubnetOctet1 / netSubnetOctet2 are the fixed high octets of
	// FleetSubnetCIDR. Kept next to the arithmetic that uses them so the two can
	// never drift apart silently (a mismatch is asserted below via InFleetSubnet).
	netSubnetOctet1 = 10
	netSubnetOctet2 = 201
)

// NetLeaseOwnerKind names which control-plane table owns a lease. Both
// fleet_replicas and image_workloads boot microVMs into the SAME index space, so
// the owner reference has to carry its table — an owner id alone would be
// ambiguous, and resolving it in the wrong table is how a live VM's lease gets
// reclaimed.
type NetLeaseOwnerKind string

const (
	// NetLeaseOwnerReplica is a fleet_replicas row (the resident fleet path).
	NetLeaseOwnerReplica NetLeaseOwnerKind = "replica"
	// NetLeaseOwnerWorkload is an image_workloads row (test/job/resident CP3 path).
	NetLeaseOwnerWorkload NetLeaseOwnerKind = "workload"
)

// IsValid reports whether the owner kind is one the plane recognizes. An
// unrecognized kind is refused at allocation: a lease whose owner cannot be
// resolved could never be reclaimed, and the DDL CHECK would reject it anyway.
func (k NetLeaseOwnerKind) IsValid() bool {
	switch k {
	case NetLeaseOwnerReplica, NetLeaseOwnerWorkload:
		return true
	}
	return false
}

// NetLease is one HELD microVM addressing allocation. The row's existence IS the
// allocation: it is inserted before the TAP/jail/VM exist and deleted only after
// they are gone, and its unique indexes (net_index, (host_id,local_slot),
// (host_id,vm_uid), (host_id,tap_name), (owner_kind,owner_id)) are the fences.
//
// It doubles as the GORM model — this service persists domain structs directly
// (see Replica / ImageWorkload). DDL is owned by golang-migrate (migrations/),
// not AutoMigrate.
//
// HostOrdinal is a SNAPSHOT of the owning host's net_ordinal at allocation time,
// not a join. A live VM's addresses must never move because a host row was
// edited, and the recorded ordinal is what proves which /30 block the lease came
// out of even if the host's ordinal is later changed.
type NetLease struct {
	ID          uuid.UUID         `json:"id" gorm:"type:uuid;primary_key"`
	HostID      uuid.UUID         `json:"host_id" gorm:"type:uuid;not null"`
	HostOrdinal int               `json:"host_ordinal" gorm:"not null"`
	LocalSlot   int               `json:"local_slot" gorm:"not null"`
	NetIndex    int               `json:"net_index" gorm:"not null"`
	HostIP      string            `json:"host_ip" gorm:"type:varchar(45);not null"`
	GuestIP     string            `json:"guest_ip" gorm:"type:varchar(45);not null"`
	TapName     string            `json:"tap_name" gorm:"type:varchar(15);not null"`
	VMUID       int               `json:"vm_uid" gorm:"column:vm_uid;not null"`
	OwnerKind   NetLeaseOwnerKind `json:"owner_kind" gorm:"type:varchar(16);not null"`
	OwnerID     uuid.UUID         `json:"owner_id" gorm:"type:uuid;not null"`
	CreatedAt   time.Time         `json:"created_at" gorm:"not null"`
	UpdatedAt   time.Time         `json:"updated_at" gorm:"not null"`
}

// TableName specifies the GORM table name.
func (NetLease) TableName() string {
	return "fleet_net_leases"
}

// DeriveNetLease computes every coordinate of one allocation from (hostOrdinal,
// localSlot) — it is the ONE place fleet microVM addressing is computed. The
// returned lease carries no owner or host id; the allocator stamps those.
//
// Every bound is a refusal, not a clamp. A clamp is how two VMs end up on one
// address: an out-of-range slot silently folded back into range would derive a
// /30 another live VM already holds. The final assertion re-checks both derived
// addresses against FleetSubnetCIDR, so even a future arithmetic mistake cannot
// hand out an address outside the fleet's own space.
func DeriveNetLease(hostOrdinal, localSlot, uidBase int) (NetLease, error) {
	if hostOrdinal < 0 || hostOrdinal > NetMaxOrdinal {
		return NetLease{}, fmt.Errorf("%w: host ordinal %d outside [0,%d]",
			ErrNetCoordinateOutOfRange, hostOrdinal, NetMaxOrdinal)
	}
	if localSlot < 1 || localSlot > NetMaxSlot {
		return NetLease{}, fmt.Errorf("%w: local slot %d outside [1,%d]",
			ErrNetCoordinateOutOfRange, localSlot, NetMaxSlot)
	}
	if uidBase <= 0 {
		return NetLease{}, fmt.Errorf("%w: vm uid base %d must be positive",
			ErrNetCoordinateOutOfRange, uidBase)
	}

	netIndex := hostOrdinal*NetSlotStride + localSlot
	if netIndex < 1 || netIndex > NetMaxIndex {
		return NetLease{}, fmt.Errorf("%w: net index %d outside [1,%d]",
			ErrNetCoordinateOutOfRange, netIndex, NetMaxIndex)
	}

	base := netIndex * netIndexIPStride
	o3 := base >> 8
	o4 := base & 0xff
	lease := NetLease{
		HostOrdinal: hostOrdinal,
		LocalSlot:   localSlot,
		NetIndex:    netIndex,
		HostIP:      fmt.Sprintf("%d.%d.%d.%d", netSubnetOctet1, netSubnetOctet2, o3, o4+1),
		GuestIP:     fmt.Sprintf("%d.%d.%d.%d", netSubnetOctet1, netSubnetOctet2, o3, o4+2),
		// The TAP (and the jail id, and the uid) is keyed by the HOST-LOCAL slot,
		// not the global index: it is a host-local resource, and keeping it local
		// keeps the name short and the uid inside the configured per-VM span.
		TapName: fmt.Sprintf("img%d", localSlot),
		VMUID:   uidBase + localSlot,
	}

	// Structural assertion, deliberately not an assumption. If either address
	// escaped FleetSubnetCIDR the plane would be programming iptables and TAPs for
	// space the fleet does not own.
	if !InFleetSubnet(lease.HostIP) || !InFleetSubnet(lease.GuestIP) {
		return NetLease{}, fmt.Errorf("%w: derived pair %s/%s for net index %d is outside %s",
			ErrNetCoordinateOutOfRange, lease.HostIP, lease.GuestIP, netIndex, FleetSubnetCIDR)
	}
	return lease, nil
}

// MatchesAddresses reports whether an owner row's recorded addressing is exactly
// the lease's. It is the ADOPTION test at boot: a running VM may only keep its
// lease if the row the fleet will operate it through names the same guest IP and
// the same TAP. A mismatch means one of the two records is wrong about a live
// VM's identity, which is not repairable by guessing.
func (l NetLease) MatchesAddresses(guestIP, tapName string) bool {
	return l.GuestIP == guestIP && l.TapName == tapName
}
