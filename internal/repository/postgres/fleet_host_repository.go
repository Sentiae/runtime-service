package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"gorm.io/gorm"
)

// hostRepository is the GORM-backed HostRepository.
type hostRepository struct {
	db *gorm.DB
}

var _ repository.HostRepository = (*hostRepository)(nil)

// NewHostRepository creates a new PostgreSQL fleet-host repository.
func NewHostRepository(db *gorm.DB) *hostRepository {
	return &hostRepository{db: db}
}

func (r *hostRepository) Create(ctx context.Context, host *domain.Host) error {
	return r.db.WithContext(ctx).Create(host).Error
}

func (r *hostRepository) Update(ctx context.Context, host *domain.Host) error {
	return r.db.WithContext(ctx).Save(host).Error
}

func (r *hostRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.Host, error) {
	var host domain.Host
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&host).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrFleetHostNotFound
		}
		return nil, err
	}
	return &host, nil
}

func (r *hostRepository) ListActive(ctx context.Context) ([]domain.Host, error) {
	var hosts []domain.Host
	err := r.db.WithContext(ctx).
		Where("status = ?", domain.HostStatusActive).
		Order("created_at ASC").
		Find(&hosts).Error
	return hosts, err
}

func (r *hostRepository) ListByStatus(ctx context.Context, status domain.HostStatus) ([]domain.Host, error) {
	var hosts []domain.Host
	err := r.db.WithContext(ctx).
		Where("status = ?", status).
		Order("created_at ASC").
		Find(&hosts).Error
	return hosts, err
}

func (r *hostRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.db.WithContext(ctx).Where("id = ?", id).Delete(&domain.Host{}).Error
}
