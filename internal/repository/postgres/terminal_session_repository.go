package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

type terminalSessionRepository struct {
	db *gorm.DB
}

// NewTerminalSessionRepository creates a new PostgreSQL terminal session repository
func NewTerminalSessionRepository(db *gorm.DB) *terminalSessionRepository {
	return &terminalSessionRepository{db: db}
}

func (r *terminalSessionRepository) Create(ctx context.Context, session *domain.TerminalSession) error {
	return r.db.WithContext(ctx).Create(session).Error
}

func (r *terminalSessionRepository) Update(ctx context.Context, session *domain.TerminalSession) error {
	return r.db.WithContext(ctx).Save(session).Error
}

func (r *terminalSessionRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.TerminalSession, error) {
	var session domain.TerminalSession
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&session).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrTerminalSessionNotFound
		}
		return nil, err
	}
	return &session, nil
}

func (r *terminalSessionRepository) FindByUser(ctx context.Context, userID uuid.UUID) ([]domain.TerminalSession, error) {
	var sessions []domain.TerminalSession
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND status != ?", userID, domain.TerminalSessionStatusClosed).
		Order("created_at DESC").
		Find(&sessions).Error
	return sessions, err
}

func (r *terminalSessionRepository) FindActive(ctx context.Context) ([]domain.TerminalSession, error) {
	var sessions []domain.TerminalSession
	err := r.db.WithContext(ctx).
		Where("status = ?", domain.TerminalSessionStatusActive).
		Find(&sessions).Error
	return sessions, err
}

func (r *terminalSessionRepository) Delete(ctx context.Context, id uuid.UUID) error {
	result := r.db.WithContext(ctx).Where("id = ?", id).Delete(&domain.TerminalSession{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrTerminalSessionNotFound
	}
	return nil
}
