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

// placementRepository is the GORM-backed PlacementRepository.
type placementRepository struct {
	db *gorm.DB
}

var _ repository.PlacementRepository = (*placementRepository)(nil)

// NewPlacementRepository creates a new PostgreSQL fleet-placement repository.
func NewPlacementRepository(db *gorm.DB) *placementRepository {
	return &placementRepository{db: db}
}

func (r *placementRepository) Upsert(ctx context.Context, placement *domain.Placement) error {
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "replica_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"host_id", "constraint_type"}),
	}).Create(placement).Error
}

func (r *placementRepository) FindByReplica(ctx context.Context, replicaID uuid.UUID) (*domain.Placement, error) {
	var placement domain.Placement
	err := r.db.WithContext(ctx).Where("replica_id = ?", replicaID).First(&placement).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrPlacementNotFound
		}
		return nil, err
	}
	return &placement, nil
}

func (r *placementRepository) Delete(ctx context.Context, replicaID uuid.UUID) error {
	return r.db.WithContext(ctx).Where("replica_id = ?", replicaID).Delete(&domain.Placement{}).Error
}
