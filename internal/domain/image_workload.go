package domain

import (
	"time"

	"github.com/google/uuid"
)

// ImageWorkloadClass is the class of an image-boot workload: an ephemeral
// "test" run (boot → run → exit → collect output), a "resident" workload (boot
// → stay up serving a port), or a "job" (the test one-shot path plus secrets,
// an egress allowlist, and at-most-once idempotency).
type ImageWorkloadClass string

const (
	// ImageWorkloadClassTest is a single-shot run: the VM boots the image,
	// runs its entrypoint (or an override command), powers off, and the host
	// collects stdout/stderr/exit_code.
	ImageWorkloadClassTest ImageWorkloadClass = "test"
	// ImageWorkloadClassResident is a long-lived workload: the VM boots the
	// image and stays up; the host advertises a port DNAT'd to the guest.
	ImageWorkloadClassResident ImageWorkloadClass = "resident"
	// ImageWorkloadClassJob is a one-shot run that, unlike the test class,
	// ALLOWS secret_refs/vault_token (a migrator needs its DSN — resolved
	// through the same P14 boot path the resident class uses), applies an
	// egress allowlist, and is idempotent on IdempotencyKey. It shares the test
	// class's observation contract: terminal == exited, success == exit_code 0.
	ImageWorkloadClassJob ImageWorkloadClass = "job"
)

// IsValid reports whether the class is one this service supports.
func (c ImageWorkloadClass) IsValid() bool {
	switch c {
	case ImageWorkloadClassTest, ImageWorkloadClassResident, ImageWorkloadClassJob:
		return true
	}
	return false
}

// IsOneShot reports whether the class runs to completion and is observed by its
// exit rather than by serving a port (test + job).
func (c ImageWorkloadClass) IsOneShot() bool {
	return c == ImageWorkloadClassTest || c == ImageWorkloadClassJob
}

// ImageWorkloadState is the lifecycle state of an image-boot workload.
type ImageWorkloadState string

const (
	ImageWorkloadStateBooting ImageWorkloadState = "booting"
	ImageWorkloadStateRunning ImageWorkloadState = "running"
	ImageWorkloadStateExited  ImageWorkloadState = "exited"
	ImageWorkloadStateFailed  ImageWorkloadState = "failed"
)

// ImageWorkload is a microVM booted from a compiled OCI image (the I1 model:
// boot the built image, never inject source). It doubles as the GORM model —
// this service pre-dates the constitution and persists domain structs directly
// (see the VMInstance / TestRun repos).
type ImageWorkload struct {
	ID          uuid.UUID `json:"id" gorm:"type:uuid;primary_key"`
	ComponentID string    `json:"component_id" gorm:"type:varchar(255);index"`
	Env         string    `json:"env" gorm:"type:varchar(64)"`
	// OwnerOrg is the attested tenant that owns this workload (job class). The
	// test class stores none (empty). It anchors both the by-handle org gate
	// (D-083) and the idempotency scope below.
	OwnerOrg string `json:"owner_org,omitempty" gorm:"type:varchar(64);index;uniqueIndex:idx_image_workloads_owner_idem,priority:1"`
	// IdempotencyKey makes a job at-most-once. It is a POINTER on purpose: SQL
	// NULLs are distinct under a UNIQUE index, so every non-job workload (NULL)
	// is exempt from the constraint while two jobs from the SAME org can never
	// share a key. The (owner_org, idempotency_key) scope means one tenant's key
	// can never resolve to another tenant's handle (I28).
	IdempotencyKey *string `json:"idempotency_key,omitempty" gorm:"type:varchar(255);uniqueIndex:idx_image_workloads_owner_idem,priority:2"`
	// JobCommand is the ARGV-EXACT entrypoint override for the job class. It is
	// exec'd as-is by the guest init — never shell-interpolated (see image-init).
	JobCommand []string `json:"job_command,omitempty" gorm:"type:jsonb;serializer:json"`
	// EgressAllow is the job's network egress allowlist applied at boot.
	EgressAllow     []string           `json:"egress_allow,omitempty" gorm:"type:jsonb;serializer:json"`
	ImageRepository string             `json:"image_repository" gorm:"type:varchar(512)"`
	ImageDigest     string             `json:"image_digest" gorm:"type:varchar(255);index"`
	Class           ImageWorkloadClass `json:"class" gorm:"type:varchar(20);not null;index"`
	State           ImageWorkloadState `json:"state" gorm:"type:varchar(20);not null;default:'booting';index"`
	GuestIP         string             `json:"guest_ip,omitempty" gorm:"type:varchar(45)"`
	HostPort        int                `json:"host_port,omitempty" gorm:"not null;default:0"`
	Port            int                `json:"port,omitempty" gorm:"not null;default:0"`
	RootfsPath      string             `json:"rootfs_path,omitempty" gorm:"type:varchar(512)"`
	SocketPath      string             `json:"socket_path,omitempty" gorm:"type:varchar(512)"`
	TapName         string             `json:"tap_name,omitempty" gorm:"type:varchar(32)"`
	NetIndex        int                `json:"net_index,omitempty" gorm:"not null;default:0"`
	PID             *int               `json:"pid,omitempty"`
	ExitCode        *int               `json:"exit_code,omitempty"`
	StdoutTail      string             `json:"stdout_tail,omitempty" gorm:"type:text"`
	StderrTail      string             `json:"stderr_tail,omitempty" gorm:"type:text"`
	URL             string             `json:"url,omitempty" gorm:"type:varchar(512)"`
	Message         string             `json:"message,omitempty" gorm:"type:text"`
	CreatedAt       time.Time          `json:"created_at" gorm:"not null"`
	UpdatedAt       time.Time          `json:"updated_at" gorm:"not null"`
}

// TableName specifies the GORM table name.
func (ImageWorkload) TableName() string {
	return "image_workloads"
}
