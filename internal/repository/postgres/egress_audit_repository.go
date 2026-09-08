package postgres

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/gormlog"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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

type egressAuditRepository struct {
	db *gorm.DB
	// silentSession is the session Record runs under, carrying a logger that
	// writes nothing. It is built ONCE in the constructor rather than per
	// Record call: Record is on the sidecar drain path. Sharing one value is
	// safe — (*gorm.DB).Session only READS its argument (gorm v1.31.2
	// gorm.go), copying the fields it needs into a new *gorm.DB.
	//
	// Typed *gorm.Session, not the ORM logger interface, so this file needs no
	// import of gorm's logger package at all (D-400 rule A).
	silentSession *gorm.Session
}

var _ repository.EgressAuditRepository = (*egressAuditRepository)(nil)

// NewEgressAuditRepository builds the audit store over the service DB.
//
// It returns an error because the session logger comes from gormlog.New, the
// fleet's only approved ORM logger (D-400), which is fail-closed on its level
// name. A store that could not build that logger must not be handed out: the
// alternative is a session that falls back to the DB's own logger and prints
// the very host this method exists to keep out of the log.
func NewEgressAuditRepository(db *gorm.DB) (*egressAuditRepository, error) {
	// io.Discard at level "silent" reproduces the previous logger.Discard
	// exactly — this chain logs nothing — while keeping the construction inside
	// gormlog, so no site in this service builds a GORM logger of its own.
	silent, err := gormlog.New(io.Discard, "silent")
	if err != nil {
		return nil, fmt.Errorf("build silent gorm logger: %w", err)
	}
	return &egressAuditRepository{db: db, silentSession: &gorm.Session{Logger: silent}}, nil
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
		Session(r.silentSession).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&rows).Error
	if err != nil {
		return fmt.Errorf("record egress audit: %w", err)
	}
	return nil
}
