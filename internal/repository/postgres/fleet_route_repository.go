package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
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

// routeHostUniqueIndex is the unique index migrations/0006 puts on
// fleet_routes.host_pattern — the fleet owns ingress (D-079), so one host maps to
// exactly one app.
const routeHostUniqueIndex = "fleet_routes_host_pattern_key"

// Create inserts the route, translating a host collision into
// domain.ErrIngressHostTaken so the caller sees a named conflict instead of the
// raw driver error (which fell through to codes.Internal "internal server error").
// The conflicting host is carried in the message for the operator log.
func (r *routeRepository) Create(ctx context.Context, route *domain.Route) error {
	err := r.db.WithContext(ctx).Create(route).Error
	if isRouteHostConflict(err) {
		return fmt.Errorf("%w: %s", domain.ErrIngressHostTaken, route.HostPattern)
	}
	return err
}

// isRouteHostConflict reports whether err is the unique violation on the
// host_pattern index specifically. Matched on SQLSTATE + constraint name via the
// pgconn error (never on message text, §30.5/§16.5) — the same driver-level
// inspection imageWorkloadRepository.IsDuplicateKey uses, because this driver is
// not configured with gorm's TranslateError. Naming the constraint keeps an
// unrelated 23505 (e.g. a primary-key collision) out of this translation.
func isRouteHostConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == pgUniqueViolation &&
		pgErr.ConstraintName == routeHostUniqueIndex
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

func (r *routeRepository) FindByHost(ctx context.Context, host string) (*domain.Route, error) {
	var route domain.Route
	err := r.db.WithContext(ctx).
		Where("host_pattern = ? OR custom_domain = ?", host, host).
		First(&route).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrRouteNotFound
		}
		return nil, err
	}
	return &route, nil
}
