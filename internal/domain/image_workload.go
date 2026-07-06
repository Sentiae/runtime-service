package domain

import (
	"time"

	"github.com/google/uuid"
)

// ImageWorkloadClass is the class of an image-boot workload. CP3 supports
// exactly two: an ephemeral "test" run (boot → run → exit → collect output)
// and a "resident" workload (boot → stay up serving a port).
type ImageWorkloadClass string

const (
	// ImageWorkloadClassTest is a single-shot run: the VM boots the image,
	// runs its entrypoint (or an override command), powers off, and the host
	// collects stdout/stderr/exit_code.
	ImageWorkloadClassTest ImageWorkloadClass = "test"
	// ImageWorkloadClassResident is a long-lived workload: the VM boots the
	// image and stays up; the host advertises a port DNAT'd to the guest.
	ImageWorkloadClassResident ImageWorkloadClass = "resident"
)

// IsValid reports whether the class is one CP3 supports.
func (c ImageWorkloadClass) IsValid() bool {
	switch c {
	case ImageWorkloadClassTest, ImageWorkloadClassResident:
		return true
	}
	return false
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
	ID              uuid.UUID          `json:"id" gorm:"type:uuid;primary_key"`
	ComponentID     string             `json:"component_id" gorm:"type:varchar(255);index"`
	Env             string             `json:"env" gorm:"type:varchar(64)"`
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
