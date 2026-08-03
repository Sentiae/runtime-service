package postgres

import (
	"context"
	"errors"
	"fmt"

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

// BindHostAffinity is the per-volume host CAS (#fleet-reconciler-acts-on-
// foreign-host-replicas). It mirrors BindVolumesToResource's transactional shape
// deliberately: SELECT ... FOR UPDATE, decide, then a TARGETED update whose WHERE
// re-states the precondition — never a read + Save, which lets two concurrent
// adopters each observe host_affinity NULL and each believe it won.
//
// The row lock makes the compare, the set and the reported outcome one atomic
// command, so the caller acts on what the DATABASE decided rather than on state
// it read a moment earlier. A non-nil affinity is never overwritten: it is the
// physical location of the customer's bytes.
func (r *volumeRepository) BindHostAffinity(ctx context.Context, volumeID, hostID uuid.UUID) (repository.VolumeHostBindResult, error) {
	var out repository.VolumeHostBindResult
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var vol domain.Volume
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", volumeID).
			First(&vol).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return domain.ErrVolumeNotFound
			}
			return err
		}
		if vol.HostAffinity != nil {
			if *vol.HostAffinity == hostID {
				out = repository.VolumeHostBindResult{Outcome: repository.VolumeHostBindAlreadyBound}
				return nil
			}
			out = repository.VolumeHostBindResult{
				Outcome:    repository.VolumeHostBindConflict,
				ActualHost: *vol.HostAffinity,
			}
			return nil
		}
		// The IS NULL predicate is redundant under the row lock and is kept anyway:
		// it is the statement's own proof of what it may overwrite, so the write can
		// never widen if the lock above is ever weakened.
		res := tx.Model(&domain.Volume{}).
			Where("id = ? AND host_affinity IS NULL", volumeID).
			Updates(map[string]any{
				"host_affinity": hostID,
				"updated_at":    gorm.Expr("now()"),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return fmt.Errorf("bind volume %s host affinity: expected 1 row, updated %d", volumeID, res.RowsAffected)
		}
		out = repository.VolumeHostBindResult{Outcome: repository.VolumeHostBindBound}
		return nil
	})
	if err != nil {
		return repository.VolumeHostBindResult{}, err
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

// ListByResource returns the volumes a durable claim OWNS (D-203). Filtered on
// resource_id alone — never on app_id — because the claim's ownership is what
// survives an app rebuild, and it is the ownership, not the current attachment,
// that says where the resource's bytes live.
func (r *volumeRepository) ListByResource(ctx context.Context, resourceID uuid.UUID) ([]domain.Volume, error) {
	var volumes []domain.Volume
	err := r.db.WithContext(ctx).
		Where("resource_id = ?", resourceID).
		Order("created_at ASC").
		Find(&volumes).Error
	return volumes, err
}

func (r *volumeRepository) ListByHost(ctx context.Context, hostID uuid.UUID) ([]domain.Volume, error) {
	var volumes []domain.Volume
	err := r.db.WithContext(ctx).
		Where("host_affinity = ?", hostID).
		Order("created_at ASC").
		Find(&volumes).Error
	return volumes, err
}
