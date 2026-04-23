//go:build integration

package messaging_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"
	kafka "github.com/sentiae/platform-kit/kafka"
	"github.com/sentiae/platform-kit/testutil"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/infrastructure/messaging"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// integrationPool captures PoolScheduler.Submit calls so the
// integration test can assert dispatch fan-out without spinning up
// Firecracker. The real pool isn't the focus of this test — the
// repository query and the consumer's write path are.
type integrationPool struct {
	mu   sync.Mutex
	jobs []usecase.PoolJob
}

func (p *integrationPool) Submit(_ context.Context, j usecase.PoolJob) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jobs = append(p.jobs, j)
	return nil
}

type integrationPublisher struct{ events int }

func (p *integrationPublisher) Publish(_ context.Context, _, _ string, _ any) error {
	p.events++
	return nil
}
func (p *integrationPublisher) Close() error { return nil }

func TestSessionCommitAddedConsumer_IntegrationWithRealRepo(t *testing.T) {
	db := testutil.NewTestDB(t, "")
	require.NoError(t, db.AutoMigrate(&domain.TestRun{}))

	repo := postgres.NewTestRunRepository(db)
	orgID := uuid.New()
	canvasID := uuid.New()
	ctx := context.Background()

	// Seed two tests wired to the canvas.
	require.NoError(t, repo.Create(ctx, &domain.TestRun{
		ID:             uuid.New(),
		OrganizationID: orgID,
		CanvasID:       &canvasID,
		TestNodeID:     uuid.New(),
		Language:       domain.Language("go"),
		TestType:       domain.TestTypeUnit,
		Status:         domain.TestRunStatusPassed,
	}))
	require.NoError(t, repo.Create(ctx, &domain.TestRun{
		ID:             uuid.New(),
		OrganizationID: orgID,
		CanvasID:       &canvasID,
		TestNodeID:     uuid.New(),
		Language:       domain.Language("python"),
		TestType:       domain.TestTypeUnit,
		Status:         domain.TestRunStatusPassed,
	}))

	pool := &integrationPool{}
	pub := &integrationPublisher{}
	c := messaging.NewSessionCommitAddedConsumer(repo, pool, pub)

	payload := map[string]any{
		"organization_id": orgID.String(),
		"metadata": map[string]any{
			"canvas_id":     canvasID.String(),
			"commit_sha":    "abc123",
			"repository_id": uuid.NewString(),
		},
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	err = c.HandleGitEvent(ctx, kafka.CloudEvent{
		Type: messaging.EventTypeSessionCommitAdded,
		Data: raw,
	})
	require.NoError(t, err)

	pool.mu.Lock()
	jobs := len(pool.jobs)
	pool.mu.Unlock()
	assert.Equal(t, 2, jobs)
	assert.Equal(t, 1, pub.events, "queued event should be emitted once for the batch")

	// Repo should now hold 2 seed + 2 fresh runs = 4 total on the canvas.
	rows, total, err := repo.FindByCanvas(ctx, canvasID, 100, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(4), total)
	assert.Len(t, rows, 4)
}
