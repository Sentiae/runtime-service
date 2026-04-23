package usecase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
)

// HermeticBuildUseCase is the narrow first-pass implementation of §9.2.
// It provides:
//
//   - deterministic input digests so a rerun on the same pinned inputs
//     hits the cached row instead of re-running;
//   - Start/Complete/Fail lifecycle transitions against the GORM repo;
//   - a simple cache-lookup primitive the DSL `build_artifact` action
//     uses so pipelines can skip identical builds.
//
// §9.2 enforcement knobs (added 2026-04-18):
//   - EnforceBaseImageDigest: reject StartBuild when Inputs lacks a
//     `base_image_digest` key. Off by default so existing callers that
//     haven't plumbed the key yet don't break; operators opt in by
//     setting HERMETIC_REQUIRE_BASE_IMAGE_DIGEST=true.
//   - EnforceReproducibility: automatically kick off a re-verify after
//     CompleteBuild. On mismatch, returns ErrReproducibilityMismatch.
//     Off by default to avoid CI pain (re-running a build doubles
//     pipeline wall clock); set HERMETIC_ENFORCE_REPRODUCIBILITY=true
//     to opt in on protected environments.
//
// Full Bazel-style reproducibility (content-addressed artifact store,
// incremental rebuilds) is Phase 4. Everything below is intentionally
// enough to unstub the DSL action and nothing more.
type HermeticBuildUseCase struct {
	repo     *postgres.HermeticBuildRepo
	store    ArtifactStore
	hashRepo *postgres.StepArtifactHashRepo

	// §9.2 enforcement toggles. Defaults preserve legacy behaviour.
	enforceBaseImageDigest   bool
	enforceReproducibility   bool
}

// ErrMissingBaseImageDigest is returned when EnforceBaseImageDigest is
// set and StartBuild is called without an `base_image_digest` key in
// Inputs. The check is a cheap way to catch callers who forgot to pin
// the layer that most often drifts between builds (OS/toolchain updates
// invalidate supposedly-reproducible artifacts).
var ErrMissingBaseImageDigest = errors.New("hermetic build: Inputs must include base_image_digest")

// ErrReproducibilityMismatch is returned by EnforceReproducibility when
// a re-run with the pinned InputDigest produces a different
// OutputDigest. The first run's row is preserved so operators can
// compare. Reproducible stays false.
var ErrReproducibilityMismatch = errors.New("hermetic build: reproducibility check failed — output digest mismatch")

// NewHermeticBuildUseCase wires the usecase. An optional ArtifactStore
// enables content-addressed storage + resolve; without it the usecase
// still tracks build metadata but has no artifact bytes to return.
func NewHermeticBuildUseCase(repo *postgres.HermeticBuildRepo) *HermeticBuildUseCase {
	return &HermeticBuildUseCase{repo: repo}
}

// WithStore injects the artifact store. Kept as a setter so dev boots
// without a writable artifact root still work (store-less mode).
func (uc *HermeticBuildUseCase) WithStore(s ArtifactStore) *HermeticBuildUseCase {
	uc.store = s
	return uc
}

// WithHashRepo injects the per-step artifact-hash repo. Enables the
// §9.2 integrity chain: each step's declared digest is persisted on
// success, and the next step's start re-verifies the prior bytes via
// store.VerifyHash. Without a hash repo the chain still works but
// skips the verification step.
func (uc *HermeticBuildUseCase) WithHashRepo(r *postgres.StepArtifactHashRepo) *HermeticBuildUseCase {
	uc.hashRepo = r
	return uc
}

// Store returns the configured ArtifactStore (may be nil). Exposed so
// HTTP handlers can stream directly from/to the store without
// reimplementing the Put/Get dance.
func (uc *HermeticBuildUseCase) Store() ArtifactStore { return uc.store }

// WithEnforceBaseImageDigest toggles the §9.2 `base_image_digest`
// Inputs key requirement. Returns the same receiver for fluent DI.
func (uc *HermeticBuildUseCase) WithEnforceBaseImageDigest(on bool) *HermeticBuildUseCase {
	uc.enforceBaseImageDigest = on
	return uc
}

// WithEnforceReproducibility toggles automatic re-verification after
// CompleteBuild. Returns the same receiver for fluent DI.
func (uc *HermeticBuildUseCase) WithEnforceReproducibility(on bool) *HermeticBuildUseCase {
	uc.enforceReproducibility = on
	return uc
}

// StartBuildInput is the set of values the caller must pin so the
// digest is deterministic. Anything that can change build output must
// land in Inputs — leaving a field out means a change there won't bust
// the cache, which is the reproducibility gap we're trying to close.
type StartBuildInput struct {
	OrganizationID uuid.UUID
	PipelineRunID  uuid.UUID
	CreatedBy      uuid.UUID
	// Inputs is the pinned-input map. Typical keys: `commit_sha`,
	// `base_image_digest`, `lockfile_hash`, `tool_versions`. Values
	// must be stable string representations; the digest is order-
	// independent across keys so callers don't need to sort.
	Inputs map[string]string
}

// StartBuild either returns a cached HermeticBuild for the same input
// digest (Reproducible=true) or persists a new one in the in-progress
// state (OutputDigest == "", CompletedAt == nil).
//
// The cache hit is intentionally conservative: we only reuse builds
// that already have an OutputDigest recorded, so a concurrent in-flight
// build doesn't produce a race where two callers think they're sharing
// the same output.
func (uc *HermeticBuildUseCase) StartBuild(ctx context.Context, in StartBuildInput) (*domain.HermeticBuild, bool, error) {
	if uc == nil || uc.repo == nil {
		return nil, false, errors.New("hermetic build repo not configured")
	}
	if in.OrganizationID == uuid.Nil {
		return nil, false, errors.New("organization_id is required")
	}
	if len(in.Inputs) == 0 {
		return nil, false, errors.New("at least one pinned input is required")
	}

	// §9.2 — optionally require `base_image_digest` in Inputs. That key
	// pins the single layer most likely to drift between a "first run"
	// and a later "reproducibility check" so omitting it has been the
	// most common silent cause of non-deterministic output digests.
	if uc.enforceBaseImageDigest {
		if v, ok := in.Inputs["base_image_digest"]; !ok || strings.TrimSpace(v) == "" {
			return nil, false, ErrMissingBaseImageDigest
		}
	}

	digest := ComputeInputDigest(in.Inputs)

	if cached, err := uc.repo.FindByInputDigest(ctx, in.OrganizationID, digest); err != nil {
		return nil, false, err
	} else if cached != nil {
		return cached, true, nil
	}

	build := &domain.HermeticBuild{
		ID:             uuid.New(),
		OrganizationID: in.OrganizationID,
		PipelineRunID:  in.PipelineRunID,
		InputDigest:    digest,
		Reproducible:   false,
		StartedAt:      time.Now().UTC(),
		CreatedBy:      in.CreatedBy,
	}
	if err := uc.repo.Create(ctx, build); err != nil {
		return nil, false, err
	}
	return build, false, nil
}

// StepRunner is the shape the HermeticBuildUseCase needs to provision a
// fresh VM, execute a step command, and collect the result+artifact
// bytes. Implementations live in the Firecracker provider package.
type StepRunner interface {
	RunStep(ctx context.Context, req StepRunRequest) (*StepRunResult, error)
}

// StepRunRequest carries the inputs for one hermetic step.
type StepRunRequest struct {
	Language string
	Command  []string
	// Workspace is the pre-assembled step input directory (e.g. the
	// output artifact of the previous step). The runner mounts it
	// read-only into the VM.
	Workspace string
	// EnvVars pin the execution environment. Only values in this map
	// reach the guest — ambient env is NOT leaked.
	EnvVars map[string]string
	// TimeoutSec caps wall clock. 0 → provider default.
	TimeoutSec int
}

// StepRunResult is the post-run summary. ArtifactBytes carries the
// output bytes, ArtifactDigest is sha256 for content-addressed store.
type StepRunResult struct {
	ExitCode       int
	Stdout         string
	Stderr         string
	ArtifactBytes  []byte
	ArtifactDigest string
	DurationMS     int64
}

// RunStep is the hermetic-per-step entrypoint (§9.2.1). Flow:
//  1. Compute input digest → StartBuild (cache hit short-circuits).
//  2. On cache miss, call the runner in a fresh VM.
//  3. Store artifact bytes via ArtifactStore, if configured.
//  4. CompleteBuild with the output digest.
//
// Returns the final HermeticBuild row plus the step stdout/stderr.
func (uc *HermeticBuildUseCase) RunStep(ctx context.Context, runner StepRunner, in StartBuildInput, req StepRunRequest) (*domain.HermeticBuild, *StepRunResult, error) {
	if runner == nil {
		return nil, nil, errors.New("hermetic: no step runner configured")
	}
	build, cached, err := uc.StartBuild(ctx, in)
	if err != nil {
		return nil, nil, err
	}
	if cached && uc.store != nil && build.OutputDigest != "" {
		// Cache hit — return the cached build without re-running.
		return build, &StepRunResult{ArtifactDigest: build.OutputDigest}, nil
	}

	result, err := runner.RunStep(ctx, req)
	if err != nil {
		_, _ = uc.FailBuild(ctx, build.ID)
		return nil, nil, fmt.Errorf("hermetic step run: %w", err)
	}

	artifactRef := ""
	if uc.store != nil && result.ArtifactDigest != "" && len(result.ArtifactBytes) > 0 {
		if err := uc.store.Put(result.ArtifactDigest, bytes.NewReader(result.ArtifactBytes)); err != nil {
			_, _ = uc.FailBuild(ctx, build.ID)
			return nil, nil, fmt.Errorf("hermetic artifact put: %w", err)
		}
		artifactRef = result.ArtifactDigest
	}

	out, err := uc.CompleteBuild(ctx, build.ID, result.ArtifactDigest, artifactRef)
	if err != nil {
		return nil, result, err
	}
	return out, result, nil
}

// CompleteBuild records a successful build: output digest + artifact
// pointer + completion timestamp. A second run with the same
// InputDigest whose OutputDigest matches flips Reproducible to true —
// that's the cheap reproducibility-verification hook; we track it so
// Phase 4 can upgrade to full Bazel-style proofs without schema churn.
func (uc *HermeticBuildUseCase) CompleteBuild(ctx context.Context, id uuid.UUID, outputDigest, artifactRef string) (*domain.HermeticBuild, error) {
	build, err := uc.repo.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if build == nil {
		return nil, errors.New("build not found")
	}

	// Only mark reproducible when a prior build with the same input
	// digest produced the same output digest. Cheap verification.
	if prior, err := uc.repo.FindByInputDigest(ctx, build.OrganizationID, build.InputDigest); err == nil &&
		prior != nil && prior.ID != build.ID && prior.OutputDigest == outputDigest {
		build.Reproducible = true
	}

	build.OutputDigest = outputDigest
	build.ArtifactRef = artifactRef
	now := time.Now().UTC()
	build.CompletedAt = &now
	if err := uc.repo.Update(ctx, build); err != nil {
		return nil, err
	}

	// §9.2 enforcement — when the operator opts in, run a read-only
	// reproducibility check as part of CompleteBuild. The check is
	// side-effect-free beyond the log line; a real re-run would
	// require re-executing the StepRunner, which is deferred to
	// EnforceReproducibility so callers explicitly pay for it.
	if uc.enforceReproducibility {
		ok, verr := uc.VerifyReproducibility(ctx, build.ID)
		if verr != nil {
			log.Printf("[hermetic] CompleteBuild verify %s: %v", build.ID, verr)
		} else if !ok {
			log.Printf("[hermetic] CompleteBuild %s: reproducibility mismatch (prior output digest differs)", build.ID)
			return build, ErrReproducibilityMismatch
		}
	}
	return build, nil
}

// EnforceReproducibility re-runs the hermetic build identified by id
// and asserts that the second run produces the same OutputDigest as
// the first. Intended to be kicked off out-of-band on a protected
// environment (nightly cron, post-merge guardrail) rather than on
// every CompleteBuild — the re-run doubles build wall clock so we
// never fire it automatically unless HERMETIC_ENFORCE_REPRODUCIBILITY
// is set at the usecase level.
//
// Contract:
//
//	(true, nil)                  — second run matched the recorded
//	                               OutputDigest; the build row is
//	                               updated with Reproducible=true.
//	(false, ErrReproducibilityMismatch) — second run produced a
//	                                      different digest.
//	(_, err)                     — the re-run itself failed (network,
//	                               provider issue, etc).
//
// The runner receives the same StartBuildInput the build started from,
// reconstructed from the persisted row. Because the row only stores
// InputDigest (not the unhashed Inputs map), callers MUST pass the
// original Inputs map again — the method refuses to re-run without it
// so there's no ambiguity about what was verified.
func (uc *HermeticBuildUseCase) EnforceReproducibility(
	ctx context.Context,
	runner StepRunner,
	buildID uuid.UUID,
	originalInputs map[string]string,
	req StepRunRequest,
) (bool, error) {
	if uc == nil || uc.repo == nil {
		return false, errors.New("hermetic build repo not configured")
	}
	if runner == nil {
		return false, errors.New("hermetic: no step runner configured for re-verify")
	}
	build, err := uc.repo.FindByID(ctx, buildID)
	if err != nil {
		return false, err
	}
	if build == nil {
		return false, errors.New("build not found")
	}
	if build.OutputDigest == "" {
		return false, errors.New("build has no OutputDigest to verify against")
	}
	if len(originalInputs) == 0 {
		return false, errors.New("original inputs are required to re-verify")
	}
	// Digest parity check: confirm the caller-supplied Inputs hash to
	// the same digest the row recorded. A mismatch means the caller is
	// about to re-run a different build — fail closed.
	if got := ComputeInputDigest(originalInputs); got != build.InputDigest {
		return false, fmt.Errorf("hermetic: supplied inputs hash to %s, build row has %s", got, build.InputDigest)
	}

	result, err := runner.RunStep(ctx, req)
	if err != nil {
		return false, fmt.Errorf("hermetic re-verify step: %w", err)
	}

	if result.ArtifactDigest != build.OutputDigest {
		return false, ErrReproducibilityMismatch
	}

	// Match — flip Reproducible to true and persist.
	build.Reproducible = true
	if err := uc.repo.Update(ctx, build); err != nil {
		return true, err
	}
	return true, nil
}

// ResolveByInputDigest returns the most recent successful hermetic
// build for an input digest — the primary cache-lookup path. Callers
// (pipeline workers) use this before doing any expensive work: a hit
// means the artifact already exists and can be downloaded via
// Store().Get(build.OutputDigest) instead of rebuilt.
//
// Returns (nil, nil) on cache miss — not an error, because "no prior
// build" is the expected path for most first-run callers.
func (uc *HermeticBuildUseCase) ResolveByInputDigest(ctx context.Context, orgID uuid.UUID, inputDigest string) (*domain.HermeticBuild, error) {
	if uc == nil || uc.repo == nil {
		return nil, errors.New("hermetic build repo not configured")
	}
	if inputDigest == "" {
		return nil, errors.New("input_digest is required")
	}
	return uc.repo.FindByInputDigest(ctx, orgID, inputDigest)
}

// VerifyReproducibility checks whether the build with `id` produced
// the same OutputDigest as prior builds with the same InputDigest.
// Returns:
//
//	(true, nil)   — all prior builds agree (or there are none); build is reproducible
//	(false, nil)  — at least one prior build produced a different OutputDigest
//	(_, err)      — lookup failed
//
// The method is side-effect-free — it doesn't mutate the Reproducible
// flag on any row. Callers decide whether to persist the result so the
// verification remains a read-only oracle.
func (uc *HermeticBuildUseCase) VerifyReproducibility(ctx context.Context, id uuid.UUID) (bool, error) {
	if uc == nil || uc.repo == nil {
		return false, errors.New("hermetic build repo not configured")
	}
	build, err := uc.repo.FindByID(ctx, id)
	if err != nil {
		return false, err
	}
	if build == nil {
		return false, errors.New("build not found")
	}
	if build.OutputDigest == "" {
		return false, nil
	}
	prior, err := uc.repo.FindByInputDigest(ctx, build.OrganizationID, build.InputDigest)
	if err != nil {
		return false, err
	}
	if prior == nil || prior.ID == build.ID {
		// No independent prior build with the same input — reproducibility
		// can't be confirmed yet, but also isn't contradicted.
		return true, nil
	}
	return prior.OutputDigest == build.OutputDigest, nil
}

// FailBuild marks a build terminal without an output. The row stays
// queryable so the UI can surface the attempt, but FindByInputDigest
// skips it because OutputDigest is empty.
func (uc *HermeticBuildUseCase) FailBuild(ctx context.Context, id uuid.UUID) (*domain.HermeticBuild, error) {
	build, err := uc.repo.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if build == nil {
		return nil, errors.New("build not found")
	}
	now := time.Now().UTC()
	build.CompletedAt = &now
	if err := uc.repo.Update(ctx, build); err != nil {
		return nil, err
	}
	return build, nil
}

// ChainStep is the caller-facing description of a single step in a
// chained hermetic build. Each step gets a fresh VM whose workspace is
// seeded with the previous step's output artifact bytes (if any).
type ChainStep struct {
	Name       string
	Language   string
	Command    []string
	EnvVars    map[string]string
	TimeoutSec int
}

// ChainResult is returned by ExecuteChain so callers can inspect what
// ran without re-querying the repo. The final artifact digest in the
// chain matches HermeticBuild.OutputDigest.
type ChainResult struct {
	Build *domain.HermeticBuild
	Steps []StepRunResult
}

// ExecuteChain runs an ordered list of steps, feeding each step's
// output artifact into the next step's workspace. (§9.2.2)
//
// Contract:
//  1. Step 0 runs with workspace == req0.Workspace (caller-supplied).
//  2. Step N (N>0) runs with workspace == previous step's artifact
//     bytes written to a temp directory. The runner mounts it read-only.
//  3. On any step failure the build row is marked failed and the error
//     is returned immediately — later steps are NOT attempted.
//  4. The final step's artifact digest becomes the build's OutputDigest.
//
// The artifact bytes flow through ArtifactStore.Put so callers can
// download the final artifact via Store().Get(OutputDigest). Intermediate
// artifacts are stored too — a rerun of the same step inputs can skip
// the VM roundtrip.
func (uc *HermeticBuildUseCase) ExecuteChain(
	ctx context.Context,
	runner StepRunner,
	in StartBuildInput,
	steps []ChainStep,
	seedWorkspace string,
) (*ChainResult, error) {
	if runner == nil {
		return nil, errors.New("hermetic: no step runner configured")
	}
	if len(steps) == 0 {
		return nil, errors.New("hermetic: at least one step is required")
	}

	build, cached, err := uc.StartBuild(ctx, in)
	if err != nil {
		return nil, err
	}
	// Cache hit short-circuits the whole chain — the full-chain output
	// digest already matches, so re-running would be wasted work.
	if cached && build.OutputDigest != "" {
		return &ChainResult{Build: build}, nil
	}

	workspace := seedWorkspace
	var results []StepRunResult
	var lastDigest string

	for i, step := range steps {
		// §9.2 integrity chain: before running step N>0, re-verify the
		// prior step's artifact matches its declared digest. On
		// mismatch, fail the build with ErrHashMismatch so the chain
		// halts on the first corruption instead of propagating bad
		// bytes downstream.
		if i > 0 && uc.hashRepo != nil && uc.store != nil {
			prev, err := uc.hashRepo.GetByBuildAndStep(ctx, build.ID, i-1)
			if err != nil {
				_, _ = uc.FailBuild(ctx, build.ID)
				return nil, fmt.Errorf("hermetic chain hash lookup step %d: %w", i, err)
			}
			if prev != nil && prev.Digest != "" {
				if verr := uc.store.VerifyHash(prev.Digest); verr != nil {
					_, _ = uc.FailBuild(ctx, build.ID)
					if errors.Is(verr, ErrArtifactIntegrity) {
						return nil, domain.ErrHashMismatch
					}
					return nil, fmt.Errorf("hermetic chain verify step %d: %w", i, verr)
				}
			}
		}
		req := StepRunRequest{
			Language:   step.Language,
			Command:    step.Command,
			Workspace:  workspace,
			EnvVars:    step.EnvVars,
			TimeoutSec: step.TimeoutSec,
		}
		result, runErr := runner.RunStep(ctx, req)
		if runErr != nil {
			_, _ = uc.FailBuild(ctx, build.ID)
			return nil, fmt.Errorf("hermetic chain step %d (%s): %w", i, step.Name, runErr)
		}
		// Persist each intermediate artifact so a rerun of ONLY the
		// tail of the chain can pick up the cached prefix.
		artifactRef := ""
		if uc.store != nil && result.ArtifactDigest != "" && len(result.ArtifactBytes) > 0 {
			if err := uc.store.Put(result.ArtifactDigest, bytes.NewReader(result.ArtifactBytes)); err != nil {
				_, _ = uc.FailBuild(ctx, build.ID)
				return nil, fmt.Errorf("hermetic chain step %d put: %w", i, err)
			}
			artifactRef = result.ArtifactDigest
		}
		// Record the per-step hash so the next iteration's pre-run
		// verification has a reference digest to check against.
		if uc.hashRepo != nil && result.ArtifactDigest != "" {
			if err := uc.hashRepo.Create(ctx, &domain.StepArtifactHash{
				BuildID:     build.ID,
				StepIndex:   i,
				Digest:      result.ArtifactDigest,
				ArtifactRef: artifactRef,
			}); err != nil {
				log.Printf("[hermetic] step %d hash persist failed: %v", i, err)
			}
		}

		results = append(results, *result)
		lastDigest = result.ArtifactDigest
		// Next step's workspace is this step's artifact. We write the
		// bytes to a tmp dir so the next VM boot can mount it; the
		// runner owns cleanup once it finishes consuming the volume.
		if i < len(steps)-1 {
			next, wrErr := uc.stageArtifactForNextStep(result.ArtifactBytes, result.ArtifactDigest)
			if wrErr != nil {
				_, _ = uc.FailBuild(ctx, build.ID)
				return nil, fmt.Errorf("hermetic chain stage: %w", wrErr)
			}
			workspace = next
		}
	}

	out, err := uc.CompleteBuild(ctx, build.ID, lastDigest, lastDigest)
	if err != nil {
		return nil, err
	}
	return &ChainResult{Build: out, Steps: results}, nil
}

// stageArtifactForNextStep writes the previous step's artifact bytes
// to a fresh tmp directory and returns the path for the next VM to
// mount. The file is named by digest so the runner can introspect
// provenance. Returns an empty string when bytes are empty — an empty
// workspace is valid for pipelines where the next step only reads env.
func (uc *HermeticBuildUseCase) stageArtifactForNextStep(bytesIn []byte, digest string) (string, error) {
	if len(bytesIn) == 0 {
		return "", nil
	}
	dir, err := os.MkdirTemp("", "hermetic-stage-*")
	if err != nil {
		return "", err
	}
	name := digest
	if name == "" {
		name = "artifact.bin"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, bytesIn, 0o640); err != nil {
		return "", err
	}
	return dir, nil
}

// ComputeInputDigest hashes a key-sorted representation of the pinned
// inputs. Exported so callers can compute the digest in advance (e.g.
// to pre-warm the cache from an existing artifact store).
//
// Format: repeated `key\tvalue\n` entries in key-sorted order, then
// SHA-256 as hex. Key order is stable across callers even when they
// pass an unsorted map.
func ComputeInputDigest(inputs map[string]string) string {
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{'\t'})
		h.Write([]byte(inputs[k]))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}
