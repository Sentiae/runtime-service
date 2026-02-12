package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

type graphExecutionRepository struct {
	db *gorm.DB
}

// NewGraphExecutionRepository creates a new PostgreSQL graph execution repository
func NewGraphExecutionRepository(db *gorm.DB) *graphExecutionRepository {
	return &graphExecutionRepository{db: db}
}

func (r *graphExecutionRepository) Create(ctx context.Context, exec *domain.GraphExecution) error {
	return r.db.WithContext(ctx).Create(exec).Error
}

func (r *graphExecutionRepository) Update(ctx context.Context, exec *domain.GraphExecution) error {
	return r.db.WithContext(ctx).Save(exec).Error
}

func (r *graphExecutionRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.GraphExecution, error) {
	var exec domain.GraphExecution
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&exec).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrGraphExecutionNotFound
		}
		return nil, err
	}
	return &exec, nil
}

func (r *graphExecutionRepository) FindByGraph(ctx context.Context, graphID uuid.UUID, limit, offset int) ([]domain.GraphExecution, int64, error) {
	var execs []domain.GraphExecution
	var total int64

	query := r.db.WithContext(ctx).Where("graph_id = ?", graphID)
	if err := query.Model(&domain.GraphExecution{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.Order("created_at DESC").Limit(limit).Offset(offset).Find(&execs).Error
	return execs, total, err
}

func (r *graphExecutionRepository) FindPending(ctx context.Context, limit int) ([]domain.GraphExecution, error) {
	var execs []domain.GraphExecution
	err := r.db.WithContext(ctx).
		Where("status = ?", domain.GraphExecPending).
		Order("created_at ASC").
		Limit(limit).
		Find(&execs).Error
	return execs, err
}
