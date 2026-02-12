package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

type executionMetricsRepository struct {
	db *gorm.DB
}

// NewExecutionMetricsRepository creates a new PostgreSQL metrics repository
func NewExecutionMetricsRepository(db *gorm.DB) *executionMetricsRepository {
	return &executionMetricsRepository{db: db}
}

func (r *executionMetricsRepository) Create(ctx context.Context, metrics *domain.ExecutionMetrics) error {
	return r.db.WithContext(ctx).Create(metrics).Error
}

func (r *executionMetricsRepository) FindByExecution(ctx context.Context, executionID uuid.UUID) (*domain.ExecutionMetrics, error) {
	var metrics domain.ExecutionMetrics
	err := r.db.WithContext(ctx).Where("execution_id = ?", executionID).First(&metrics).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &metrics, nil
}

func (r *executionMetricsRepository) FindByVM(ctx context.Context, vmID uuid.UUID, limit int) ([]domain.ExecutionMetrics, error) {
	var metrics []domain.ExecutionMetrics
	err := r.db.WithContext(ctx).
		Where("vm_id = ?", vmID).
		Order("collected_at DESC").
		Limit(limit).
		Find(&metrics).Error
	return metrics, err
}
