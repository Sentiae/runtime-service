package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

type graphNodeRepository struct {
	db *gorm.DB
}

// NewGraphNodeRepository creates a new PostgreSQL graph node repository
func NewGraphNodeRepository(db *gorm.DB) *graphNodeRepository {
	return &graphNodeRepository{db: db}
}

func (r *graphNodeRepository) Create(ctx context.Context, node *domain.GraphNode) error {
	return r.db.WithContext(ctx).Create(node).Error
}

func (r *graphNodeRepository) CreateBatch(ctx context.Context, nodes []domain.GraphNode) error {
	if len(nodes) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Create(&nodes).Error
}

func (r *graphNodeRepository) Update(ctx context.Context, node *domain.GraphNode) error {
	return r.db.WithContext(ctx).Save(node).Error
}

func (r *graphNodeRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.GraphNode, error) {
	var node domain.GraphNode
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&node).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrGraphNodeNotFound
		}
		return nil, err
	}
	return &node, nil
}

func (r *graphNodeRepository) FindByGraph(ctx context.Context, graphID uuid.UUID) ([]domain.GraphNode, error) {
	var nodes []domain.GraphNode
	err := r.db.WithContext(ctx).
		Where("graph_id = ?", graphID).
		Order("sort_order ASC, created_at ASC").
		Find(&nodes).Error
	return nodes, err
}

func (r *graphNodeRepository) DeleteByGraph(ctx context.Context, graphID uuid.UUID) error {
	return r.db.WithContext(ctx).Where("graph_id = ?", graphID).Delete(&domain.GraphNode{}).Error
}
