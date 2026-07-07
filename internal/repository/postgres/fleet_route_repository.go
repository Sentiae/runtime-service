package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"gorm.io/gorm"
)

// routeRepository is the GORM-backed RouteRepository.
type routeRepository struct {
	db *gorm.DB
}

var _ repository.RouteRepository = (*routeRepository)(nil)

// NewRouteRepository creates a new PostgreSQL fleet-route repository.
func NewRouteRepository(db *gorm.DB) *routeRepository {
	return &routeRepository{db: db}
}

func (r *routeRepository) Create(ctx context.Context, route *domain.Route) error {
	return r.db.WithContext(ctx).Create(route).Error
}

func (r *routeRepository) ListByApp(ctx context.Context, appID uuid.UUID) ([]domain.Route, error) {
	var routes []domain.Route
	err := r.db.WithContext(ctx).
		Where("app_id = ?", appID).
		Order("created_at ASC").
		Find(&routes).Error
	return routes, err
}

func (r *routeRepository) DeleteByApp(ctx context.Context, appID uuid.UUID) error {
	return r.db.WithContext(ctx).Where("app_id = ?", appID).Delete(&domain.Route{}).Error
}
