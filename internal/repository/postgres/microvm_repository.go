package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

type microVMRepository struct {
	db *gorm.DB
}

// NewMicroVMRepository creates a new PostgreSQL microVM repository
func NewMicroVMRepository(db *gorm.DB) *microVMRepository {
	return &microVMRepository{db: db}
}

func (r *microVMRepository) Create(ctx context.Context, vm *domain.MicroVM) error {
	return r.db.WithContext(ctx).Create(vm).Error
}

func (r *microVMRepository) Update(ctx context.Context, vm *domain.MicroVM) error {
	return r.db.WithContext(ctx).Save(vm).Error
}

func (r *microVMRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.MicroVM, error) {
	var vm domain.MicroVM
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&vm).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrVMNotFound
		}
		return nil, err
	}
	return &vm, nil
}

func (r *microVMRepository) FindAvailable(ctx context.Context, language domain.Language) (*domain.MicroVM, error) {
	var vm domain.MicroVM
	err := r.db.WithContext(ctx).
		Where("status = ? AND language = ? AND execution_id IS NULL", domain.VMStatusReady, language).
		Order("created_at ASC").
		First(&vm).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrVMPoolExhausted
		}
		return nil, err
	}
	return &vm, nil
}

func (r *microVMRepository) FindByExecution(ctx context.Context, executionID uuid.UUID) (*domain.MicroVM, error) {
	var vm domain.MicroVM
	err := r.db.WithContext(ctx).Where("execution_id = ?", executionID).First(&vm).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrVMNotFound
		}
		return nil, err
	}
	return &vm, nil
}

func (r *microVMRepository) FindActive(ctx context.Context) ([]domain.MicroVM, error) {
	var vms []domain.MicroVM
	err := r.db.WithContext(ctx).
		Where("status IN ?", []domain.VMStatus{domain.VMStatusReady, domain.VMStatusRunning, domain.VMStatusCreating}).
		Order("created_at ASC").
		Find(&vms).Error
	return vms, err
}

func (r *microVMRepository) CountByStatus(ctx context.Context, status domain.VMStatus) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&domain.MicroVM{}).Where("status = ?", status).Count(&count).Error
	return count, err
}
