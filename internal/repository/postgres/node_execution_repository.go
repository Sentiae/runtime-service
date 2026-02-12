package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

type nodeExecutionRepository struct {
	db *gorm.DB
}

// NewNodeExecutionRepository creates a new PostgreSQL node execution repository
func NewNodeExecutionRepository(db *gorm.DB) *nodeExecutionRepository {
	return &nodeExecutionRepository{db: db}
}

func (r *nodeExecutionRepository) Create(ctx context.Context, exec *domain.NodeExecution) error {
	return r.db.WithContext(ctx).Create(exec).Error
}

func (r *nodeExecutionRepository) Update(ctx context.Context, exec *domain.NodeExecution) error {
	return r.db.WithContext(ctx).Save(exec).Error
}

func (r *nodeExecutionRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.NodeExecution, error) {
	var exec domain.NodeExecution
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&exec).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrNodeExecutionNotFound
		}
		return nil, err
	}
	return &exec, nil
}

func (r *nodeExecutionRepository) FindByGraphExecution(ctx context.Context, graphExecID uuid.UUID) ([]domain.NodeExecution, error) {
	var execs []domain.NodeExecution
	err := r.db.WithContext(ctx).
		Where("graph_execution_id = ?", graphExecID).
		Order("sequence_number ASC").
		Find(&execs).Error
	return execs, err
}
