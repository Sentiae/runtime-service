package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"gorm.io/gorm"
)

// fleetAppRepository is the GORM-backed FleetAppRepository.
type fleetAppRepository struct {
	db *gorm.DB
}

var _ repository.FleetAppRepository = (*fleetAppRepository)(nil)

// NewFleetAppRepository creates a new PostgreSQL fleet-app repository.
func NewFleetAppRepository(db *gorm.DB) *fleetAppRepository {
	return &fleetAppRepository{db: db}
}

func (r *fleetAppRepository) Create(ctx context.Context, app *domain.FleetApp) error {
	return r.db.WithContext(ctx).Create(app).Error
}

func (r *fleetAppRepository) Update(ctx context.Context, app *domain.FleetApp) error {
	return r.db.WithContext(ctx).Save(app).Error
}

func (r *fleetAppRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.FleetApp, error) {
	var app domain.FleetApp
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&app).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrFleetAppNotFound
		}
		return nil, err
	}
	return &app, nil
}

func (r *fleetAppRepository) FindByComponentEnv(ctx context.Context, componentID, env string) (*domain.FleetApp, error) {
	var app domain.FleetApp
	err := r.db.WithContext(ctx).
		Where("component_id = ? AND env = ?", componentID, env).
		First(&app).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrFleetAppNotFound
		}
		return nil, err
	}
	return &app, nil
}

func (r *fleetAppRepository) List(ctx context.Context) ([]domain.FleetApp, error) {
	var apps []domain.FleetApp
	if err := r.db.WithContext(ctx).Find(&apps).Error; err != nil {
		return nil, err
	}
	return apps, nil
}

func (r *fleetAppRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.db.WithContext(ctx).Where("id = ?", id).Delete(&domain.FleetApp{}).Error
}
