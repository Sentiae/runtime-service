package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

type graphTraceSnapshotRepository struct {
	db *gorm.DB
}

// NewGraphTraceSnapshotRepository creates a new PostgreSQL graph trace snapshot repository
func NewGraphTraceSnapshotRepository(db *gorm.DB) *graphTraceSnapshotRepository {
	return &graphTraceSnapshotRepository{db: db}
}

func (r *graphTraceSnapshotRepository) Create(ctx context.Context, snapshot *domain.GraphTraceNodeSnapshot) error {
	return r.db.WithContext(ctx).Create(snapshot).Error
}

func (r *graphTraceSnapshotRepository) FindByTrace(ctx context.Context, traceID uuid.UUID) ([]domain.GraphTraceNodeSnapshot, error) {
	var snapshots []domain.GraphTraceNodeSnapshot
	err := r.db.WithContext(ctx).
		Where("trace_id = ?", traceID).
		Order("sequence_number ASC").
		Find(&snapshots).Error
	return snapshots, err
}

func (r *graphTraceSnapshotRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.GraphTraceNodeSnapshot, error) {
	var snapshot domain.GraphTraceNodeSnapshot
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&snapshot).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrTraceNotFound
		}
		return nil, err
	}
	return &snapshot, nil
}

func (r *graphTraceSnapshotRepository) DeleteByTrace(ctx context.Context, traceID uuid.UUID) error {
	return r.db.WithContext(ctx).Where("trace_id = ?", traceID).Delete(&domain.GraphTraceNodeSnapshot{}).Error
}
