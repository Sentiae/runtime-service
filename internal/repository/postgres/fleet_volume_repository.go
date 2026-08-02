package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

func (r *volumeRepository) BindVolumesToResource(ctx context.Context, appID, resourceID uuid.UUID) (repository.VolumeBindResult, error) {
	var out repository.VolumeBindResult
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var volumes []domain.Volume
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("app_id = ?", appID).
			Order("created_at ASC").
			Find(&volumes).Error; err != nil {
			return err
		}
		if len(volumes) == 0 {
			out = repository.VolumeBindResult{Outcome: repository.VolumeBindNoVolumes}
			return nil
		}
		for i := range volumes {
			if volumes[i].ResourceID != nil && *volumes[i].ResourceID != resourceID {
				out = repository.VolumeBindResult{
					Outcome:          repository.VolumeBindConflict,
					ConflictVolumeID: volumes[i].ID,
					ConflictOwner:    *volumes[i].ResourceID,
				}
				return nil
			}
		}
		res := tx.Model(&domain.Volume{}).
			Where("app_id = ? AND resource_id IS NULL", appID).
			Updates(map[string]any{
				"resource_id": resourceID,
				"updated_at":  gorm.Expr("now()"),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected > 0 {
			out = repository.VolumeBindResult{Outcome: repository.VolumeBindBound}
			return nil
		}
		out = repository.VolumeBindResult{Outcome: repository.VolumeBindAlreadyBound}
		return nil
	})
	if err != nil {
		return repository.VolumeBindResult{}, err
	}
	return out, nil
}

func (r *volumeRepository) HasUnstampedVolumes(ctx context.Context, appID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.WithContext(ctx).
		Raw("SELECT EXISTS(SELECT 1 FROM fleet_volumes WHERE app_id = ? AND resource_id IS NULL)", appID).
		Scan(&exists).Error
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (r *volumeRepository) ListByHost(ctx context.Context, hostID uuid.UUID) ([]domain.Volume, error) {
	var volumes []domain.Volume
	err := r.db.WithContext(ctx).
		Where("host_affinity = ?", hostID).
		Order("created_at ASC").
		Find(&volumes).Error
	return volumes, err
}
