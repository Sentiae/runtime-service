package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

type egressDecisionModel struct {
	ID           uuid.UUID `gorm:"type:uuid;primaryKey"`
	RunID        uuid.UUID `gorm:"column:run_id;type:uuid;not null"`
	InvocationID string    `gorm:"column:invocation_id;not null"`
	Node         string    `gorm:"not null"`
	Decision     string    `gorm:"not null"`
	Reason       string    `gorm:"not null"`
	Host         string    `gorm:"not null"`
	HostRedacted bool      `gorm:"column:host_redacted;not null"`
	Port         int       `gorm:"not null"`
	RequestCount int64     `gorm:"column:request_count;not null"`
	FirstSeenAt  time.Time `gorm:"column:first_seen_at;not null"`
	LastSeenAt   time.Time `gorm:"column:last_seen_at;not null"`
	Capped       bool      `gorm:"not null"`
	RecordedAt   time.Time `gorm:"column:recorded_at;not null"`
}

func (egressDecisionModel) TableName() string { return "node_egress_decisions" }

type egressAuditRepository struct{ db *gorm.DB }

var _ repository.EgressAuditRepository = (*egressAuditRepository)(nil)

// NewEgressAuditRepository builds the audit store over the service DB.
func NewEgressAuditRepository(db *gorm.DB) *egressAuditRepository {
	return &egressAuditRepository{db: db}
}

// Record writes one sidecar's aggregated decisions in one statement. The tuple
// unique index plus DO NOTHING makes a repeated drain of the same sidecar a
// no-op rather than a duplicate.
//
// The session's logger is DISCARDED: a refused INSERT is the one moment a host
// a tenant chose could be printed into the runtime's own log by the ORM, and
// the returned error carries the SQLSTATE without the row.
func (r *egressAuditRepository) Record(ctx context.Context, decisions []domain.EgressDecision) error {
	if len(decisions) == 0 {
		return nil
	}
	now := time.Now().UTC()
	rows := make([]egressDecisionModel, 0, len(decisions))
	for _, d := range decisions {
		if err := d.Validate(); err != nil {
			return fmt.Errorf("egress audit row: %w", err)
		}
		rows = append(rows, egressDecisionModel{
			ID: uuid.New(), RunID: d.RunID, InvocationID: d.InvocationID, Node: d.Node,
			Decision: string(d.Decision), Reason: d.Reason, Host: d.Host, HostRedacted: d.HostRedacted,
			Port: d.Port, RequestCount: d.Hits, FirstSeenAt: d.FirstAt, LastSeenAt: d.LastAt,
			Capped: d.Capped, RecordedAt: now,
		})
	}
	err := r.db.WithContext(ctx).
		Session(&gorm.Session{Logger: gormlogger.Discard}).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&rows).Error
	if err != nil {
		return fmt.Errorf("record egress audit: %w", err)
	}
	return nil
}
