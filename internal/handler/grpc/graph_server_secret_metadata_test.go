package grpc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// ---------------------------------------------------------------------------
// A real engine over stub repositories. The handler holds the concrete engine,
// so the only way to observe what it passed is to let the engine judge it: the
// credential check is the engine's first opinion on the token/environment pair,
// and it runs before the execution row is created.
// ---------------------------------------------------------------------------

// errRunRowRefused is what the execution repository answers so a request that
// PASSED the credential check stops immediately, without starting a run
// goroutine. Reaching it is the proof that both values arrived.
var errRunRowRefused = errors.New("execution row refused by the test repository")

type secretGraphStore struct {
	graph *domain.GraphDefinition
	nodes []domain.GraphNode
}

type stubDefRepo struct{ s *secretGraphStore }

func (r stubDefRepo) Create(context.Context, *domain.GraphDefinition) error { return nil }
func (r stubDefRepo) Update(context.Context, *domain.GraphDefinition) error { return nil }
func (r stubDefRepo) FindByID(context.Context, uuid.UUID) (*domain.GraphDefinition, error) {
	return r.s.graph, nil
}
func (r stubDefRepo) FindByOrganization(context.Context, uuid.UUID, int, int) ([]domain.GraphDefinition, int64, error) {
	return nil, 0, nil
}
func (r stubDefRepo) FindActive(context.Context, uuid.UUID) ([]domain.GraphDefinition, error) {
	return nil, nil
}
func (r stubDefRepo) Delete(context.Context, uuid.UUID) error { return nil }

type stubNodeRepo struct{ s *secretGraphStore }

func (r stubNodeRepo) Create(context.Context, *domain.GraphNode) error       { return nil }
func (r stubNodeRepo) CreateBatch(context.Context, []domain.GraphNode) error { return nil }
func (r stubNodeRepo) Update(context.Context, *domain.GraphNode) error       { return nil }
func (r stubNodeRepo) FindByID(context.Context, uuid.UUID) (*domain.GraphNode, error) {
	return nil, domain.ErrGraphNodeNotFound
}
func (r stubNodeRepo) FindByGraph(context.Context, uuid.UUID) ([]domain.GraphNode, error) {
	return r.s.nodes, nil
}
func (r stubNodeRepo) DeleteByGraph(context.Context, uuid.UUID) error { return nil }

type stubEdgeRepo struct{}

func (stubEdgeRepo) Create(context.Context, *domain.GraphEdge) error       { return nil }
func (stubEdgeRepo) CreateBatch(context.Context, []domain.GraphEdge) error { return nil }
func (stubEdgeRepo) FindByGraph(context.Context, uuid.UUID) ([]domain.GraphEdge, error) {
	return nil, nil
}
func (stubEdgeRepo) DeleteByGraph(context.Context, uuid.UUID) error { return nil }

type refusingExecRepo struct{}

func (refusingExecRepo) Create(context.Context, *domain.GraphExecution) error {
	return errRunRowRefused
}
func (refusingExecRepo) Update(context.Context, *domain.GraphExecution) error { return nil }
func (refusingExecRepo) FindByID(context.Context, uuid.UUID) (*domain.GraphExecution, error) {
	return nil, domain.ErrGraphExecutionNotFound
}
func (refusingExecRepo) FindByGraph(context.Context, uuid.UUID, int, int) ([]domain.GraphExecution, int64, error) {
	return nil, 0, nil
}
func (refusingExecRepo) FindPending(context.Context, int) ([]domain.GraphExecution, error) {
	return nil, nil
}

type stubNodeExecRepo struct{}

func (stubNodeExecRepo) Create(context.Context, *domain.NodeExecution) error { return nil }
func (stubNodeExecRepo) Update(context.Context, *domain.NodeExecution) error { return nil }
func (stubNodeExecRepo) FindByID(context.Context, uuid.UUID) (*domain.NodeExecution, error) {
	return nil, domain.ErrNodeExecutionNotFound
}
func (stubNodeExecRepo) FindByGraphExecution(context.Context, uuid.UUID) ([]domain.NodeExecution, error) {
	return nil, nil
}

type silentPublisher struct{}

func (silentPublisher) Publish(context.Context, string, string, any) error { return nil }
func (silentPublisher) Close() error                                       { return nil }

// secretDeclaringServer builds a handler over a one-node graph whose node
// declares a secret — the shape whose run the two metadata values govern.
func secretDeclaringServer(t *testing.T, secrets []domain.SecretSpec) (*GraphServer, uuid.UUID, uuid.UUID) {
	t.Helper()
	org := uuid.New()
	ref, err := domain.NewNodeRef("@acme/hello", "1.0.9", "go",
		"10.0.10.20:8078/acme/hello.node:1.0.9-go", "sha256:"+strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("NewNodeRef: %v", err)
	}
	ports, err := domain.NewPortSpecs(nil, []domain.PortSpec{{Name: "out", Required: true}})
	if err != nil {
		t.Fatalf("NewPortSpecs: %v", err)
	}
	store := &secretGraphStore{
		graph: &domain.GraphDefinition{
			ID: uuid.New(), OrganizationID: org, Name: "secret graph",
			Status: domain.GraphStatusActive,
		},
		nodes: []domain.GraphNode{{
			ID: uuid.New(), NodeType: domain.GraphNodeTypeBundle, Name: "greet",
			Resources: domain.ResourceLimit{MemoryMB: 64, TimeoutSec: 5},
			NodeRef:   ref, Ports: ports, Secrets: secrets,
		}},
	}
	engine := usecase.NewGraphExecutionEngine(
		stubDefRepo{store}, stubNodeRepo{store}, stubEdgeRepo{},
		refusingExecRepo{}, stubNodeExecRepo{},
		silentPublisher{},
		usecase.NewNodeInvoker(
			usecase.NotConfiguredBundleRunner{}, usecase.NotConfiguredSidecarManager{},
			nil, nil, "10.0.10.20:8443", usecase.SystemClock{},
		),
		usecase.NotConfiguredSidecarManager{},
	)
	return NewGraphServer(&recordingGraphUC{}, engine), store.graph.ID, org
}

// T3.2 — ExecuteGraph reads BOTH x-sentiae-secret-token and
// x-sentiae-flow-environment off the request metadata and hands both to the
// engine. They travel together or not at all: a token without an environment
// would resolve refs under an empty environment, which is a different tenant
// path that happens to exist, not a failure.
//
// The engine's credential check is the observation point — it is the first
// thing that reads the pair and it runs before the execution row is created, so
// each row's error names exactly which of the two values arrived.
//
// Control: pass "" for both arguments (the pre-S3a call) ⇒ the three rows that
// hand a token all report ErrSecretTokenRequired and fail.
func TestExecuteGraph_PassesSecretToken(t *testing.T) {
	declared := []domain.SecretSpec{{Name: "greeting_suffix"}}

	tests := []struct {
		name    string
		secrets []domain.SecretSpec
		md      map[string]string
		wantErr error
	}{
		{
			name: "both values reach the engine", secrets: declared,
			md:      map[string]string{"x-sentiae-secret-token": "handed-token", "x-sentiae-flow-environment": "preview"},
			wantErr: errRunRowRefused,
		},
		{
			name: "the token reaches the engine without an environment", secrets: declared,
			md:      map[string]string{"x-sentiae-secret-token": "handed-token"},
			wantErr: domain.ErrEnvironmentRequired,
		},
		{
			name: "the environment reaches the engine and is checked", secrets: declared,
			md:      map[string]string{"x-sentiae-secret-token": "handed-token", "x-sentiae-flow-environment": "staging"},
			wantErr: domain.ErrEnvironmentInvalid,
		},
		{
			name: "no metadata at all is a secret-declaring graph without a token", secrets: declared,
			md: map[string]string{}, wantErr: domain.ErrSecretTokenRequired,
		},
		{
			name: "an environment on a secretless graph is refused", secrets: nil,
			md:      map[string]string{"x-sentiae-flow-environment": "preview"},
			wantErr: domain.ErrEnvironmentUnexpected,
		},
		{
			name: "a token on a secretless graph is refused", secrets: nil,
			md:      map[string]string{"x-sentiae-secret-token": "handed-token", "x-sentiae-flow-environment": "preview"},
			wantErr: domain.ErrSecretTokenUnexpected,
		},
		{
			name: "a secretless graph with no metadata runs", secrets: nil,
			md: map[string]string{}, wantErr: errRunRowRefused,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, graphID, org := secretDeclaringServer(t, tt.secrets)
			ctx := metadata.NewIncomingContext(orgCtx(org), metadata.New(tt.md))

			_, err := srv.ExecuteGraph(ctx, &runtimev1.ExecuteGraphRequest{GraphId: graphID.String()})
			if err == nil {
				t.Fatalf("ExecuteGraph error = nil, want %v", tt.wantErr)
			}
			// The handler converts through pkerrors.ToGRPC, so the sentinel is
			// matched on the message the status carries.
			if !strings.Contains(err.Error(), tt.wantErr.Error()) {
				t.Fatalf("ExecuteGraph error = %v, want one carrying %q", err, tt.wantErr.Error())
			}
		})
	}
}
