package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

type graphTraceRepository struct {
	db *gorm.DB
}

// NewGraphTraceRepository creates a new PostgreSQL graph trace repository
func NewGraphTraceRepository(db *gorm.DB) *graphTraceRepository {
	return &graphTraceRepository{db: db}
}

func (r *graphTraceRepository) Create(ctx context.Context, trace *domain.GraphExecutionTrace) error {
	return r.db.WithContext(ctx).Create(trace).Error
}

func (r *graphTraceRepository) Update(ctx context.Context, trace *domain.GraphExecutionTrace) error {
	return r.db.WithContext(ctx).Save(trace).Error
}

func (r *graphTraceRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.GraphExecutionTrace, error) {
	var trace domain.GraphExecutionTrace
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&trace).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrTraceNotFound
		}
		return nil, err
	}
	return &trace, nil
}

func (r *graphTraceRepository) FindByExecution(ctx context.Context, execID uuid.UUID) (*domain.GraphExecutionTrace, error) {
	var trace domain.GraphExecutionTrace
	err := r.db.WithContext(ctx).Where("graph_execution_id = ?", execID).First(&trace).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrTraceNotFound
		}
		return nil, err
	}
	return &trace, nil
}

func (r *graphTraceRepository) FindByGraph(ctx context.Context, graphID uuid.UUID, limit, offset int) ([]domain.GraphExecutionTrace, int64, error) {
	var traces []domain.GraphExecutionTrace
	var total int64

	query := r.db.WithContext(ctx).Where("graph_id = ?", graphID)
	if err := query.Model(&domain.GraphExecutionTrace{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.Order("created_at DESC").Limit(limit).Offset(offset).Find(&traces).Error
	return traces, total, err
}

func (r *graphTraceRepository) Delete(ctx context.Context, id uuid.UUID) error {
	result := r.db.WithContext(ctx).Where("id = ?", id).Delete(&domain.GraphExecutionTrace{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrTraceNotFound
	}
	return nil
}
