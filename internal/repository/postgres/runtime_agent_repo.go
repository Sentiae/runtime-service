package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/sentiae/runtime-service/internal/domain"
)

// RuntimeAgentRepository implements usecase.AgentRepository on top of
// GORM/Postgres. Lookups are keyed by agent id (uuid); policy is
// one-per-org so the lookup is by org id.
type RuntimeAgentRepository struct {
	db *gorm.DB
}

func NewRuntimeAgentRepository(db *gorm.DB) *RuntimeAgentRepository {
	return &RuntimeAgentRepository{db: db}
}

func (r *RuntimeAgentRepository) Create(ctx context.Context, a *domain.RuntimeAgent) error {
	return r.db.WithContext(ctx).Create(a).Error
}

func (r *RuntimeAgentRepository) Update(ctx context.Context, a *domain.RuntimeAgent) error {
	return r.db.WithContext(ctx).Save(a).Error
}

func (r *RuntimeAgentRepository) Get(ctx context.Context, id uuid.UUID) (*domain.RuntimeAgent, error) {
	var a domain.RuntimeAgent
	err := r.db.WithContext(ctx).First(&a, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// GetPolicy returns the routing policy for an org or nil if none is
// configured. A nil policy means "always run locally" to the dispatcher.
func (r *RuntimeAgentRepository) GetPolicy(ctx context.Context, orgID uuid.UUID) (*domain.AgentRoutingPolicy, error) {
	var p domain.AgentRoutingPolicy
	err := r.db.WithContext(ctx).First(&p, "organization_id = ?", orgID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// UpsertPolicy writes or replaces the policy for an org. Idempotent so
// PATCHes from the admin UI don't leak duplicate rows.
func (r *RuntimeAgentRepository) UpsertPolicy(ctx context.Context, p *domain.AgentRoutingPolicy) error {
	return r.db.WithContext(ctx).Save(p).Error
}

// ListByOrg returns every agent the org has registered. Used by the
// admin UI; not on the dispatch hot path.
func (r *RuntimeAgentRepository) ListByOrg(ctx context.Context, orgID uuid.UUID) ([]domain.RuntimeAgent, error) {
	var rows []domain.RuntimeAgent
	err := r.db.WithContext(ctx).
		Where("organization_id = ?", orgID).
		Order("created_at ASC").
		Find(&rows).Error
	return rows, err
}
