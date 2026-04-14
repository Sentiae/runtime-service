package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"gorm.io/gorm"
)

// TestRunRepo is the exported alias used by handlers.
type TestRunRepo = testRunRepository

type testRunRepository struct {
	db *gorm.DB
}

func NewTestRunRepository(db *gorm.DB) *testRunRepository {
	return &testRunRepository{db: db}
}

func (r *testRunRepository) Create(ctx context.Context, run *domain.TestRun) error {
	return r.db.WithContext(ctx).Create(run).Error
}

func (r *testRunRepository) Update(ctx context.Context, run *domain.TestRun) error {
	return r.db.WithContext(ctx).Save(run).Error
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
