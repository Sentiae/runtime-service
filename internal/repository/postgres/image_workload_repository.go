package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"gorm.io/gorm"
)

// imageWorkloadRepository is the GORM-backed ImageWorkloadRepository.
type imageWorkloadRepository struct {
	db *gorm.DB
}

var _ repository.ImageWorkloadRepository = (*imageWorkloadRepository)(nil)

// NewImageWorkloadRepository creates a new PostgreSQL image-workload repository.
func NewImageWorkloadRepository(db *gorm.DB) *imageWorkloadRepository {
	return &imageWorkloadRepository{db: db}
}

func (r *imageWorkloadRepository) Create(ctx context.Context, workload *domain.ImageWorkload) error {
	return r.db.WithContext(ctx).Create(workload).Error
}

func (r *imageWorkloadRepository) Update(ctx context.Context, workload *domain.ImageWorkload) error {
	return r.db.WithContext(ctx).Save(workload).Error
}

func (r *imageWorkloadRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.ImageWorkload, error) {
	var workload domain.ImageWorkload
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&workload).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrWorkloadNotFound
		}
		return nil, err
	}
	return &workload, nil
}

func (r *imageWorkloadRepository) FindActive(ctx context.Context) ([]domain.ImageWorkload, error) {
	var workloads []domain.ImageWorkload
	err := r.db.WithContext(ctx).
		Where("state IN ?", []domain.ImageWorkloadState{
			domain.ImageWorkloadStateBooting,
			domain.ImageWorkloadStateRunning,
		}).
		Order("created_at ASC").
		Find(&workloads).Error
	return workloads, err
}

func (r *imageWorkloadRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.db.WithContext(ctx).Where("id = ?", id).Delete(&domain.ImageWorkload{}).Error
}
