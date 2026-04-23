package domain

import (
	"time"

	"github.com/google/uuid"
)

// HermeticBuild is a reproducible build over pinned inputs. §9.2
// gap-closure: pipelines previously ran inline with no formal
// input/output manifest, so a "green build" on Tuesday and a green build
// on Wednesday could diverge without notice. The hermetic-build record
// captures the pinned inputs (commit SHA, base image digest, tool
// versions) and the content-addressed outputs so reruns are verifiable.
//
// This is the domain skeleton only. Phase 4 of the gap-closure plan
// adds the full Bazel-style subsystem — content-addressable artifact
// store, incremental rebuilds, cross-pipeline sharing. The schema here
// is intentionally narrow so it can be extended without breaking
// existing rows.
type HermeticBuild struct {
	ID             uuid.UUID `json:"id" gorm:"type:uuid;primary_key"`
	OrganizationID uuid.UUID `json:"organization_id" gorm:"type:uuid;not null;index"`
	PipelineRunID  uuid.UUID `json:"pipeline_run_id" gorm:"type:uuid;not null;index"`

	// InputDigest is the stable hash over every pinned input — source
	// commit SHA, every dependency lockfile hash, base-image digest,
	// tool versions. Two builds with the same InputDigest MUST produce
	// the same OutputDigest, and the scheduler can short-circuit to
	// the cached outputs when it sees a hit.
	InputDigest string `json:"input_digest" gorm:"type:varchar(128);not null;index"`

	// OutputDigest is the hash of the produced artifact set. Nil until
	// the build succeeds.
	OutputDigest string `json:"output_digest,omitempty" gorm:"type:varchar(128);index"`

	// ArtifactRef points at the content-addressed store (future
	// `platform-kit/artifact-store`). Format is opaque today;
	// consumers read it through a resolver helper.
	ArtifactRef string `json:"artifact_ref,omitempty" gorm:"type:varchar(500)"`

	// Reproducible is true once a second build with the same
	// InputDigest has verified it produces the same OutputDigest. The
	// first-ever build sets Reproducible=false until the verification
	// pass runs.
	Reproducible bool `json:"reproducible" gorm:"not null;default:false"`

	StartedAt   time.Time  `json:"started_at" gorm:"not null"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	CreatedBy   uuid.UUID  `json:"created_by" gorm:"type:uuid;not null"`
}

// TableName pins the GORM table.
func (HermeticBuild) TableName() string { return "hermetic_builds" }

// HermeticBuildStep describes one ordered step in a chained hermetic
// build (§9.2.2). Each step runs in a FRESH microVM whose workspace is
// seeded with the previous step's output artifact — no shared state,
// no ambient env. Order is explicit via the Index field so the caller
// can reorder without reconstructing the slice.
//
// Input/Output paths are relative to the step workspace. InputArtifact
// is empty for the first step; the build driver wires the previous
// step's OutputArtifact in on every subsequent iteration.
type HermeticBuildStep struct {
	ID            uuid.UUID `json:"id" gorm:"type:uuid;primary_key"`
	BuildID       uuid.UUID `json:"build_id" gorm:"type:uuid;not null;index"`
	Index         int       `json:"index" gorm:"not null"`
	Name          string    `json:"name" gorm:"type:varchar(200);not null"`
	Language      string    `json:"language" gorm:"type:varchar(40);not null"`
	Command       string    `json:"command" gorm:"type:text;not null"`
	InputArtifact string    `json:"input_artifact,omitempty" gorm:"type:varchar(500)"`
	// OutputArtifact is the digest produced once the step succeeds. Set
	// by the build driver, read by the next step as its InputArtifact.
	OutputArtifact string `json:"output_artifact,omitempty" gorm:"type:varchar(500)"`
	// EnvJSON pins the guest environment. Stored as a JSON-encoded
	// map[string]string so the repo layer stays dialect-free.
	EnvJSON string `json:"env_json,omitempty" gorm:"type:jsonb"`
	// TimeoutSec caps per-step wall clock. 0 → runner default.
	TimeoutSec int        `json:"timeout_sec" gorm:"not null;default:0"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Stdout     string     `json:"stdout,omitempty" gorm:"type:text"`
	Stderr     string     `json:"stderr,omitempty" gorm:"type:text"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// TableName for the chained step rows.
func (HermeticBuildStep) TableName() string { return "hermetic_build_steps" }
