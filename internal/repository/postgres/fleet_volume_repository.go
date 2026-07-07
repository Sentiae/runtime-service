package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"gorm.io/gorm"
)

// volumeRepository is the GORM-backed VolumeRepository.
type volumeRepository struct {
	db *gorm.DB
}

var _ repository.VolumeRepository = (*volumeRepository)(nil)

// NewVolumeRepository creates a new PostgreSQL fleet-volume repository.
func NewVolumeRepository(db *gorm.DB) *volumeRepository {
	return &volumeRepository{db: db}
}

func (r *volumeRepository) Create(ctx context.Context, volume *domain.Volume) error {
	return r.db.WithContext(ctx).Create(volume).Error
}

func (r *volumeRepository) ListByApp(ctx context.Context, appID uuid.UUID) ([]domain.Volume, error) {
	var volumes []domain.Volume
	err := r.db.WithContext(ctx).
		Where("app_id = ?", appID).
		Order("created_at ASC").
		Find(&volumes).Error
	return volumes, err
}
