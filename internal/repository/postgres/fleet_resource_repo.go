package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"gorm.io/gorm"
)

// fleetResourceRepository is the GORM-backed FleetResourceRepository.
type fleetResourceRepository struct {
	db *gorm.DB
}

var _ repository.FleetResourceRepository = (*fleetResourceRepository)(nil)

// NewFleetResourceRepository creates a new PostgreSQL fleet-resource repository.
func NewFleetResourceRepository(db *gorm.DB) *fleetResourceRepository {
	return &fleetResourceRepository{db: db}
}

func (r *fleetResourceRepository) SaveResource(ctx context.Context, resource *domain.FleetResource) error {
	return r.db.WithContext(ctx).Save(resource).Error
}

func (r *fleetResourceRepository) GetResourceByHandle(ctx context.Context, id uuid.UUID) (*domain.FleetResource, error) {
	var resource domain.FleetResource
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&resource).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrResourceNotFound
		}
		return nil, err
	}
	return &resource, nil
}

func (r *fleetResourceRepository) FindResource(ctx context.Context, ownerOrg uuid.UUID, claimKey, env string) (*domain.FleetResource, error) {
	var resource domain.FleetResource
	err := r.db.WithContext(ctx).
		Where("owner_org = ? AND claim_key = ? AND env = ?", ownerOrg, claimKey, env).
		First(&resource).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrResourceNotFound
		}
		return nil, err
	}
	return &resource, nil
}

// FindLiveResourceByApp resolves the live claim backed by appID (the
// fleet_resources_app_id_idx lookup). "Live" is decommissioned_at IS NULL AND
// phase <> 'decommissioned': BOTH, because the timestamp is stamped when a
// teardown starts and the phase only when it finishes, so either one alone
// would still call an in-flight teardown live and deadlock the resource's own
// path. Newest first purely for determinism — the (owner_org, claim_key, env)
// unique index means at most one live claim per app in practice.
func (r *fleetResourceRepository) FindLiveResourceByApp(ctx context.Context, appID uuid.UUID) (*domain.FleetResource, error) {
	var resource domain.FleetResource
	err := r.db.WithContext(ctx).
		Where("app_id = ? AND decommissioned_at IS NULL AND phase <> ?", appID, domain.FleetResourcePhaseDecommissioned).
		Order("created_at DESC").
		First(&resource).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrResourceNotFound
		}
		return nil, err
	}
	return &resource, nil
}

func (r *fleetResourceRepository) UpdateResourcePhase(ctx context.Context, id uuid.UUID, phase domain.FleetResourcePhase) error {
	res := r.db.WithContext(ctx).
		Model(&domain.FleetResource{}).
		Where("id = ?", id).
		Update("phase", phase)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrResourceNotFound
	}
	return nil
}

// CompareAndSwapPhase advances the phase only from one of `from`, reporting
// whether a row changed. Single statement — the WHERE runs under the row lock
// the UPDATE takes, so two concurrent callers cannot both observe 1 row.
func (r *fleetResourceRepository) CompareAndSwapPhase(ctx context.Context, id uuid.UUID, from []domain.FleetResourcePhase, to domain.FleetResourcePhase) (bool, error) {
	if len(from) == 0 {
		return false, nil
	}
	res := r.db.WithContext(ctx).
		Model(&domain.FleetResource{}).
		Where("id = ? AND phase IN ?", id, from).
		Updates(map[string]any{"phase": to, "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

func (r *fleetResourceRepository) SetResourceLastError(ctx context.Context, id uuid.UUID, msg string) error {
	res := r.db.WithContext(ctx).
		Model(&domain.FleetResource{}).
		Where("id = ?", id).
		Updates(map[string]any{"last_error": msg, "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrResourceNotFound
	}
	return nil
}

func (r *fleetResourceRepository) ListResourcesByPhase(ctx context.Context, phase domain.FleetResourcePhase) ([]domain.FleetResource, error) {
	var resources []domain.FleetResource
	err := r.db.WithContext(ctx).
		Where("phase = ? AND decommissioned_at IS NULL", phase).
		Order("updated_at ASC").
		Find(&resources).Error
	return resources, err
}

// ListExpiredShared returns shared-variant resources (app_id IS NULL) whose TTL
// has elapsed and that are not yet tombstoned. Dedicated resources carry an
// app_id and are reclaimed through their FleetApp, so they are excluded here.
func (r *fleetResourceRepository) ListExpiredShared(ctx context.Context, now time.Time) ([]domain.FleetResource, error) {
	var resources []domain.FleetResource
	err := r.db.WithContext(ctx).
		Where("app_id IS NULL AND expires_at IS NOT NULL AND expires_at <= ? AND decommissioned_at IS NULL", now).
		Order("expires_at ASC").
		Find(&resources).Error
	return resources, err
}

func (r *fleetResourceRepository) SaveRecoveryPoint(ctx context.Context, rp *domain.FleetResourceRecoveryPoint) error {
	return r.db.WithContext(ctx).Create(rp).Error
}

func (r *fleetResourceRepository) ListRecoveryPoints(ctx context.Context, resourceID uuid.UUID) ([]domain.FleetResourceRecoveryPoint, error) {
	var points []domain.FleetResourceRecoveryPoint
	err := r.db.WithContext(ctx).
		Where("resource_id = ?", resourceID).
		Order("created_at DESC").
		Find(&points).Error
	return points, err
}

// GetRecoveryPointByRef resolves a ref WITHIN one resource. The resource_id
// predicate is the security boundary, not an optimization: object keys are
// guessable-shaped (volumes/<vol>/<uuid>.ext4), so a global lookup would let a
// leaked key from another org's resource be restored into this one.
func (r *fleetResourceRepository) GetRecoveryPointByRef(ctx context.Context, resourceID uuid.UUID, objectKey string) (*domain.FleetResourceRecoveryPoint, error) {
	var rp domain.FleetResourceRecoveryPoint
	err := r.db.WithContext(ctx).
		Where("resource_id = ? AND object_key = ?", resourceID, objectKey).
		Order("created_at DESC").
		First(&rp).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrRecoveryPointNotFound
		}
		return nil, err
	}
	return &rp, nil
}

// MarkRecoveryPointRestoredInPlace sets the `verified` COLUMN, which records
// RestoredInPlaceOK — an in-place restore that came back serving, not a
// verification drill. The column keeps its 0012 name on purpose (see
// domain.FleetResourceRecoveryPoint.RestoredInPlaceOK).
func (r *fleetResourceRepository) MarkRecoveryPointRestoredInPlace(ctx context.Context, id uuid.UUID) error {
	res := r.db.WithContext(ctx).
		Model(&domain.FleetResourceRecoveryPoint{}).
		Where("id = ?", id).
		Update("verified", true)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrRecoveryPointNotFound
	}
	return nil
}
