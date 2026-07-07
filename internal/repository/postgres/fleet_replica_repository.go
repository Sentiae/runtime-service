package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"gorm.io/gorm"
)

// replicaRepository is the GORM-backed ReplicaRepository.
type replicaRepository struct {
	db *gorm.DB
}

var _ repository.ReplicaRepository = (*replicaRepository)(nil)

// NewReplicaRepository creates a new PostgreSQL fleet-replica repository.
func NewReplicaRepository(db *gorm.DB) *replicaRepository {
	return &replicaRepository{db: db}
}

func (r *replicaRepository) Create(ctx context.Context, replica *domain.Replica) error {
	return r.db.WithContext(ctx).Create(replica).Error
}

func (r *replicaRepository) Update(ctx context.Context, replica *domain.Replica) error {
	return r.db.WithContext(ctx).Save(replica).Error
}

func (r *replicaRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.Replica, error) {
	var replica domain.Replica
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&replica).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrReplicaNotFound
		}
		return nil, err
	}
	return &replica, nil
}

func (r *replicaRepository) ListByApp(ctx context.Context, appID uuid.UUID) ([]domain.Replica, error) {
	var replicas []domain.Replica
	err := r.db.WithContext(ctx).
		Where("app_id = ?", appID).
		Order("created_at ASC").
		Find(&replicas).Error
	return replicas, err
}

func (r *replicaRepository) ListByHost(ctx context.Context, hostID uuid.UUID) ([]domain.Replica, error) {
	var replicas []domain.Replica
	err := r.db.WithContext(ctx).
		Where("host_id = ?", hostID).
		Order("created_at ASC").
		Find(&replicas).Error
	return replicas, err
}

func (r *replicaRepository) ListByState(ctx context.Context, state domain.ReplicaState) ([]domain.Replica, error) {
	var replicas []domain.Replica
	err := r.db.WithContext(ctx).
		Where("state = ?", state).
		Order("created_at ASC").
		Find(&replicas).Error
	return replicas, err
}

func (r *replicaRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.db.WithContext(ctx).Where("id = ?", id).Delete(&domain.Replica{}).Error
}
