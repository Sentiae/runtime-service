package usecase

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// GraphUseCase defines the interface for graph definition business logic
type GraphUseCase interface {
	CreateGraph(ctx context.Context, input CreateGraphInput) (*domain.GraphDefinition, error)
	GetGraph(ctx context.Context, id uuid.UUID) (*domain.GraphDefinition, []domain.GraphNode, []domain.GraphEdge, error)
	UpdateGraph(ctx context.Context, id uuid.UUID, input UpdateGraphInput) (*domain.GraphDefinition, error)
	DeleteGraph(ctx context.Context, id uuid.UUID) error
	ListGraphs(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]domain.GraphDefinition, int64, error)
	DeployGraph(ctx context.Context, id uuid.UUID) (*domain.GraphDefinition, error)
	ValidateGraph(ctx context.Context, id uuid.UUID) error
}

// CreateGraphInput represents the input for creating a graph definition
type CreateGraphInput struct {
	OrganizationID uuid.UUID
	CanvasID       *uuid.UUID
	WorkflowID     *uuid.UUID
	Name           string
	Description    string
	CreatedBy      uuid.UUID
	Nodes          []CreateGraphNodeInput
	Edges          []CreateGraphEdgeInput
}

// CreateGraphNodeInput represents a node to create within a graph
type CreateGraphNodeInput struct {
	NodeType  domain.GraphNodeType
	Name      string
	Config    domain.JSONMap
	Language  *domain.Language
	Code      string
	Resources *domain.ResourceLimit
	Position  domain.JSONMap
	SortOrder int
}

// CreateGraphEdgeInput represents an edge to create within a graph
type CreateGraphEdgeInput struct {
	SourceNodeIndex int // index into the Nodes slice
	TargetNodeIndex int
	SourcePort      string
	TargetPort      string
	ConditionExpr   string
}

// UpdateGraphInput represents the input for updating a graph definition
type UpdateGraphInput struct {
	Name        *string
	Description *string
	Nodes       []CreateGraphNodeInput
	Edges       []CreateGraphEdgeInput
}

type graphService struct {
	graphRepo      repository.GraphDefinitionRepository
	nodeRepo       repository.GraphNodeRepository
	edgeRepo       repository.GraphEdgeRepository
	eventPublisher EventPublisher
}

// NewGraphService creates a new graph use case service
func NewGraphService(
	graphRepo repository.GraphDefinitionRepository,
	nodeRepo repository.GraphNodeRepository,
	edgeRepo repository.GraphEdgeRepository,
	eventPublisher EventPublisher,
) GraphUseCase {
	return &graphService{
		graphRepo:      graphRepo,
		nodeRepo:       nodeRepo,
		edgeRepo:       edgeRepo,
		eventPublisher: eventPublisher,
	}
}

func (s *graphService) CreateGraph(ctx context.Context, input CreateGraphInput) (*domain.GraphDefinition, error) {
	now := time.Now().UTC()

	graph := &domain.GraphDefinition{
		ID:             uuid.New(),
		OrganizationID: input.OrganizationID,
		CanvasID:       input.CanvasID,
		WorkflowID:     input.WorkflowID,
		Name:           input.Name,
		Description:    input.Description,
		Version:        1,
		Status:         domain.GraphStatusDraft,
		CreatedBy:      input.CreatedBy,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if err := graph.Validate(); err != nil {
		return nil, err
	}

	if err := s.graphRepo.Create(ctx, graph); err != nil {
		return nil, fmt.Errorf("failed to create graph: %w", err)
	}

	// Create nodes
	nodes := make([]domain.GraphNode, len(input.Nodes))
	for i, n := range input.Nodes {
		resources := domain.ResourceLimit{}
		if n.Resources != nil {
			resources = *n.Resources
		}
		resources.ApplyDefaults()

		nodes[i] = domain.GraphNode{
			ID:        uuid.New(),
			GraphID:   graph.ID,
			NodeType:  n.NodeType,
			Name:      n.Name,
			Config:    n.Config,
			Language:  n.Language,
			Code:      n.Code,
			Resources: resources,
			Position:  n.Position,
			SortOrder: n.SortOrder,
			CreatedAt: now,
		}
	}
	if len(nodes) > 0 {
		if err := s.nodeRepo.CreateBatch(ctx, nodes); err != nil {
			return nil, fmt.Errorf("failed to create graph nodes: %w", err)
		}
	}

	// Create edges (referencing node IDs by index)
	edges := make([]domain.GraphEdge, 0, len(input.Edges))
	for _, e := range input.Edges {
		if e.SourceNodeIndex < 0 || e.SourceNodeIndex >= len(nodes) ||
			e.TargetNodeIndex < 0 || e.TargetNodeIndex >= len(nodes) {
			return nil, fmt.Errorf("invalid edge node index")
		}
		sourcePort := e.SourcePort
		if sourcePort == "" {
			sourcePort = "output"
		}
		targetPort := e.TargetPort
		if targetPort == "" {
			targetPort = "input"
		}
		edges = append(edges, domain.GraphEdge{
			ID:            uuid.New(),
			GraphID:       graph.ID,
			SourceNodeID:  nodes[e.SourceNodeIndex].ID,
			TargetNodeID:  nodes[e.TargetNodeIndex].ID,
			SourcePort:    sourcePort,
			TargetPort:    targetPort,
			ConditionExpr: e.ConditionExpr,
			CreatedAt:     now,
		})
	}
	if len(edges) > 0 {
		if err := s.edgeRepo.CreateBatch(ctx, edges); err != nil {
			return nil, fmt.Errorf("failed to create graph edges: %w", err)
		}
	}

	_ = s.eventPublisher.Publish(ctx, EventGraphCreated, graph.ID.String(), graph)
	log.Printf("Graph created: %s (name=%s, nodes=%d, edges=%d)", graph.ID, graph.Name, len(nodes), len(edges))
	return graph, nil
}

func (s *graphService) GetGraph(ctx context.Context, id uuid.UUID) (*domain.GraphDefinition, []domain.GraphNode, []domain.GraphEdge, error) {
	graph, err := s.graphRepo.FindByID(ctx, id)
	if err != nil {
		return nil, nil, nil, err
	}

	nodes, err := s.nodeRepo.FindByGraph(ctx, id)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to load graph nodes: %w", err)
	}

	edges, err := s.edgeRepo.FindByGraph(ctx, id)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to load graph edges: %w", err)
	}

	return graph, nodes, edges, nil
}

func (s *graphService) UpdateGraph(ctx context.Context, id uuid.UUID, input UpdateGraphInput) (*domain.GraphDefinition, error) {
	graph, err := s.graphRepo.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}

	if input.Name != nil {
		graph.Name = *input.Name
	}
	if input.Description != nil {
		graph.Description = *input.Description
	}
	graph.Version++
	graph.UpdatedAt = time.Now().UTC()

	if err := s.graphRepo.Update(ctx, graph); err != nil {
		return nil, fmt.Errorf("failed to update graph: %w", err)
	}

	// Replace nodes and edges if provided
	if input.Nodes != nil {
		now := time.Now().UTC()

		if err := s.edgeRepo.DeleteByGraph(ctx, id); err != nil {
			return nil, fmt.Errorf("failed to delete old edges: %w", err)
		}
		if err := s.nodeRepo.DeleteByGraph(ctx, id); err != nil {
			return nil, fmt.Errorf("failed to delete old nodes: %w", err)
		}

		nodes := make([]domain.GraphNode, len(input.Nodes))
		for i, n := range input.Nodes {
			resources := domain.ResourceLimit{}
			if n.Resources != nil {
				resources = *n.Resources
			}
			resources.ApplyDefaults()
			nodes[i] = domain.GraphNode{
				ID:        uuid.New(),
				GraphID:   id,
				NodeType:  n.NodeType,
				Name:      n.Name,
				Config:    n.Config,
				Language:  n.Language,
				Code:      n.Code,
				Resources: resources,
				Position:  n.Position,
				SortOrder: n.SortOrder,
				CreatedAt: now,
			}
		}
		if len(nodes) > 0 {
			if err := s.nodeRepo.CreateBatch(ctx, nodes); err != nil {
				return nil, fmt.Errorf("failed to create graph nodes: %w", err)
			}
		}

		if input.Edges != nil {
			edges := make([]domain.GraphEdge, 0, len(input.Edges))
			for _, e := range input.Edges {
				if e.SourceNodeIndex < 0 || e.SourceNodeIndex >= len(nodes) ||
					e.TargetNodeIndex < 0 || e.TargetNodeIndex >= len(nodes) {
					return nil, fmt.Errorf("invalid edge node index")
				}
				sourcePort := e.SourcePort
				if sourcePort == "" {
					sourcePort = "output"
				}
				targetPort := e.TargetPort
				if targetPort == "" {
					targetPort = "input"
				}
				edges = append(edges, domain.GraphEdge{
					ID:            uuid.New(),
					GraphID:       id,
					SourceNodeID:  nodes[e.SourceNodeIndex].ID,
					TargetNodeID:  nodes[e.TargetNodeIndex].ID,
					SourcePort:    sourcePort,
					TargetPort:    targetPort,
					ConditionExpr: e.ConditionExpr,
					CreatedAt:     now,
				})
			}
			if len(edges) > 0 {
				if err := s.edgeRepo.CreateBatch(ctx, edges); err != nil {
					return nil, fmt.Errorf("failed to create graph edges: %w", err)
				}
			}
		}
	}

	_ = s.eventPublisher.Publish(ctx, EventGraphUpdated, graph.ID.String(), graph)
	return graph, nil
}

func (s *graphService) DeleteGraph(ctx context.Context, id uuid.UUID) error {
	if err := s.edgeRepo.DeleteByGraph(ctx, id); err != nil {
		return fmt.Errorf("failed to delete edges: %w", err)
	}
	if err := s.nodeRepo.DeleteByGraph(ctx, id); err != nil {
		return fmt.Errorf("failed to delete nodes: %w", err)
	}
	if err := s.graphRepo.Delete(ctx, id); err != nil {
		return err
	}
	_ = s.eventPublisher.Publish(ctx, EventGraphDeleted, id.String(), map[string]string{"id": id.String()})
	return nil
}

func (s *graphService) ListGraphs(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]domain.GraphDefinition, int64, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	return s.graphRepo.FindByOrganization(ctx, orgID, limit, offset)
}

func (s *graphService) DeployGraph(ctx context.Context, id uuid.UUID) (*domain.GraphDefinition, error) {
	if err := s.ValidateGraph(ctx, id); err != nil {
		return nil, err
	}

	graph, err := s.graphRepo.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}

	graph.Status = domain.GraphStatusActive
	graph.UpdatedAt = time.Now().UTC()
	if err := s.graphRepo.Update(ctx, graph); err != nil {
		return nil, fmt.Errorf("failed to deploy graph: %w", err)
	}

	_ = s.eventPublisher.Publish(ctx, EventGraphDeployed, graph.ID.String(), graph)
	log.Printf("Graph deployed: %s (name=%s)", graph.ID, graph.Name)
	return graph, nil
}

func (s *graphService) ValidateGraph(ctx context.Context, id uuid.UUID) error {
	nodes, err := s.nodeRepo.FindByGraph(ctx, id)
	if err != nil {
		return fmt.Errorf("failed to load nodes: %w", err)
	}
	if len(nodes) == 0 {
		return fmt.Errorf("graph has no nodes")
	}

	edges, err := s.edgeRepo.FindByGraph(ctx, id)
	if err != nil {
		return fmt.Errorf("failed to load edges: %w", err)
	}

	// Build node ID set for edge validation
	nodeSet := make(map[uuid.UUID]bool, len(nodes))
	for i := range nodes {
		nodeSet[nodes[i].ID] = true
	}

	// Validate edges reference valid nodes
	for _, edge := range edges {
		if !nodeSet[edge.SourceNodeID] {
			return fmt.Errorf("edge references unknown source node %s", edge.SourceNodeID)
		}
		if !nodeSet[edge.TargetNodeID] {
			return fmt.Errorf("edge references unknown target node %s", edge.TargetNodeID)
		}
	}

	// Validate code nodes have language and code
	for _, node := range nodes {
		if err := node.Validate(); err != nil {
			return fmt.Errorf("node %q (%s): %w", node.Name, node.ID, err)
		}
	}

	// Cycle detection via Kahn's algorithm
	inDegree := make(map[uuid.UUID]int, len(nodes))
	adjacency := make(map[uuid.UUID][]uuid.UUID)
	for i := range nodes {
		inDegree[nodes[i].ID] = 0
	}
	for _, edge := range edges {
		adjacency[edge.SourceNodeID] = append(adjacency[edge.SourceNodeID], edge.TargetNodeID)
		inDegree[edge.TargetNodeID]++
	}

	queue := make([]uuid.UUID, 0)
	for id, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, id)
		}
	}

	processed := 0
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		processed++
		for _, next := range adjacency[current] {
			inDegree[next]--
			if inDegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	if processed < len(nodes) {
		return domain.ErrGraphHasCycle
	}

	return nil
}
