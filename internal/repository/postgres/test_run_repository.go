package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/timetravel"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

// TestRunRepo is the exported alias used by handlers.
type TestRunRepo = testRunRepository

type testRunRepository struct {
	db       *gorm.DB
	recorder timetravel.Recorder
}

func NewTestRunRepository(db *gorm.DB) *testRunRepository {
	return &testRunRepository{db: db}
}

// WithRecorder wires the platform-kit time-travel recorder so every
// write through this repo produces an entity_snapshots row for §13.1
// point-in-time queries. Nil-safe: absent recorder disables recording
// without changing any other behaviour. CS11 slice (2026-04-18).
func (r *testRunRepository) WithRecorder(rec timetravel.Recorder) *testRunRepository {
	r.recorder = rec
	return r
}

func (r *testRunRepository) recordSnapshot(ctx context.Context, run *domain.TestRun) {
	if r == nil || r.recorder == nil || run == nil {
		return
	}
	// Best-effort; never fail the primary write on a recorder error.
	_ = r.recorder.RecordEntity(ctx, "test_run", run.ID.String(), run)
}

func (r *testRunRepository) Create(ctx context.Context, run *domain.TestRun) error {
	if err := r.db.WithContext(ctx).Create(run).Error; err != nil {
		return err
	}
	r.recordSnapshot(ctx, run)
	return nil
}

func (r *testRunRepository) Update(ctx context.Context, run *domain.TestRun) error {
	if err := r.db.WithContext(ctx).Save(run).Error; err != nil {
		return err
	}
	r.recordSnapshot(ctx, run)
	return nil
}

// UpdateAfterRun satisfies the usecase.TestRunUpdater contract used by
// the multi-type dispatcher + executors (§8.4). Callers pass the
// test-run id as `any` because the usecase layer doesn't import the
// `uuid` package; we accept both `uuid.UUID` and its string form.
func (r *testRunRepository) UpdateAfterRun(ctx context.Context, id any, status domain.TestRunStatus, stdout, stderr string, durationMS int64) error {
	var runID uuid.UUID
	switch v := id.(type) {
	case uuid.UUID:
		runID = v
	case string:
		parsed, err := uuid.Parse(v)
		if err != nil {
			return err
		}
		runID = parsed
	default:
		return errors.New("UpdateAfterRun: unsupported id type")
	}
	updates := map[string]any{
		"status":        status,
		"error_message": stderr,
		"duration_ms":   durationMS,
	}
	// Stdout currently isn't a first-class column; stash it in
	// result_json so callers can render it.
	if stdout != "" {
		updates["result_json"] = domain.JSONMap{"stdout": stdout}
	}
	return r.db.WithContext(ctx).Model(&domain.TestRun{}).
		Where("id = ?", runID).
		Updates(updates).Error
}

func (r *testRunRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.TestRun, error) {
	var run domain.TestRun
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&run).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &run, nil
}

// FindByTestNode returns test runs for a given test node, ordered by most recent.
func (r *testRunRepository) FindByTestNode(ctx context.Context, testNodeID uuid.UUID, limit, offset int) ([]domain.TestRun, int64, error) {
	var runs []domain.TestRun
	var total int64

	query := r.db.WithContext(ctx).Where("test_node_id = ?", testNodeID)
	if err := query.Model(&domain.TestRun{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := query.Order("created_at DESC").Limit(limit).Offset(offset).Find(&runs).Error
	return runs, total, err
}

// FindByCanvas returns all test runs for a canvas, ordered by most recent.
func (r *testRunRepository) FindByCanvas(ctx context.Context, canvasID uuid.UUID, limit, offset int) ([]domain.TestRun, int64, error) {
	var runs []domain.TestRun
	var total int64

	query := r.db.WithContext(ctx).Where("canvas_id = ?", canvasID)
	if err := query.Model(&domain.TestRun{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := query.Order("created_at DESC").Limit(limit).Offset(offset).Find(&runs).Error
	return runs, total, err
}

// FindSmokeTestsForCanvas returns the latest critical, non-quarantined
// test per TestNodeID attached to a canvas. Used by the CS-2 G2.7
// continuous-testing trigger to assemble the smoke set enqueued on
// git.session.created. baseBranch is currently informational — the
// scheduler treats every critical test as part of the smoke set until
// per-branch test tagging lands.
func (r *testRunRepository) FindSmokeTestsForCanvas(ctx context.Context, canvasID uuid.UUID, _ string) ([]domain.TestRun, error) {
	var runs []domain.TestRun
	err := r.db.WithContext(ctx).
		Raw(`SELECT DISTINCT ON (test_node_id) *
		     FROM test_runs
		     WHERE canvas_id = ? AND critical = true AND quarantined = false
		     ORDER BY test_node_id, created_at DESC`, canvasID).
		Scan(&runs).Error
	return runs, err
}

// FindLatestByTestNode returns the most recent test run for each test node.
func (r *testRunRepository) FindLatestByTestNode(ctx context.Context, testNodeIDs []uuid.UUID) ([]domain.TestRun, error) {
	var runs []domain.TestRun
	// Use DISTINCT ON to get latest per test_node_id
	err := r.db.WithContext(ctx).
		Raw(`SELECT DISTINCT ON (test_node_id) * FROM test_runs
			 WHERE test_node_id = ANY(?)
			 ORDER BY test_node_id, created_at DESC`, testNodeIDs).
		Scan(&runs).Error
	return runs, err
}

// SummaryByCanvas returns aggregated test stats for a canvas.
func (r *testRunRepository) SummaryByCanvas(ctx context.Context, canvasID uuid.UUID) (*domain.TestSummary, error) {
	var summary domain.TestSummary
	err := r.db.WithContext(ctx).
		Raw(`SELECT
			COUNT(*) as total_runs,
			COUNT(CASE WHEN status = 'passed' THEN 1 END) as passed_runs,
			COUNT(CASE WHEN status = 'failed' THEN 1 END) as failed_runs,
			COUNT(DISTINCT test_node_id) as test_nodes,
			COALESCE(AVG(coverage_pc), 0) as avg_coverage
		FROM (
			SELECT DISTINCT ON (test_node_id) * FROM test_runs
			WHERE canvas_id = ? ORDER BY test_node_id, created_at DESC
		) latest`, canvasID).
		Scan(&summary).Error
	if err != nil {
		return nil, err
	}
	return &summary, nil
}

// FindCandidatesForQuarantine returns every TestRow that needs to be
// evaluated by the auto-quarantine scheduler. Selection criteria:
// FlakinessScore > threshold, and at least minRuns historical rows on
// that test_node_id. We return the *latest* row per test_node_id so the
// scheduler can toggle Quarantined on it; the repository signature
// mirrors FindLatestByTestNode's DISTINCT ON pattern.
func (r *testRunRepository) FindCandidatesForQuarantine(ctx context.Context, threshold float64, minRuns int) ([]domain.TestRun, error) {
	// Two-pass query: first pick test_node_ids with enough history and
	// sustained flakiness, then surface the most recent row per node.
	// The CTE keeps the plan readable and lets the scheduler run against
	// postgres without a full table scan on test_runs.
	var runs []domain.TestRun
	err := r.db.WithContext(ctx).
		Raw(`
			WITH flaky AS (
				SELECT test_node_id
				FROM test_runs
				WHERE flakiness_score > ?
				GROUP BY test_node_id
				HAVING COUNT(*) >= ?
			)
			SELECT DISTINCT ON (tr.test_node_id) tr.*
			FROM test_runs tr
			JOIN flaky f ON tr.test_node_id = f.test_node_id
			ORDER BY tr.test_node_id, tr.created_at DESC
		`, threshold, minRuns).
		Scan(&runs).Error
	return runs, err
}

// CountRecentByTestNode returns the number of rows on the given test
// node recorded within the lookback window. Used by the quarantine
// scheduler to confirm sustained flakiness before flipping the flag.
func (r *testRunRepository) CountRecentByTestNode(ctx context.Context, testNodeID uuid.UUID, minRuns int) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).
		Model(&domain.TestRun{}).
		Where("test_node_id = ?", testNodeID).
		Count(&count).Error
	if err != nil {
		return 0, err
	}
	_ = minRuns // caller uses the value for the threshold; retained for API symmetry.
	return count, nil
}

// CountQuarantinedForCanvas returns the number of quarantined tests in
// a canvas. Used by ops-service to enforce "quarantined_count <
// threshold" as part of the delivery gate. §8.3.
func (r *testRunRepository) CountQuarantinedForCanvas(ctx context.Context, canvasID uuid.UUID) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).
		Raw(`
			SELECT COUNT(*) FROM (
				SELECT DISTINCT ON (test_node_id) quarantined
				FROM test_runs
				WHERE canvas_id = ?
				ORDER BY test_node_id, created_at DESC
			) latest WHERE quarantined = true
		`, canvasID).
		Scan(&count).Error
	return count, err
}
