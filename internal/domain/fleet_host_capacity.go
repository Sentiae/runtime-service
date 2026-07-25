package domain

import "fmt"

// HostCapacityMeasurement is what a fleet host PHYSICALLY has, read from the
// machine itself at boot. It is the only trustworthy source of a host's capacity:
// an advertised number that was asserted by config rather than measured is a
// promise the machine never made, and the scheduler places customer data on it.
type HostCapacityMeasurement struct {
	// VCPU is the number of usable logical CPUs.
	VCPU int
	// MemTotalMB is the machine's total RAM.
	MemTotalMB int64
	// DiskTotalMB is the size of the filesystem holding the volume directory.
	DiskTotalMB int64
	// DiskAvailableMB is the FREE space on that filesystem. It — not
	// DiskTotalMB — is what a host can still place: the bytes already spent on
	// images, other volumes, and the host's own files are gone, and advertising
	// them is how a host accepts a volume it cannot materialize.
	DiskAvailableMB int64
}

// HostCapacityOverride is the operator's optional NARROWING of a measurement: a
// deliberate reservation that keeps part of the machine out of the fleet. Zero
// means "no override — advertise what was measured". A value ABOVE the
// measurement is refused (see ResolveHostCapacity): under-advertising is a
// reservation, over-advertising is a lie about bytes that do not exist.
type HostCapacityOverride struct {
	VCPU  int
	MemMB int64
	// DiskMB caps advertised disk. Compared against the measured AVAILABLE disk,
	// never the total.
	DiskMB int64
	// DiskReserveMB is subtracted from the advertised disk so the host cannot be
	// packed to 100% of its filesystem.
	DiskReserveMB int64
}

// HostCapacity is the resolved capacity a host advertises to the fleet registry.
type HostCapacity struct {
	VCPU   int
	MemMB  int64
	DiskMB int64
}

// ResolveHostCapacity turns a measurement plus an operator override into the
// capacity a host may advertise, or refuses.
//
// It fails closed in three ways, all of them protecting placement decisions made
// over customer data:
//
//   - An incomplete measurement is refused outright. A host that cannot measure
//     itself must advertise nothing rather than fall back to a code default.
//   - An override ABOVE the measurement is refused, naming both numbers. Disk is
//     the one that costs bytes — a host that claims free space it does not have
//     accepts a volume it cannot create — but the same asymmetry holds for vCPU
//     and memory, so all three are refused in the permissive direction only.
//   - A reserve that leaves nothing (or, negative, that ADDS capacity) is refused.
//
// Under-advertising deliberately stays allowed: that is what an override is for.
func ResolveHostCapacity(m HostCapacityMeasurement, o HostCapacityOverride) (HostCapacity, error) {
	if m.VCPU <= 0 || m.MemTotalMB <= 0 || m.DiskTotalMB <= 0 || m.DiskAvailableMB <= 0 {
		return HostCapacity{}, fmt.Errorf("%w: vcpu=%d mem_total=%dMB disk_total=%dMB disk_available=%dMB",
			ErrHostCapacityUnmeasured, m.VCPU, m.MemTotalMB, m.DiskTotalMB, m.DiskAvailableMB)
	}
	if o.VCPU > m.VCPU {
		return HostCapacity{}, fmt.Errorf("%w: configured vcpu %d exceeds the %d this host measures",
			ErrHostCapacityOverAdvertised, o.VCPU, m.VCPU)
	}
	if o.MemMB > m.MemTotalMB {
		return HostCapacity{}, fmt.Errorf("%w: configured memory %dMB exceeds the %dMB this host measures",
			ErrHostCapacityOverAdvertised, o.MemMB, m.MemTotalMB)
	}
	if o.DiskMB > m.DiskAvailableMB {
		return HostCapacity{}, fmt.Errorf("%w: configured disk %dMB exceeds the %dMB actually FREE on this host (total %dMB)",
			ErrHostCapacityOverAdvertised, o.DiskMB, m.DiskAvailableMB, m.DiskTotalMB)
	}

	resolved := HostCapacity{VCPU: m.VCPU, MemMB: m.MemTotalMB, DiskMB: m.DiskAvailableMB}
	if o.VCPU > 0 {
		resolved.VCPU = o.VCPU
	}
	if o.MemMB > 0 {
		resolved.MemMB = o.MemMB
	}
	if o.DiskMB > 0 {
		resolved.DiskMB = o.DiskMB
	}
	if o.DiskReserveMB < 0 || o.DiskReserveMB >= resolved.DiskMB {
		return HostCapacity{}, fmt.Errorf("%w: reserve %dMB against an advertisable %dMB",
			ErrHostDiskReserveInvalid, o.DiskReserveMB, resolved.DiskMB)
	}
	resolved.DiskMB -= o.DiskReserveMB
	return resolved, nil
}
