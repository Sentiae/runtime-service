package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/timetravel"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

// HermeticBuildRepo is the exported alias used by usecases.
type HermeticBuildRepo = hermeticBuildRepository

type hermeticBuildRepository struct {
	db       *gorm.DB
	recorder timetravel.Recorder
}

// NewHermeticBuildRepository builds the repo. Keeping the constructor
// name consistent with TestRunRepository for pattern parity.
func NewHermeticBuildRepository(db *gorm.DB) *hermeticBuildRepository {
	return &hermeticBuildRepository{db: db}
}

// WithRecorder enables time-travel snapshots for every HermeticBuild
// write. CS11 slice (2026-04-18).
func (r *hermeticBuildRepository) WithRecorder(rec timetravel.Recorder) *hermeticBuildRepository {
	r.recorder = rec
	return r
}

func (r *hermeticBuildRepository) recordSnapshot(ctx context.Context, b *domain.HermeticBuild) {
	if r == nil || r.recorder == nil || b == nil {
		return
	}
	_ = r.recorder.RecordEntity(ctx, "hermetic_build", b.ID.String(), b)
}

// Create persists a new HermeticBuild row.
func (r *hermeticBuildRepository) Create(ctx context.Context, b *domain.HermeticBuild) error {
	if err := r.db.WithContext(ctx).Create(b).Error; err != nil {
		return err
	}
	r.recordSnapshot(ctx, b)
	return nil
}

// Update persists status + digest changes on an existing row.
func (r *hermeticBuildRepository) Update(ctx context.Context, b *domain.HermeticBuild) error {
	if err := r.db.WithContext(ctx).Save(b).Error; err != nil {
		return err
	}
	r.recordSnapshot(ctx, b)
	return nil
}

// FindByID looks a build up by primary key. Missing rows return (nil, nil)
// rather than gorm.ErrRecordNotFound so callers can decide their own
// policy for absent builds.
func (r *hermeticBuildRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.HermeticBuild, error) {
	var b domain.HermeticBuild
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&b).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}

// FindByInputDigest returns the most recent successful build with the
// given input digest so the caller can short-circuit to cached outputs.
// Returns (nil, nil) when no hit exists.
func (r *hermeticBuildRepository) FindByInputDigest(ctx context.Context, orgID uuid.UUID, inputDigest string) (*domain.HermeticBuild, error) {
	var b domain.HermeticBuild
	err := r.db.WithContext(ctx).
		Where("organization_id = ? AND input_digest = ? AND output_digest <> ''", orgID, inputDigest).
		Order("created_at DESC").
		First(&b).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}
