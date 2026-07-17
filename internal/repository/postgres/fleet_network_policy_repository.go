package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"gorm.io/gorm"
)

// fleetNetworkPolicyRepository is the GORM-backed FleetNetworkPolicyRepository.
type fleetNetworkPolicyRepository struct {
	db *gorm.DB
}

var _ repository.FleetNetworkPolicyRepository = (*fleetNetworkPolicyRepository)(nil)

// NewFleetNetworkPolicyRepository creates a new PostgreSQL policy repository.
func NewFleetNetworkPolicyRepository(db *gorm.DB) *fleetNetworkPolicyRepository {
	return &fleetNetworkPolicyRepository{db: db}
}

// ReplaceForNetwork swaps the network's COMPLETE policy set inside ONE
// transaction: a half-applied policy set is a lie about enforcement, so the
// delete and the insert either both land or neither does.
func (r *fleetNetworkPolicyRepository) ReplaceForNetwork(ctx context.Context, networkID uuid.UUID, ps []domain.FleetNetworkPolicy) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("network_id = ?", networkID).
			Delete(&domain.FleetNetworkPolicy{}).Error; err != nil {
			return err
		}
		if len(ps) == 0 {
			// The empty set is valid and means "revoke everything" — it is the most
			// restrictive set, not a no-op.
			return nil
		}
		return tx.Create(&ps).Error
	})
}

func (r *fleetNetworkPolicyRepository) ListForNetwork(ctx context.Context, networkID uuid.UUID) ([]domain.FleetNetworkPolicy, error) {
	var ps []domain.FleetNetworkPolicy
	if err := r.db.WithContext(ctx).
		Where("network_id = ?", networkID).
		Order("created_at").
		Find(&ps).Error; err != nil {
		return nil, err
	}
	return ps, nil
}
