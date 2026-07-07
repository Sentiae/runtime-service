package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"gorm.io/gorm"
)

// secretBindingRepository is the GORM-backed SecretBindingRepository.
type secretBindingRepository struct {
	db *gorm.DB
}

var _ repository.SecretBindingRepository = (*secretBindingRepository)(nil)

// NewSecretBindingRepository creates a new PostgreSQL fleet-secret-binding repository.
func NewSecretBindingRepository(db *gorm.DB) *secretBindingRepository {
	return &secretBindingRepository{db: db}
}

func (r *secretBindingRepository) Create(ctx context.Context, binding *domain.SecretBinding) error {
	return r.db.WithContext(ctx).Create(binding).Error
}

func (r *secretBindingRepository) ListByApp(ctx context.Context, appID uuid.UUID) ([]domain.SecretBinding, error) {
	var bindings []domain.SecretBinding
	err := r.db.WithContext(ctx).
		Where("app_id = ?", appID).
		Order("created_at ASC").
		Find(&bindings).Error
	return bindings, err
}

func (r *secretBindingRepository) DeleteByApp(ctx context.Context, appID uuid.UUID) error {
	return r.db.WithContext(ctx).Where("app_id = ?", appID).Delete(&domain.SecretBinding{}).Error
}
