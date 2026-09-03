package usecase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sentiae/platform-kit/flowlang"
	"github.com/sentiae/runtime-service/internal/domain"
)

// goldenPlanSHA256 pins the ONE wire projection of a lowered plan across
// platform-kit, this service and codegen. The bytes below are a copy; if the
// copy drifts from what S0 produced, every later assertion in this file would
// be measuring the copy instead of the contract, so the hash is checked first
// and nothing else runs until it holds.
//
// Derived by S0 from the file it produced, recomputed independently with
// `shasum -a 256` (2039 bytes). Never typed from memory.
const goldenPlanSHA256 = "21d980bb21e12ed1ea6c99af4718405a1260496c8a74fff7b43bfc575ec85943"

const goldenPlanPath = "testdata/09_phase4_acceptance.plan.json"

// fakeGraphDefRepo/fakeGraphNodeRepo/fakeGraphEdgeRepo record what CreateGraph
// wrote, so a refusal can be told apart from a partial write.
type fakeGraphDefRepo struct{ created []*domain.GraphDefinition }

func (f *fakeGraphDefRepo) Create(_ context.Context, g *domain.GraphDefinition) error {
	f.created = append(f.created, g)
	return nil
}
func (f *fakeGraphDefRepo) Update(context.Context, *domain.GraphDefinition) error { return nil }
func (f *fakeGraphDefRepo) FindByID(context.Context, uuid.UUID) (*domain.GraphDefinition, error) {
	return nil, domain.ErrGraphNotFound
}
func (f *fakeGraphDefRepo) FindByOrganization(context.Context, uuid.UUID, int, int) ([]domain.GraphDefinition, int64, error) {
	return nil, 0, nil
}
func (f *fakeGraphDefRepo) FindActive(context.Context, uuid.UUID) ([]domain.GraphDefinition, error) {
	return nil, nil
}
func (f *fakeGraphDefRepo) Delete(context.Context, uuid.UUID) error { return nil }

type fakeGraphNodeRepo struct{ batches [][]domain.GraphNode }

func (f *fakeGraphNodeRepo) Create(context.Context, *domain.GraphNode) error { return nil }
func (f *fakeGraphNodeRepo) CreateBatch(_ context.Context, nodes []domain.GraphNode) error {
	f.batches = append(f.batches, nodes)
	return nil
}
func (f *fakeGraphNodeRepo) Update(context.Context, *domain.GraphNode) error { return nil }
func (f *fakeGraphNodeRepo) FindByID(context.Context, uuid.UUID) (*domain.GraphNode, error) {
	return nil, domain.ErrGraphNodeNotFound
}
func (f *fakeGraphNodeRepo) FindByGraph(context.Context, uuid.UUID) ([]domain.GraphNode, error) {
	return nil, nil
}
func (f *fakeGraphNodeRepo) DeleteByGraph(context.Context, uuid.UUID) error { return nil }

type fakeGraphEdgeRepo struct{ batches [][]domain.GraphEdge }

func (f *fakeGraphEdgeRepo) Create(context.Context, *domain.GraphEdge) error { return nil }
func (f *fakeGraphEdgeRepo) CreateBatch(_ context.Context, edges []domain.GraphEdge) error {
	f.batches = append(f.batches, edges)
	return nil
}
func (f *fakeGraphEdgeRepo) FindByGraph(context.Context, uuid.UUID) ([]domain.GraphEdge, error) {
	return nil, nil
}
func (f *fakeGraphEdgeRepo) DeleteByGraph(context.Context, uuid.UUID) error { return nil }

type fakeEventPublisher struct{ published []string }

func (f *fakeEventPublisher) Publish(_ context.Context, eventType, _ string, _ any) error {
	f.published = append(f.published, eventType)
	return nil
}
func (f *fakeEventPublisher) Close() error { return nil }

func newTestGraphService() (GraphUseCase, *fakeGraphDefRepo, *fakeGraphNodeRepo, *fakeGraphEdgeRepo) {
	defs := &fakeGraphDefRepo{}
	nodes := &fakeGraphNodeRepo{}
	edges := &fakeGraphEdgeRepo{}
	return NewGraphService(defs, nodes, edges, &fakeEventPublisher{}), defs, nodes, edges
}

func testRef(t *testing.T, qualified, semver string) *domain.NodeRef {
	t.Helper()
	ref, err := domain.NewNodeRef(qualified, semver, "go",
		"10.0.10.20:8443/"+strings.TrimPrefix(qualified, "@")+".node:"+semver+"-go",
		"sha256:"+strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("NewNodeRef(%q,%q): %v", qualified, semver, err)
	}
	return ref
}

// T1.5 — CreateGraph refuses a node with no pin and refuses a node set whose
// carried order is not the order the plan reconstitutes, and it refuses BEFORE
// writing anything.
//
// Control (node ref): remove the `n.NodeRef == nil` branch from
// validateBundleNodes ⇒ the "missing node ref" row reaches PlanFromWire and
// panics on a nil pin instead of refusing.
// Control (topology): delete the sort.SliceStable in validateGraphPlan ⇒ the
// "sort_order disagrees with topology" row passes, because the inputs happen to
// arrive in dependency order.
func TestCreateGraph_RequiresNodeRefAndTopology(t *testing.T) {
	trigger := func(sortOrder int) CreateGraphNodeInput {
		return CreateGraphNodeInput{
			Name:      "intake",
			SortOrder: sortOrder,
			NodeRef:   testRef(t, "@sentiae/webhook-trigger", "1.0.0"),
			Ports:     domain.PortSpecs{Outputs: []domain.PortSpec{{Name: "body"}}},
			Role:      domain.NodeRoleTrigger,
		}
	}
	worker := func(sortOrder int) CreateGraphNodeInput {
		return CreateGraphNodeInput{
			Name:      "pick",
			SortOrder: sortOrder,
			NodeRef:   testRef(t, "@acme/pick-name", "1.0.0"),
			Ports: domain.PortSpecs{
				Inputs:  []domain.PortSpec{{Name: "body", Required: true}},
				Outputs: []domain.PortSpec{{Name: "name"}},
			},
		}
	}
	wire := []CreateGraphEdgeInput{{SourceNodeIndex: 0, TargetNodeIndex: 1, SourcePort: "body", TargetPort: "body"}}

	tests := []struct {
		name    string
		nodes   []CreateGraphNodeInput
		edges   []CreateGraphEdgeInput
		wantErr error
	}{
		{
			name:  "valid two node flow",
			nodes: []CreateGraphNodeInput{trigger(0), worker(1)},
			edges: wire,
		},
		{
			name: "missing node ref",
			nodes: []CreateGraphNodeInput{trigger(0), func() CreateGraphNodeInput {
				n := worker(1)
				n.NodeRef = nil
				return n
			}()},
			edges:   wire,
			wantErr: domain.ErrNodeRefRequired,
		},
		{
			name: "missing name",
			nodes: []CreateGraphNodeInput{trigger(0), func() CreateGraphNodeInput {
				n := worker(1)
				n.Name = ""
				return n
			}()},
			edges:   wire,
			wantErr: domain.ErrInvalidData,
		},
		{
			name: "invalid egress pattern",
			nodes: []CreateGraphNodeInput{trigger(0), func() CreateGraphNodeInput {
				n := worker(1)
				n.Egress = []string{"http://example.com/path"}
				return n
			}()},
			edges:   wire,
			wantErr: domain.ErrInvalidEgressPattern,
		},
		{
			name: "invalid role",
			nodes: []CreateGraphNodeInput{trigger(0), func() CreateGraphNodeInput {
				n := worker(1)
				n.Role = domain.NodeRole("transform")
				return n
			}()},
			edges:   wire,
			wantErr: domain.ErrInvalidRole,
		},
		{
			name:    "sort_order disagrees with topology",
			nodes:   []CreateGraphNodeInput{trigger(1), worker(0)},
			edges:   wire,
			wantErr: domain.ErrPlanInvalid,
		},
		{
			name:    "required input unwired",
			nodes:   []CreateGraphNodeInput{trigger(0), worker(1)},
			edges:   nil,
			wantErr: domain.ErrPlanInvalid,
		},
		{
			name:    "edge into the trigger",
			nodes:   []CreateGraphNodeInput{trigger(0), worker(1)},
			edges:   append(append([]CreateGraphEdgeInput{}, wire...), CreateGraphEdgeInput{SourceNodeIndex: 1, TargetNodeIndex: 0, SourcePort: "name", TargetPort: "body"}),
			wantErr: domain.ErrPlanInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, defs, nodes, edges := newTestGraphService()
			_, err := svc.CreateGraph(context.Background(), CreateGraphInput{
				OrganizationID: uuid.New(),
				Name:           "phase 4",
				CreatedBy:      uuid.New(),
				Nodes:          tt.nodes,
				Edges:          tt.edges,
			})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CreateGraph error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil {
				if len(nodes.batches) != 1 || len(nodes.batches[0]) != len(tt.nodes) {
					t.Fatalf("expected one node batch of %d, got %v", len(tt.nodes), nodes.batches)
				}
				for _, n := range nodes.batches[0] {
					if n.NodeType != domain.GraphNodeTypeBundle {
						t.Fatalf("node %q persisted as %q, want bundle", n.Name, n.NodeType)
					}
				}
				return
			}
			// A refusal writes NOTHING: the graph row, the nodes and the edges
			// are all absent, so there is no half-created graph to clean up.
			if len(defs.created) != 0 {
				t.Fatalf("refused create still wrote %d graph row(s)", len(defs.created))
			}
			if len(nodes.batches) != 0 {
				t.Fatalf("refused create still wrote node batches: %v", nodes.batches)
			}
			if len(edges.batches) != 0 {
				t.Fatalf("refused create still wrote edge batches: %v", edges.batches)
			}
		})
	}
}

// T1.5b — the golden lowered plan, byte-pinned, decoded strictly, mapped
// through CreateGraph exactly as delivery will map it, and required to
// reconstitute the SAME order it carried.
//
// This is the join between three slices: platform-kit produces the document,
// codegen emits it, and the runtime rebuilds a graph from it. All three assert
// this one constant, so a schema change on any side is caught on every side.
//
// Control 1 (the wire shape): rename one `to_port` key to `toPort` in the
// embedded copy ⇒ DisallowUnknownFields refuses the document.
// Control 2 (the order): permute `order` ⇒ sort_order stops being the
// topological index and CreateGraph refuses with ErrPlanInvalid.
func TestCreateGraph_GoldenPlan(t *testing.T) {
	raw, err := os.ReadFile(goldenPlanPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != goldenPlanSHA256 {
		t.Fatalf("golden plan sha256 = %s, want %s — this copy has drifted from platform-kit's", got, goldenPlanSHA256)
	}

	plan := decodeGoldenPlan(t, raw)

	if got, want := plan.Order, []string{"intake", "pick", "greet", "echo", "reply"}; !equalStringSlices(got, want) {
		t.Fatalf("golden order = %v, want %v", got, want)
	}
	if plan.Trigger != "intake" {
		t.Fatalf("golden trigger = %q, want intake", plan.Trigger)
	}

	svc, _, nodeRepo, edgeRepo := newTestGraphService()
	nodes, edges := planToCreateInputs(t, plan)
	if _, err := svc.CreateGraph(context.Background(), CreateGraphInput{
		OrganizationID: uuid.New(),
		Name:           "Phase 4 acceptance",
		CreatedBy:      uuid.New(),
		Nodes:          nodes,
		Edges:          edges,
	}); err != nil {
		t.Fatalf("CreateGraph on the golden plan: %v", err)
	}
	if len(nodeRepo.batches) != 1 || len(nodeRepo.batches[0]) != 5 {
		t.Fatalf("expected 5 persisted nodes, got %v", nodeRepo.batches)
	}
	if len(edgeRepo.batches) != 1 || len(edgeRepo.batches[0]) != 4 {
		t.Fatalf("expected 4 persisted edges, got %v", edgeRepo.batches)
	}

	// The same wire, straight through flowlang: the order the runtime
	// recomputes must be the order the document carried.
	rebuilt := rebuildPlan(t, plan)
	if !equalStringSlices(rebuilt.Order, plan.Order) {
		t.Fatalf("PlanFromWire order = %v, want the carried %v", rebuilt.Order, plan.Order)
	}

	// Control 2, driven: permuting `order` permutes sort_order, and the
	// carried order stops being topological.
	permuted := permuteGoldenOrder(t, raw)
	svc2, _, _, _ := newTestGraphService()
	pNodes, pEdges := planToCreateInputs(t, permuted)
	_, err = svc2.CreateGraph(context.Background(), CreateGraphInput{
		OrganizationID: uuid.New(),
		Name:           "permuted",
		CreatedBy:      uuid.New(),
		Nodes:          pNodes,
		Edges:          pEdges,
	})
	if !errors.Is(err, domain.ErrPlanInvalid) {
		t.Fatalf("permuted order: CreateGraph error = %v, want ErrPlanInvalid", err)
	}
	if !errors.Is(err, flowlang.ErrPlanNotTopological) {
		t.Fatalf("permuted order: error does not carry the flowlang cause: %v", err)
	}
}

// TestGoldenPlan_RejectsUnknownField is control 1, driven rather than described:
// the same document with one renamed key must not decode.
func TestGoldenPlan_RejectsUnknownField(t *testing.T) {
	raw, err := os.ReadFile(goldenPlanPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	mutated := bytes.Replace(raw, []byte(`"to_port"`), []byte(`"toPort"`), 1)
	if bytes.Equal(mutated, raw) {
		t.Fatal("mutation did not apply — the golden no longer contains a to_port key")
	}
	dec := json.NewDecoder(bytes.NewReader(mutated))
	dec.DisallowUnknownFields()
	var plan flowlang.WirePlan
	if err := dec.Decode(&plan); err == nil {
		t.Fatal("strict decode accepted an unknown field — the wire shape is not pinned")
	}
}

func decodeGoldenPlan(t *testing.T, raw []byte) flowlang.WirePlan {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var plan flowlang.WirePlan
	if err := dec.Decode(&plan); err != nil {
		t.Fatalf("strict decode of the golden plan: %v", err)
	}
	return plan
}

// planToCreateInputs is the mapping delivery performs (§3.5.2): sort_order is
// the node's index in `order`, and an edge carries the two ports by name.
func planToCreateInputs(t *testing.T, plan flowlang.WirePlan) ([]CreateGraphNodeInput, []CreateGraphEdgeInput) {
	t.Helper()
	orderIndex := map[string]int{}
	for i, slug := range plan.Order {
		orderIndex[slug] = i
	}
	index := map[string]int{}
	nodes := make([]CreateGraphNodeInput, 0, len(plan.Nodes))
	for i, n := range plan.Nodes {
		index[n.Slug] = i
		pin := n.Pin
		at := strings.LastIndex(pin, "@")
		if at <= 0 {
			t.Fatalf("node %q has an unusable pin %q", n.Slug, pin)
		}
		ref := testRef(t, pin[:at], pin[at+1:])

		var inputs []domain.PortSpec
		for _, req := range n.Required {
			inputs = append(inputs, domain.PortSpec{Name: req, Required: true})
		}
		var outputs []domain.PortSpec
		for _, o := range n.Outputs {
			outputs = append(outputs, domain.PortSpec{Name: o})
		}
		ports, err := domain.NewPortSpecs(inputs, outputs)
		if err != nil {
			t.Fatalf("NewPortSpecs for %q: %v", n.Slug, err)
		}
		role, err := domain.ParseNodeRole(n.Role)
		if err != nil {
			t.Fatalf("ParseNodeRole for %q: %v", n.Slug, err)
		}
		sortOrder, ok := orderIndex[n.Slug]
		if !ok {
			t.Fatalf("node %q is absent from the plan order", n.Slug)
		}
		nodes = append(nodes, CreateGraphNodeInput{
			Name:      n.Slug,
			SortOrder: sortOrder,
			NodeRef:   ref,
			Ports:     ports,
			Role:      role,
		})
	}

	edges := make([]CreateGraphEdgeInput, 0, len(plan.Edges))
	for _, e := range plan.Edges {
		from, ok := index[e.From]
		if !ok {
			t.Fatalf("edge names an unknown source %q", e.From)
		}
		to, ok := index[e.To]
		if !ok {
			t.Fatalf("edge names an unknown target %q", e.To)
		}
		edges = append(edges, CreateGraphEdgeInput{
			SourceNodeIndex: from,
			TargetNodeIndex: to,
			SourcePort:      e.FromPort,
			TargetPort:      e.ToPort,
		})
	}
	return nodes, edges
}

func rebuildPlan(t *testing.T, plan flowlang.WirePlan) *flowlang.Plan {
	t.Helper()
	edges := make([]flowlang.Edge, 0, len(plan.Edges))
	for _, e := range plan.Edges {
		edges = append(edges, flowlang.Edge{From: e.From, FromPort: e.FromPort, To: e.To, ToPort: e.ToPort})
	}
	rebuilt, err := flowlang.PlanFromWire(plan.Nodes, edges)
	if err != nil {
		t.Fatalf("PlanFromWire on the golden: %v", err)
	}
	return rebuilt
}

// permuteGoldenOrder swaps the first two entries of `order` and re-sorts the
// node list to match, which is exactly what a compiler emitting a wrong order
// would produce.
func permuteGoldenOrder(t *testing.T, raw []byte) flowlang.WirePlan {
	t.Helper()
	plan := decodeGoldenPlan(t, raw)
	if len(plan.Order) < 2 {
		t.Fatal("golden plan has fewer than two nodes; nothing to permute")
	}
	plan.Order[0], plan.Order[1] = plan.Order[1], plan.Order[0]
	pos := map[string]int{}
	for i, slug := range plan.Order {
		pos[slug] = i
	}
	sort.SliceStable(plan.Nodes, func(i, j int) bool { return pos[plan.Nodes[i].Slug] < pos[plan.Nodes[j].Slug] })
	return plan
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
