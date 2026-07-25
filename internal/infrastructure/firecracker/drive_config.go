package firecracker

// driveConfigBody builds the body of a Firecracker `PUT /drives/<id>` for one
// virtio block device. EVERY drive this platform configures goes through here —
// that is the point of the helper, not convenience.
//
// SentiaeDB durability gate condition A2 (#p19-firecracker-cache-type-fsync):
// Firecracker's DEFAULT cache_type is `Unsafe`, which does not turn a guest FLUSH
// into a host fsync. On that setting every fsync inside the guest — including
// every Postgres commit — is acknowledged without the bytes reaching stable
// storage, so a host crash silently loses committed transactions. `Writeback` is
// the mode that honors the guest's flush.
//
// It is set UNCONDITIONALLY rather than only on the drives believed to hold
// PGDATA, because "believed" is how this class of bug survives: the image-boot
// rootfs is exactly where a failed /dev/vdb mount would have landed PGDATA, and a
// per-site judgement call is a site a future drive can be added without. For the
// genuinely ephemeral sandbox drives the cost is paid only when a guest actually
// fsyncs, which for them is negligible.
//
// Per-drive extras (rate_limiter) are added by the caller on the returned map.
func driveConfigBody(driveID, pathOnHost string, isRootDevice, isReadOnly bool) map[string]any {
	return map[string]any{
		"drive_id":       driveID,
		"path_on_host":   pathOnHost,
		"is_root_device": isRootDevice,
		"is_read_only":   isReadOnly,
		"cache_type":     driveCacheType,
	}
}

// driveCacheType is the ONE cache mode this platform configures. Named so a
// change is a one-line, reviewable, greppable event rather than a per-site
// omission (see driveConfigBody for why Unsafe is not survivable).
const driveCacheType = "Writeback"
