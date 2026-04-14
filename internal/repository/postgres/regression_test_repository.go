package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

// RegressionTestRepo persists regression test templates generated from
// captured production traces.
type RegressionTestRepo struct {
	db *gorm.DB
}

// NewRegressionTestRepository constructs the repo.
func NewRegressionTestRepository(db *gorm.DB) *RegressionTestRepo {
	return &RegressionTestRepo{db: db}
}

// Create inserts a new template row.
func (r *RegressionTestRepo) Create(ctx context.Context, t *domain.RegressionTestTemplate) error {
	return r.db.WithContext(ctx).Create(t).Error
}

// FindByID retrieves a template by primary key. Returns (nil, nil)
// when no row matches so HTTP handlers can map that to 404 cleanly.
func (r *RegressionTestRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.RegressionTestTemplate, error) {
	var t domain.RegressionTestTemplate
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&t).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

// FindByTrace retrieves the template generated for a trace, if any.
func (r *RegressionTestRepo) FindByTrace(ctx context.Context, traceID string) (*domain.RegressionTestTemplate, error) {
	var t domain.RegressionTestTemplate
	err := r.db.WithContext(ctx).Where("trace_id = ?", traceID).First(&t).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

// ListByOrganization returns templates for an organization, newest first.
func (r *RegressionTestRepo) ListByOrganization(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]domain.RegressionTestTemplate, int64, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows []domain.RegressionTestTemplate
	var total int64
	q := r.db.WithContext(ctx).Where("organization_id = ?", orgID)
	if err := q.Model(&domain.RegressionTestTemplate{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if err := q.Order("created_at DESC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// Update saves an existing row (used by callers wanting to back-fill
// the linked TestRunID after the generated test is first executed).
func (r *RegressionTestRepo) Update(ctx context.Context, t *domain.RegressionTestTemplate) error {
	return r.db.WithContext(ctx).Save(t).Error
}
