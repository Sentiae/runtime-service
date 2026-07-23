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

// fleetNetworkRepository is the GORM-backed FleetNetworkRepository (CP4.5 §9 #5).
type fleetNetworkRepository struct {
	db *gorm.DB
}

var _ repository.FleetNetworkRepository = (*fleetNetworkRepository)(nil)

// NewFleetNetworkRepository creates a new PostgreSQL fleet-network repository.
func NewFleetNetworkRepository(db *gorm.DB) *fleetNetworkRepository {
	return &fleetNetworkRepository{db: db}
}

func (r *fleetNetworkRepository) Create(ctx context.Context, n *domain.FleetNetwork) error {
	return r.db.WithContext(ctx).Create(n).Error
}

func (r *fleetNetworkRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.FleetNetwork, error) {
	var n domain.FleetNetwork
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&n).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrFleetNetworkNotFound
		}
		return nil, err
	}
	return &n, nil
}

func (r *fleetNetworkRepository) FindBySystemEnv(ctx context.Context, systemID, env string) (*domain.FleetNetwork, error) {
	// An empty system_id is not a lookup key — it is the absence of membership.
	// Resolving it to a row would let an unscoped workload inherit a network.
	if systemID == "" {
		return nil, domain.ErrFleetNetworkNotFound
	}
	var n domain.FleetNetwork
	err := r.db.WithContext(ctx).
		Where("system_id = ? AND env = ?", systemID, env).
		First(&n).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrFleetNetworkNotFound
		}
		return nil, err
	}
	return &n, nil
}

func (r *fleetNetworkRepository) ListActive(ctx context.Context) ([]domain.FleetNetwork, error) {
	var ns []domain.FleetNetwork
	if err := r.db.WithContext(ctx).
		Where("status = ?", string(domain.FleetNetworkActive)).
		Find(&ns).Error; err != nil {
		return nil, err
	}
	return ns, nil
}

// MarkDeprovisioned tombstones the network (SD3) — the row is never deleted, so
// the scope key stays resolvable for audit after teardown.
func (r *fleetNetworkRepository) MarkDeprovisioned(ctx context.Context, id uuid.UUID) error {
	res := r.db.WithContext(ctx).
		Model(&domain.FleetNetwork{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":     string(domain.FleetNetworkDeprovisioned),
			"updated_at": time.Now().UTC(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrFleetNetworkNotFound
	}
	return nil
}

// MarkActive revives a tombstoned scope by reusing its existing row: status flips
// back to 'active' and updated_at is bumped. Reusing the row is the point of the
// ruling — a second (system_id, env) row would violate uq_fleet_networks_system_env,
// so a re-EnsureNetwork after Deprovision goes through here instead (D-179 §807,
// #fleet-network-revive-after-teardown). There is no separate tombstone timestamp
// column to clear — the status field IS the tombstone.
func (r *fleetNetworkRepository) MarkActive(ctx context.Context, id uuid.UUID) error {
	res := r.db.WithContext(ctx).
		Model(&domain.FleetNetwork{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":     string(domain.FleetNetworkActive),
			"updated_at": time.Now().UTC(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrFleetNetworkNotFound
	}
	return nil
}
