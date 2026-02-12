package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

type graphDefinitionRepository struct {
	db *gorm.DB
}

// NewGraphDefinitionRepository creates a new PostgreSQL graph definition repository
func NewGraphDefinitionRepository(db *gorm.DB) *graphDefinitionRepository {
	return &graphDefinitionRepository{db: db}
}

func (r *graphDefinitionRepository) Create(ctx context.Context, graph *domain.GraphDefinition) error {
	return r.db.WithContext(ctx).Create(graph).Error
}

func (r *graphDefinitionRepository) Update(ctx context.Context, graph *domain.GraphDefinition) error {
	return r.db.WithContext(ctx).Save(graph).Error
}

func (r *graphDefinitionRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.GraphDefinition, error) {
	var graph domain.GraphDefinition
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&graph).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrGraphNotFound
		}
		return nil, err
	}
	return &graph, nil
}

func (r *graphDefinitionRepository) FindByOrganization(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]domain.GraphDefinition, int64, error) {
	var graphs []domain.GraphDefinition
	var total int64

	query := r.db.WithContext(ctx).Where("organization_id = ?", orgID)
	if err := query.Model(&domain.GraphDefinition{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.Order("created_at DESC").Limit(limit).Offset(offset).Find(&graphs).Error
	return graphs, total, err
}

func (r *graphDefinitionRepository) FindActive(ctx context.Context, orgID uuid.UUID) ([]domain.GraphDefinition, error) {
	var graphs []domain.GraphDefinition
	err := r.db.WithContext(ctx).
		Where("organization_id = ? AND status = ?", orgID, domain.GraphStatusActive).
		Order("name ASC").
		Find(&graphs).Error
	return graphs, err
}

func (r *graphDefinitionRepository) Delete(ctx context.Context, id uuid.UUID) error {
	result := r.db.WithContext(ctx).Where("id = ?", id).Delete(&domain.GraphDefinition{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrGraphNotFound
	}
	return nil
}
