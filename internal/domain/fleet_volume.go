package domain

import (
	"time"

	"github.com/google/uuid"
)

// VolumeStatus is the attachment lifecycle state of a persistent volume.
type VolumeStatus string

const (
	// VolumeStatusAvailable — the backing file exists and is unattached.
	VolumeStatusAvailable VolumeStatus = "available"
	// VolumeStatusAttached — the volume is attached to a resident replica.
	VolumeStatusAttached VolumeStatus = "attached"
	// VolumeStatusDegraded — the volume's affinity host is gone; this cycle the
	// volume (and its app) is terminal-degraded (no cross-host restore yet).
	VolumeStatusDegraded VolumeStatus = "degraded"
	// VolumeStatusRestoring — an in-place restore owns this volume (D-184). It is
	// the boot STAND-OFF: while it holds, BootReplica refuses to attach the
	// backing file, so no VM can hold an fd to the inode the restore renames.
	// Like degraded, a detach never revives it — only the restore clears it.
	VolumeStatusRestoring VolumeStatus = "restoring"
)

// IsValid reports whether the volume status is one the fleet recognizes.
func (s VolumeStatus) IsValid() bool {
	switch s {
	case VolumeStatusAvailable, VolumeStatusAttached, VolumeStatusDegraded,
		VolumeStatusRestoring:
		return true
	}
	return false
}

// Volume is a persistent volume attached to a FleetApp, optionally pinned to a
// host. It doubles as the GORM model (see ImageWorkload). DDL is owned by
// golang-migrate (migrations/), not AutoMigrate.
type Volume struct {
	ID uuid.UUID `json:"id" gorm:"type:uuid;primary_key"`
	// AppID + MountPath are the volume's REAL key, and they are unique together
	// (migrations/0017). EnsureAppVolumes does a read-then-write on that pair with
	// no transaction, so without the constraint two concurrent provisions of one
	// app mint two rows and two backing files — after which the in-place restore
	// refuses the resource forever (soleVolume wants exactly one), PrimaryVolume
	// picks by created_at luck, and a final snapshot can capture the empty
	// duplicate.
	//
	// ⚠ The tags here MUST match migration 0024 exactly, and the index name MUST
	// stay equal to the one 0017 creates over the same columns: the integration
	// tests AutoMigrate this struct, and the tags are the model's documented
	// contract (golang-migrate is the sole production schema authority, D-178).
	// A name or column-set mismatch builds a second, differently-keyed index
	// under the tests and silently reopens the race (the same trap documented on
	// FleetApp.OwnerOrg for 0014).
	//
	// AppID is now an ATTACHMENT, not the owner (D-203, 0024): it says which
	// workload currently mounts the volume, and it is nullable because a volume
	// can outlive its app once the claim owns it.
	AppID *uuid.UUID `json:"app_id,omitempty" gorm:"type:uuid;index;uniqueIndex:fleet_volumes_app_mount_key"`
	// ResourceID is the durable claim that OWNS this volume (D-203). Nil for a
	// plain stateful app's volume — AppID is then the only parent. When set, the
	// DDL (0024) RESTRICTs the resource delete and DeleteAppVolumes refuses while
	// the claim is live: a claim-owned volume is deletable only through the
	// resource's own snapshot-first teardown.
	ResourceID *uuid.UUID `json:"resource_id,omitempty" gorm:"column:resource_id;type:uuid;index:fleet_volumes_resource_id_idx"`
	// PoolGUID is the pool-GUID location primitive (D-203): the ZFS pool the
	// bytes live on, decimal-printed uint64 (TEXT — BIGINT is signed). Nil =
	// location not pool-attested (every ext4-file-era volume). No writer yet;
	// the host-identity ruling supplies one.
	PoolGUID     *string    `json:"pool_guid,omitempty" gorm:"column:pool_guid;type:text"`
	SizeMB       int64      `json:"size_mb" gorm:"not null"`
	HostAffinity *uuid.UUID `json:"host_affinity,omitempty" gorm:"type:uuid"`
	SnapshotRef  string     `json:"snapshot_ref" gorm:"type:varchar(255);not null;default:''"`
	MountPath    string     `json:"mount_path" gorm:"type:varchar(255);not null;default:'/data';uniqueIndex:fleet_volumes_app_mount_key"`
	// BackingPath is the host path of the ext4 backing file the guest attaches as
	// its data disk (/dev/vdb). Empty until the backend has materialized it.
	BackingPath string `json:"backing_path" gorm:"column:backing_path;type:varchar(512);not null;default:''"`
	// AttachedReplica is the resident replica currently holding this volume, or
	// nil when available. Single-writer: at most one replica at a time.
	AttachedReplica *uuid.UUID `json:"attached_replica,omitempty" gorm:"column:attached_replica;type:uuid"`
	// Status is the attachment lifecycle state (available|attached|degraded).
	Status VolumeStatus `json:"status" gorm:"type:varchar(20);not null;default:'available'"`
	// DeviceName is the in-guest block device the volume is mounted from.
	DeviceName string    `json:"device_name" gorm:"column:device_name;type:varchar(16);not null;default:'/dev/vdb'"`
	CreatedAt  time.Time `json:"created_at" gorm:"not null"`
	UpdatedAt  time.Time `json:"updated_at" gorm:"not null"`
}

// TableName specifies the GORM table name.
func (Volume) TableName() string {
	return "fleet_volumes"
}
