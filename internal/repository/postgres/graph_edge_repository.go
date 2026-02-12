package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

type graphEdgeRepository struct {
	db *gorm.DB
}

// NewGraphEdgeRepository creates a new PostgreSQL graph edge repository
func NewGraphEdgeRepository(db *gorm.DB) *graphEdgeRepository {
	return &graphEdgeRepository{db: db}
}

func (r *graphEdgeRepository) Create(ctx context.Context, edge *domain.GraphEdge) error {
	return r.db.WithContext(ctx).Create(edge).Error
}

func (r *graphEdgeRepository) CreateBatch(ctx context.Context, edges []domain.GraphEdge) error {
	if len(edges) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Create(&edges).Error
}

func (r *graphEdgeRepository) FindByGraph(ctx context.Context, graphID uuid.UUID) ([]domain.GraphEdge, error) {
	var edges []domain.GraphEdge
	err := r.db.WithContext(ctx).
		Where("graph_id = ?", graphID).
		Order("created_at ASC").
		Find(&edges).Error
	return edges, err
}

func (r *graphEdgeRepository) DeleteByGraph(ctx context.Context, graphID uuid.UUID) error {
	return r.db.WithContext(ctx).Where("graph_id = ?", graphID).Delete(&domain.GraphEdge{}).Error
}
