package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

type vmInstanceRepository struct {
	db *gorm.DB
}

// NewVMInstanceRepository creates a new PostgreSQL VM instance repository
func NewVMInstanceRepository(db *gorm.DB) *vmInstanceRepository {
	return &vmInstanceRepository{db: db}
}

func (r *vmInstanceRepository) Create(ctx context.Context, instance *domain.VMInstance) error {
	return r.db.WithContext(ctx).Create(instance).Error
}

func (r *vmInstanceRepository) Update(ctx context.Context, instance *domain.VMInstance) error {
	return r.db.WithContext(ctx).Save(instance).Error
}

func (r *vmInstanceRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.VMInstance, error) {
	var instance domain.VMInstance
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&instance).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrVMInstanceNotFound
		}
		return nil, err
	}
	return &instance, nil
}

func (r *vmInstanceRepository) FindAll(ctx context.Context, statusFilter *domain.VMInstanceState) ([]domain.VMInstance, error) {
	var instances []domain.VMInstance
	query := r.db.WithContext(ctx)

	if statusFilter != nil {
		query = query.Where("state = ?", *statusFilter)
	}

	err := query.Order("created_at DESC").Find(&instances).Error
	return instances, err
}

func (r *vmInstanceRepository) FindNeedingReconciliation(ctx context.Context) ([]domain.VMInstance, error) {
	var instances []domain.VMInstance
	// Find instances where state != desired_state and state is not terminal
	// (unless desired_state is also terminal)
	err := r.db.WithContext(ctx).
		Where("state != desired_state AND state NOT IN ?",
			[]domain.VMInstanceState{domain.VMInstanceStateTerminated}).
		Order("created_at ASC").
		Find(&instances).Error
	return instances, err
}

func (r *vmInstanceRepository) FindByHost(ctx context.Context, hostID string) ([]domain.VMInstance, error) {
	var instances []domain.VMInstance
	err := r.db.WithContext(ctx).
		Where("host_id = ? AND state NOT IN ?", hostID,
			[]domain.VMInstanceState{domain.VMInstanceStateTerminated}).
		Order("created_at ASC").
		Find(&instances).Error
	return instances, err
}

func (r *vmInstanceRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.db.WithContext(ctx).Where("id = ?", id).Delete(&domain.VMInstance{}).Error
}
