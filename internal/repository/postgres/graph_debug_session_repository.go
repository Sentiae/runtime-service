package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

type graphDebugSessionRepository struct {
	db *gorm.DB
}

// NewGraphDebugSessionRepository creates a new PostgreSQL graph debug session repository
func NewGraphDebugSessionRepository(db *gorm.DB) *graphDebugSessionRepository {
	return &graphDebugSessionRepository{db: db}
}

func (r *graphDebugSessionRepository) Create(ctx context.Context, session *domain.GraphDebugSession) error {
	return r.db.WithContext(ctx).Create(session).Error
}

func (r *graphDebugSessionRepository) Update(ctx context.Context, session *domain.GraphDebugSession) error {
	return r.db.WithContext(ctx).Save(session).Error
}

func (r *graphDebugSessionRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.GraphDebugSession, error) {
	var session domain.GraphDebugSession
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&session).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrDebugSessionNotFound
		}
		return nil, err
	}
	return &session, nil
}

func (r *graphDebugSessionRepository) FindActiveByGraph(ctx context.Context, graphID uuid.UUID) (*domain.GraphDebugSession, error) {
	var session domain.GraphDebugSession
	err := r.db.WithContext(ctx).
		Where("graph_id = ? AND status IN ?", graphID, []string{
			string(domain.DebugStatusCreated),
			string(domain.DebugStatusRunning),
			string(domain.DebugStatusPaused),
		}).
		First(&session).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &session, nil
}
