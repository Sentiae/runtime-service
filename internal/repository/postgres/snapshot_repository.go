package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

type snapshotRepository struct {
	db *gorm.DB
}

// NewSnapshotRepository creates a new PostgreSQL snapshot repository
func NewSnapshotRepository(db *gorm.DB) *snapshotRepository {
	return &snapshotRepository{db: db}
}

func (r *snapshotRepository) Create(ctx context.Context, snapshot *domain.Snapshot) error {
	return r.db.WithContext(ctx).Create(snapshot).Error
}

func (r *snapshotRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.Snapshot, error) {
	var snapshot domain.Snapshot
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&snapshot).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrSnapshotNotFound
		}
		return nil, err
	}
	return &snapshot, nil
}

func (r *snapshotRepository) FindBaseByLanguage(ctx context.Context, language domain.Language) (*domain.Snapshot, error) {
	var snapshot domain.Snapshot
	err := r.db.WithContext(ctx).
		Where("is_base_image = ? AND language = ?", true, language).
		Order("created_at DESC").
		First(&snapshot).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrSnapshotNotFound
		}
		return nil, err
	}
	return &snapshot, nil
}

func (r *snapshotRepository) FindByExecution(ctx context.Context, executionID uuid.UUID) ([]domain.Snapshot, error) {
	var snapshots []domain.Snapshot
	err := r.db.WithContext(ctx).
		Where("execution_id = ?", executionID).
		Order("created_at DESC").
		Find(&snapshots).Error
	return snapshots, err
}

func (r *snapshotRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.db.WithContext(ctx).Where("id = ?", id).Delete(&domain.Snapshot{}).Error
}
