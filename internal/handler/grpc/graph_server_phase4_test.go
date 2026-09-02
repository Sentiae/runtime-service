package grpc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/sentiae/platform-kit/middleware"
	"github.com/sentiae/platform-kit/tenant"
	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/app"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

func init() {
	// The graph RPCs answer through pkerrors.ToGRPC, which reads the registry
	// the bootstrap fills. Without this the codes asserted below would all be
	// Internal — the very collapse the registration exists to prevent.
	app.RegisterErrors()
}

// recordingGraphUC captures whatever survived the handler's refusals.
type recordingGraphUC struct{ created []usecase.CreateGraphInput }

func (r *recordingGraphUC) CreateGraph(_ context.Context, in usecase.CreateGraphInput) (*domain.GraphDefinition, error) {
	r.created = append(r.created, in)
	return &domain.GraphDefinition{ID: uuid.New(), OrganizationID: in.OrganizationID, Name: in.Name}, nil
}
func (r *recordingGraphUC) GetGraph(context.Context, uuid.UUID) (*domain.GraphDefinition, []domain.GraphNode, []domain.GraphEdge, error) {
	return nil, nil, nil, domain.ErrGraphNotFound
}
func (r *recordingGraphUC) UpdateGraph(context.Context, uuid.UUID, usecase.UpdateGraphInput) (*domain.GraphDefinition, error) {
	return nil, domain.ErrGraphNotFound
}
func (r *recordingGraphUC) DeleteGraph(context.Context, uuid.UUID) error { return nil }
func (r *recordingGraphUC) ListGraphs(context.Context, uuid.UUID, int, int) ([]domain.GraphDefinition, int64, error) {
	return nil, 0, nil
}
func (r *recordingGraphUC) DeployGraph(context.Context, uuid.UUID) (*domain.GraphDefinition, error) {
	return nil, domain.ErrGraphNotFound
}
func (r *recordingGraphUC) ValidateGraph(context.Context, uuid.UUID) error { return nil }

// orgCtx builds a caller the handler will attribute to org.
func orgCtx(org uuid.UUID) context.Context {
	return tenant.ContextWithPrincipal(context.Background(), tenant.Principal{
		Claims: &middleware.Claims{OrganizationID: org.String()},
	})
}

func validNodePB() *runtimev1.GraphNodeInput {
	return &runtimev1.GraphNodeInput{
		Name: "intake",
		NodeRef: &runtimev1.NodeRef{
			QualifiedName: "@sentiae/webhook-trigger",
			Semver:        "1.0.0",
			Language:      "go",
			ImageRef:      "10.0.10.20:8443/sentiae/webhook-trigger.node:1.0.0-go",
			Digest:        "sha256:aaf52988aaf52988aaf52988aaf52988aaf52988aaf52988aaf52988aaf52988",
		},
		Ports: &runtimev1.PortSpecs{Outputs: []*runtimev1.PortSpec{{Name: "body"}}},
		Role:  "trigger",
	}
}

// T1.6 — the wire refuses the interpreter's shape. node_type, language and code
// are retired; a caller still sending them is running against the pre-Phase-4
// contract and is told so, rather than having the fields silently dropped.
//
// Control: delete the `n.GetLanguage() != "" || n.GetCode() != ""` branch from
// graphNodeInputFromPB ⇒ the "language" and "code" rows create a graph, i.e. a
// caller believes its source shipped when the runtime discarded it.
func TestCreateGraph_RefusesLegacyInput(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*runtimev1.GraphNodeInput)
		wantCode codes.Code
		wantMsg  string
	}{
		{"legacy node_type", func(n *runtimev1.GraphNodeInput) { n.NodeType = "code" }, codes.InvalidArgument, domain.ErrLegacyNodeInput.Error()},
		{"legacy transform type", func(n *runtimev1.GraphNodeInput) { n.NodeType = "transform" }, codes.InvalidArgument, domain.ErrLegacyNodeInput.Error()},
		{"language", func(n *runtimev1.GraphNodeInput) { n.Language = "python" }, codes.InvalidArgument, domain.ErrLegacyNodeInput.Error()},
		{"code", func(n *runtimev1.GraphNodeInput) { n.Code = "print(1)" }, codes.InvalidArgument, domain.ErrLegacyNodeInput.Error()},
		{"missing node_ref", func(n *runtimev1.GraphNodeInput) { n.NodeRef = nil }, codes.InvalidArgument, domain.ErrNodeRefRequired.Error()},
		{"node_ref with a tag for a digest", func(n *runtimev1.GraphNodeInput) { n.NodeRef.Digest = "1.0.0-go" }, codes.InvalidArgument, domain.ErrInvalidNodeRef.Error()},
		{"unknown language in node_ref", func(n *runtimev1.GraphNodeInput) { n.NodeRef.Language = "python" }, codes.InvalidArgument, domain.ErrInvalidNodeRef.Error()},
		{"invalid role", func(n *runtimev1.GraphNodeInput) { n.Role = "transform" }, codes.InvalidArgument, domain.ErrInvalidRole.Error()},
		{"invalid port name", func(n *runtimev1.GraphNodeInput) {
			n.Ports = &runtimev1.PortSpecs{Outputs: []*runtimev1.PortSpec{{Name: "Body"}}}
		}, codes.InvalidArgument, domain.ErrInvalidPortSpec.Error()},
		{"invalid secret name", func(n *runtimev1.GraphNodeInput) {
			n.Secrets = []*runtimev1.SecretSpec{{Name: "API_KEY"}}
		}, codes.InvalidArgument, domain.ErrInvalidSecretSpec.Error()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uc := &recordingGraphUC{}
			srv := NewGraphServer(uc, nil)
			node := validNodePB()
			tt.mutate(node)

			_, err := srv.CreateGraph(orgCtx(uuid.New()), &runtimev1.CreateGraphRequest{
				Name:  "phase 4",
				Nodes: []*runtimev1.GraphNodeInput{node},
			})
			if err == nil {
				t.Fatal("CreateGraph accepted a retired input")
			}
			st, ok := status.FromError(err)
			if !ok {
				t.Fatalf("error is not a gRPC status: %v", err)
			}
			if st.Code() != tt.wantCode {
				t.Fatalf("code = %s, want %s (message %q)", st.Code(), tt.wantCode, st.Message())
			}
			if st.Message() != tt.wantMsg {
				t.Fatalf("message = %q, want %q", st.Message(), tt.wantMsg)
			}
			if len(uc.created) != 0 {
				t.Fatalf("a refused request still reached the use case: %+v", uc.created)
			}
		})
	}
}

// Positive anchor: the same request WITHOUT a retired field is accepted and the
// bundle fields arrive intact. Without this, every assertion above would still
// pass if CreateGraph refused everything.
func TestCreateGraph_AcceptsBundleNode(t *testing.T) {
	uc := &recordingGraphUC{}
	srv := NewGraphServer(uc, nil)
	org := uuid.New()

	node := validNodePB()
	node.NodeType = "bundle" // explicitly allowed
	node.Secrets = []*runtimev1.SecretSpec{{Name: "greeting_suffix", Required: false}}
	node.Egress = []string{"httpbin.org"}
	node.SortOrder = 0
	cfg, err := structpb.NewStruct(map[string]any{"path": "/phase-4"})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	node.Config = cfg

	if _, err := srv.CreateGraph(orgCtx(org), &runtimev1.CreateGraphRequest{
		Name:  "phase 4",
		Nodes: []*runtimev1.GraphNodeInput{node},
	}); err != nil {
		t.Fatalf("CreateGraph on a bundle node: %v", err)
	}
	if len(uc.created) != 1 {
		t.Fatalf("use case saw %d creates, want 1", len(uc.created))
	}
	got := uc.created[0]
	if got.OrganizationID != org {
		t.Fatalf("org = %s, want %s", got.OrganizationID, org)
	}
	if len(got.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(got.Nodes))
	}
	n := got.Nodes[0]
	if n.NodeRef == nil || n.NodeRef.Pin() != "@sentiae/webhook-trigger@1.0.0" {
		t.Fatalf("node ref did not survive the mapping: %+v", n.NodeRef)
	}
	if n.Role != domain.NodeRoleTrigger {
		t.Fatalf("role = %q, want trigger", n.Role)
	}
	if len(n.Ports.Outputs) != 1 || n.Ports.Outputs[0].Name != "body" {
		t.Fatalf("ports did not survive the mapping: %+v", n.Ports)
	}
	if len(n.Secrets) != 1 || n.Secrets[0].Name != "greeting_suffix" {
		t.Fatalf("secrets did not survive the mapping: %+v", n.Secrets)
	}
	if len(n.Egress) != 1 || n.Egress[0] != "httpbin.org" {
		t.Fatalf("egress did not survive the mapping: %+v", n.Egress)
	}
	if n.Config["path"] != "/phase-4" {
		t.Fatalf("config did not survive the mapping: %+v", n.Config)
	}
}

// T1.6 (second half) — seeded_outputs is retired. A caller that still sends the
// map must be told, not quietly ignored: silently dropping it would let the
// caller believe it skipped work the runtime actually re-ran.
//
// Control: delete the `len(req.GetSeededOutputs()) > 0` branch from ExecuteGraph
// ⇒ the request falls through to the nil-engine Unavailable instead of the
// InvalidArgument refusal, and the row fails.
func TestExecuteGraph_RefusesSeededOutputs(t *testing.T) {
	srv := NewGraphServer(&recordingGraphUC{}, nil)
	seed, err := structpb.NewStruct(map[string]any{"out": "cached"})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}

	_, err = srv.ExecuteGraph(orgCtx(uuid.New()), &runtimev1.ExecuteGraphRequest{
		GraphId:       uuid.New().String(),
		SeededOutputs: map[string]*structpb.Struct{"greet": seed},
	})
	if err == nil {
		t.Fatal("ExecuteGraph accepted seeded_outputs")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("code = %s, want InvalidArgument (message %q)", st.Code(), st.Message())
	}
	if st.Message() != domain.ErrSeededOutputsRetired.Error() {
		t.Fatalf("message = %q, want %q", st.Message(), domain.ErrSeededOutputsRetired.Error())
	}

	// Anchor: with no seeds the request gets PAST the retirement check and
	// fails on the unconfigured engine instead — so the assertion above is
	// measuring the seed refusal and not a blanket rejection.
	_, err = srv.ExecuteGraph(orgCtx(uuid.New()), &runtimev1.ExecuteGraphRequest{GraphId: uuid.New().String()})
	st, _ = status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Fatalf("unseeded request code = %s, want Unavailable", st.Code())
	}
}
