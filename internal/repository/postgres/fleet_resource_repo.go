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
