package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

type executionRepository struct {
	db *gorm.DB
}

// NewExecutionRepository creates a new PostgreSQL execution repository
func NewExecutionRepository(db *gorm.DB) *executionRepository {
	return &executionRepository{db: db}
}

func (r *executionRepository) Create(ctx context.Context, execution *domain.Execution) error {
	return r.db.WithContext(ctx).Create(execution).Error
}

func (r *executionRepository) Update(ctx context.Context, execution *domain.Execution) error {
	return r.db.WithContext(ctx).Save(execution).Error
}

func (r *executionRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.Execution, error) {
	var execution domain.Execution
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&execution).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrExecutionNotFound
		}
		return nil, err
	}
	return &execution, nil
}

func (r *executionRepository) FindByOrganization(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]domain.Execution, int64, error) {
	var executions []domain.Execution
	var total int64

	query := r.db.WithContext(ctx).Where("organization_id = ?", orgID)
	if err := query.Model(&domain.Execution{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.Order("created_at DESC").Limit(limit).Offset(offset).Find(&executions).Error
	return executions, total, err
}

func (r *executionRepository) FindPending(ctx context.Context, limit int) ([]domain.Execution, error) {
	var executions []domain.Execution
	err := r.db.WithContext(ctx).
		Where("status = ?", domain.ExecutionStatusPending).
		Order("created_at ASC").
		Limit(limit).
		Find(&executions).Error
	return executions, err
}

func (r *executionRepository) FindRunning(ctx context.Context) ([]domain.Execution, error) {
	var executions []domain.Execution
	err := r.db.WithContext(ctx).
		Where("status = ?", domain.ExecutionStatusRunning).
		Order("started_at ASC").
		Find(&executions).Error
	return executions, err
}
