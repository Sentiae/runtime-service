package postgres

import (
	"context"
	"errors"

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

func (r *volumeRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.Volume, error) {
	var volume domain.Volume
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&volume).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrVolumeNotFound
		}
		return nil, err
	}
	return &volume, nil
}

func (r *volumeRepository) Update(ctx context.Context, volume *domain.Volume) error {
	return r.db.WithContext(ctx).Save(volume).Error
}

func (r *volumeRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.db.WithContext(ctx).Where("id = ?", id).Delete(&domain.Volume{}).Error
}

func (r *volumeRepository) ListByHost(ctx context.Context, hostID uuid.UUID) ([]domain.Volume, error) {
	var volumes []domain.Volume
	err := r.db.WithContext(ctx).
		Where("host_affinity = ?", hostID).
		Order("created_at ASC").
		Find(&volumes).Error
	return volumes, err
}
