package domain

import (
	"time"

	"github.com/google/uuid"
)

// StepArtifactHash captures the declared digest of one hermetic-build
// step's output artifact (§9.2). Every step row is verified against its
// predecessor on next-step start: the runner rehydrates the prior
// digest from the store and asserts the bytes still hash to the
// declared value. A mismatch halts the build with ErrHashMismatch.
//
// Unique on (build_id, step_index) so the chain has exactly one
// authoritative digest per ordered step.
type StepArtifactHash struct {
	ID          uuid.UUID `json:"id" gorm:"type:uuid;primary_key"`
	BuildID     uuid.UUID `json:"build_id" gorm:"type:uuid;not null;index;uniqueIndex:idx_step_hash_build_step,priority:1"`
	StepIndex   int       `json:"step_index" gorm:"not null;uniqueIndex:idx_step_hash_build_step,priority:2"`
	Digest      string    `json:"digest" gorm:"type:varchar(128);not null"`
	ArtifactRef string    `json:"artifact_ref,omitempty" gorm:"type:varchar(500)"`
	CreatedAt   time.Time `json:"created_at" gorm:"not null"`
}

// TableName pins the GORM table.
func (StepArtifactHash) TableName() string { return "step_artifact_hashes" }
