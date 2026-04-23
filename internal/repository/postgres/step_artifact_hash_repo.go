package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/timetravel"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

// StepArtifactHashRepo is the exported alias used by usecases.
type StepArtifactHashRepo = stepArtifactHashRepository

type stepArtifactHashRepository struct {
	db       *gorm.DB
	recorder timetravel.Recorder
}

// NewStepArtifactHashRepository builds the repo.
func NewStepArtifactHashRepository(db *gorm.DB) *stepArtifactHashRepository {
	return &stepArtifactHashRepository{db: db}
}

// WithRecorder enables time-travel snapshots on every hash-row write.
// CS11 slice (2026-04-18).
func (r *stepArtifactHashRepository) WithRecorder(rec timetravel.Recorder) *stepArtifactHashRepository {
	r.recorder = rec
	return r
}

// Create persists a new step-artifact-hash row.
func (r *stepArtifactHashRepository) Create(ctx context.Context, h *domain.StepArtifactHash) error {
	if h.ID == uuid.Nil {
		h.ID = uuid.New()
	}
	if h.CreatedAt.IsZero() {
		h.CreatedAt = time.Now().UTC()
	}
	if err := r.db.WithContext(ctx).Create(h).Error; err != nil {
		return err
	}
	if r.recorder != nil {
		_ = r.recorder.RecordEntity(ctx, "hermetic_build_step", h.ID.String(), h)
	}
	return nil
}

// GetByBuildAndStep returns the declared digest for the given
// (build_id, step_index). Missing rows return (nil, nil) — callers
// use that to decide whether there's a predecessor to verify.
func (r *stepArtifactHashRepository) GetByBuildAndStep(ctx context.Context, buildID uuid.UUID, stepIndex int) (*domain.StepArtifactHash, error) {
	var h domain.StepArtifactHash
	err := r.db.WithContext(ctx).
		Where("build_id = ? AND step_index = ?", buildID, stepIndex).
		First(&h).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &h, nil
}
