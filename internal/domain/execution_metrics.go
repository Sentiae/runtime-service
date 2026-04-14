package domain

import (
	"time"

	"github.com/google/uuid"
)

// ExecutionMetrics captures resource usage during an execution
type ExecutionMetrics struct {
	ID            uuid.UUID `json:"id" gorm:"type:uuid;primary_key"`
	ExecutionID   uuid.UUID `json:"execution_id" gorm:"type:uuid;not null;uniqueIndex"`
	VMID          uuid.UUID `json:"vm_id" gorm:"type:uuid;not null;index"`
	CPUTimeMS     int64     `json:"cpu_time_ms" gorm:"not null;default:0"`
	MemoryPeakMB  float64   `json:"memory_peak_mb" gorm:"not null;default:0"`
	MemoryAvgMB   float64   `json:"memory_avg_mb" gorm:"not null;default:0"`
	IOReadBytes   int64     `json:"io_read_bytes" gorm:"not null;default:0"`
	IOWriteBytes  int64     `json:"io_write_bytes" gorm:"not null;default:0"`
	NetBytesIn    int64     `json:"net_bytes_in" gorm:"not null;default:0"`
	NetBytesOut   int64     `json:"net_bytes_out" gorm:"not null;default:0"`
	BootTimeMS    int64     `json:"boot_time_ms" gorm:"not null;default:0"`
	CompileTimeMS *int64    `json:"compile_time_ms,omitempty"`
	ExecTimeMS    int64     `json:"exec_time_ms" gorm:"not null;default:0"`
	TotalTimeMS   int64     `json:"total_time_ms" gorm:"not null;default:0"`
	CollectedAt   time.Time `json:"collected_at" gorm:"not null"`
}

// TableName specifies the table name for GORM
func (ExecutionMetrics) TableName() string {
	return "execution_metrics"
}

// Validate performs validation on the metrics
func (m *ExecutionMetrics) Validate() error {
	if m.ID == uuid.Nil {
		return ErrInvalidID
	}
	if m.ExecutionID == uuid.Nil {
		return ErrInvalidID
	}
	if m.VMID == uuid.Nil {
		return ErrInvalidID
	}
	return nil
}
