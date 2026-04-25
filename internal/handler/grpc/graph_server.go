package grpc

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// GraphServer implements runtimev1.GraphServiceServer.
// Thin wrapper over GraphUseCase + GraphExecutionEngine — replaces the
// legacy REST endpoints under /api/v1/graphs + /api/v1/graph-executions
// per Rule 3 (service↔service = gRPC).
type GraphServer struct {
	runtimev1.UnimplementedGraphServiceServer

	graphUC  usecase.GraphUseCase
	execEng  *usecase.GraphExecutionEngine
}

// NewGraphServer constructs the handler.
func NewGraphServer(graphUC usecase.GraphUseCase, execEng *usecase.GraphExecutionEngine) *GraphServer {
	return &GraphServer{graphUC: graphUC, execEng: execEng}
}

// --- helpers --------------------------------------------------------

func graphOrgIDFromCtx(ctx context.Context) uuid.UUID {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, k := range []string{"x-organization-id"} {
		if vals := md.Get(k); len(vals) > 0 && vals[0] != "" {
			if id, err := uuid.Parse(vals[0]); err == nil {
				return id
			}
		}
	}
	return uuid.Nil
}

func graphUserIDFromCtx(ctx context.Context) uuid.UUID {
	md, _ := metadata.FromIncomingContext(ctx)
	if vals := md.Get("x-user-id"); len(vals) > 0 && vals[0] != "" {
		if id, err := uuid.Parse(vals[0]); err == nil {
			return id
		}
	}
	return uuid.Nil
}

func structToJSONMap(s *structpb.Struct) domain.JSONMap {
	if s == nil {
		return nil
	}
	return domain.JSONMap(s.AsMap())
}

func jsonMapToStruct(m domain.JSONMap) *structpb.Struct {
	if len(m) == 0 {
		return nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	var generic map[string]interface{}
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil
	}
	out, err := structpb.NewStruct(generic)
	if err != nil {
		return nil
	}
	return out
}

func graphToPB(g *domain.GraphDefinition) *runtimev1.Graph {
	if g == nil {
		return nil
	}
	canvasID := ""
	if g.CanvasID != nil {
		canvasID = g.CanvasID.String()
	}
	return &runtimev1.Graph{
		Id:             g.ID.String(),
		OrganizationId: g.OrganizationID.String(),
		CanvasId:       canvasID,
		Name:           g.Name,
		Description:    g.Description,
		Version:        int32(g.Version),
		Status:         string(g.Status),
		CreatedBy:      g.CreatedBy.String(),
		CreatedAt:      g.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:      g.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func timePtrToRFC3339(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func graphExecToPB(e *domain.GraphExecution) *runtimev1.GraphExecution {
	if e == nil {
		return nil
	}
	out := &runtimev1.GraphExecution{
		Id:             e.ID.String(),
		GraphId:        e.GraphID.String(),
		OrganizationId: e.OrganizationID.String(),
		Status:         string(e.Status),
		Input:          jsonMapToStruct(e.Input),
		Output:         jsonMapToStruct(e.Output),
		Error:          e.Error,
		TotalNodes:     int32(e.TotalNodes),
		CompletedNodes: int32(e.CompletedNodes),
		RequestedBy:    e.RequestedBy.String(),
		DebugMode:      e.DebugMode,
		StartedAt:      timePtrToRFC3339(e.StartedAt),
		CompletedAt:    timePtrToRFC3339(e.CompletedAt),
		CreatedAt:      e.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if e.DurationMS != nil {
		out.HasDuration = true
		out.DurationMs = *e.DurationMS
	}
	return out
}

func nodeExecToPB(n *domain.NodeExecution) *runtimev1.NodeExecution {
	if n == nil {
		return nil
	}
	out := &runtimev1.NodeExecution{
		Id:               n.ID.String(),
		GraphExecutionId: n.GraphExecutionID.String(),
		GraphNodeId:      n.GraphNodeID.String(),
		NodeType:         string(n.NodeType),
		NodeName:         n.NodeName,
		SequenceNumber:   int32(n.SequenceNumber),
		Status:           string(n.Status),
		Input:            jsonMapToStruct(n.Input),
		Output:           jsonMapToStruct(n.Output),
		Error:            n.Error,
		StartedAt:        timePtrToRFC3339(n.StartedAt),
		CompletedAt:      timePtrToRFC3339(n.CompletedAt),
		CreatedAt:        n.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if n.DurationMS != nil {
		out.HasDuration = true
		out.DurationMs = *n.DurationMS
	}
	return out
}

// --- RPCs -----------------------------------------------------------

func (s *GraphServer) CreateGraph(ctx context.Context, req *runtimev1.CreateGraphRequest) (*runtimev1.Graph, error) {
	orgID := graphOrgIDFromCtx(ctx)
	if orgID == uuid.Nil {
		return nil, status.Error(codes.Unauthenticated, "missing x-organization-id")
	}
	userID := graphUserIDFromCtx(ctx)

	var canvasID *uuid.UUID
	if req.CanvasId != "" {
		if id, err := uuid.Parse(req.CanvasId); err == nil {
			canvasID = &id
		}
	}

	nodes := make([]usecase.CreateGraphNodeInput, 0, len(req.Nodes))
	for _, n := range req.Nodes {
		var lang *domain.Language
		if n.Language != "" {
			l := domain.Language(n.Language)
			lang = &l
		}
		var resources *domain.ResourceLimit
		if n.Resources != nil {
			resources = &domain.ResourceLimit{
				VCPU:       int(n.Resources.CpuMillicores / 1000),
				MemoryMB:   int(n.Resources.MemoryMib),
				TimeoutSec: int(n.Resources.TimeoutSec),
			}
			if resources.VCPU == 0 {
				resources.VCPU = 1
			}
		}
		nodes = append(nodes, usecase.CreateGraphNodeInput{
			NodeType:  domain.GraphNodeType(n.NodeType),
			Name:      n.Name,
			Config:    structToJSONMap(n.Config),
			Language:  lang,
			Code:      n.Code,
			Resources: resources,
			Position:  structToJSONMap(n.Position),
			SortOrder: int(n.SortOrder),
		})
	}
	edges := make([]usecase.CreateGraphEdgeInput, 0, len(req.Edges))
	for _, e := range req.Edges {
		edges = append(edges, usecase.CreateGraphEdgeInput{
			SourceNodeIndex: int(e.SourceNodeIndex),
			TargetNodeIndex: int(e.TargetNodeIndex),
			SourcePort:      e.SourcePort,
			TargetPort:      e.TargetPort,
			ConditionExpr:   e.ConditionExpr,
		})
	}

	g, err := s.graphUC.CreateGraph(ctx, usecase.CreateGraphInput{
		OrganizationID: orgID,
		CanvasID:       canvasID,
		Name:           req.Name,
		Description:    req.Description,
		CreatedBy:      userID,
		Nodes:          nodes,
		Edges:          edges,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create graph: %v", err)
	}
	return graphToPB(g), nil
}

func (s *GraphServer) DeployGraph(ctx context.Context, req *runtimev1.DeployGraphRequest) (*runtimev1.Graph, error) {
	id, err := uuid.Parse(req.GraphId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid graph_id")
	}
	g, err := s.graphUC.DeployGraph(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "deploy graph: %v", err)
	}
	return graphToPB(g), nil
}

func (s *GraphServer) ExecuteGraph(ctx context.Context, req *runtimev1.ExecuteGraphRequest) (*runtimev1.GraphExecution, error) {
	if s.execEng == nil {
		return nil, status.Error(codes.Unavailable, "graph execution engine not configured")
	}
	graphID, err := uuid.Parse(req.GraphId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid graph_id")
	}
	orgID := graphOrgIDFromCtx(ctx)
	userID := graphUserIDFromCtx(ctx)
	input := structToJSONMap(req.Input)
	exec, err := s.execEng.ExecuteGraph(ctx, graphID, orgID, userID, input, false)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "execute graph: %v", err)
	}
	return graphExecToPB(exec), nil
}

func (s *GraphServer) GetGraphExecution(ctx context.Context, req *runtimev1.GetGraphExecutionRequest) (*runtimev1.GraphExecution, error) {
	if s.execEng == nil {
		return nil, status.Error(codes.Unavailable, "graph execution engine not configured")
	}
	id, err := uuid.Parse(req.ExecutionId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid execution_id")
	}
	exec, err := s.execEng.GetGraphExecution(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "execution not found: %v", err)
	}
	return graphExecToPB(exec), nil
}

func (s *GraphServer) CancelGraphExecution(ctx context.Context, req *runtimev1.CancelGraphExecutionRequest) (*runtimev1.CancelGraphExecutionResponse, error) {
	if s.execEng == nil {
		return nil, status.Error(codes.Unavailable, "graph execution engine not configured")
	}
	id, err := uuid.Parse(req.ExecutionId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid execution_id")
	}
	if err := s.execEng.CancelGraphExecution(ctx, id); err != nil {
		return nil, status.Errorf(codes.Internal, "cancel: %v", err)
	}
	return &runtimev1.CancelGraphExecutionResponse{Ok: true}, nil
}

func (s *GraphServer) GetGraph(ctx context.Context, req *runtimev1.GetGraphRequest) (*runtimev1.Graph, error) {
	id, err := uuid.Parse(req.GraphId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid graph_id")
	}
	g, _, _, err := s.graphUC.GetGraph(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "graph not found: %v", err)
	}
	return graphToPB(g), nil
}

func (s *GraphServer) ListGraphs(ctx context.Context, req *runtimev1.ListGraphsRequest) (*runtimev1.ListGraphsResponse, error) {
	orgID := uuid.Nil
	if req.OrganizationId != "" {
		if id, err := uuid.Parse(req.OrganizationId); err == nil {
			orgID = id
		}
	}
	if orgID == uuid.Nil {
		orgID = graphOrgIDFromCtx(ctx)
	}
	if orgID == uuid.Nil {
		return nil, status.Error(codes.InvalidArgument, "organization_id required")
	}
	page := int(req.Page)
	if page <= 0 {
		page = 1
	}
	pageSize := int(req.PageSize)
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	offset := (page - 1) * pageSize
	graphs, total, err := s.graphUC.ListGraphs(ctx, orgID, pageSize, offset)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list graphs: %v", err)
	}
	items := make([]*runtimev1.Graph, 0, len(graphs))
	for i := range graphs {
		items = append(items, graphToPB(&graphs[i]))
	}
	return &runtimev1.ListGraphsResponse{
		Items:    items,
		Total:    total,
		Page:     int32(page),
		PageSize: int32(pageSize),
	}, nil
}

func (s *GraphServer) DeleteGraph(ctx context.Context, req *runtimev1.DeleteGraphRequest) (*runtimev1.DeleteGraphResponse, error) {
	id, err := uuid.Parse(req.GraphId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid graph_id")
	}
	if err := s.graphUC.DeleteGraph(ctx, id); err != nil {
		return nil, status.Errorf(codes.Internal, "delete graph: %v", err)
	}
	return &runtimev1.DeleteGraphResponse{Ok: true}, nil
}

func (s *GraphServer) ListGraphExecutions(ctx context.Context, req *runtimev1.ListGraphExecutionsRequest) (*runtimev1.ListGraphExecutionsResponse, error) {
	if s.execEng == nil {
		return nil, status.Error(codes.Unavailable, "graph execution engine not configured")
	}
	graphID, err := uuid.Parse(req.GraphId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid graph_id")
	}
	page := int(req.Page)
	if page <= 0 {
		page = 1
	}
	pageSize := int(req.PageSize)
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	offset := (page - 1) * pageSize
	execs, total, err := s.execEng.ListGraphExecutions(ctx, graphID, pageSize, offset)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list graph executions: %v", err)
	}
	items := make([]*runtimev1.GraphExecution, 0, len(execs))
	for i := range execs {
		items = append(items, graphExecToPB(&execs[i]))
	}
	return &runtimev1.ListGraphExecutionsResponse{
		Items:    items,
		Total:    total,
		Page:     int32(page),
		PageSize: int32(pageSize),
	}, nil
}

func (s *GraphServer) GetNodeExecution(ctx context.Context, req *runtimev1.GetNodeExecutionRequest) (*runtimev1.NodeExecution, error) {
	if s.execEng == nil {
		return nil, status.Error(codes.Unavailable, "graph execution engine not configured")
	}
	nodeID, err := uuid.Parse(req.NodeId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid node_id")
	}
	n, err := s.execEng.GetNodeExecution(ctx, nodeID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "node execution not found: %v", err)
	}
	return nodeExecToPB(n), nil
}

func (s *GraphServer) ListNodeExecutions(ctx context.Context, req *runtimev1.ListNodeExecutionsRequest) (*runtimev1.ListNodeExecutionsResponse, error) {
	if s.execEng == nil {
		return nil, status.Error(codes.Unavailable, "graph execution engine not configured")
	}
	id, err := uuid.Parse(req.ExecutionId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid execution_id")
	}
	items, err := s.execEng.ListNodeExecutions(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list node executions: %v", err)
	}
	out := make([]*runtimev1.NodeExecution, 0, len(items))
	for i := range items {
		out = append(out, nodeExecToPB(&items[i]))
	}
	return &runtimev1.ListNodeExecutionsResponse{Items: out}, nil
}
