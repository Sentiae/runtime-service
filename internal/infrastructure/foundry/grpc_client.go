package foundry

import (
	"context"
	"fmt"

	foundryv1 "github.com/sentiae/foundry-service/gen/proto/foundry/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// grpcDispatcher wraps foundry's gRPC FoundryService for the `generate_tests`
// Dispatch operation. A7.2: runtime-service prefers this path when a gRPC
// channel is wired. HTTP stays as a rollback fallback.
type grpcDispatcher struct {
	client foundryv1.FoundryServiceClient
}

func newGRPCDispatcher(conn *grpc.ClientConn) *grpcDispatcher {
	if conn == nil {
		return nil
	}
	return &grpcDispatcher{client: foundryv1.NewFoundryServiceClient(conn)}
}

// generateTests issues Dispatch(operation=generate_tests) and maps the
// Struct result back into GenerateTestResponse.
func (d *grpcDispatcher) generateTests(ctx context.Context, orgID string, req GenerateTestRequest) (*GenerateTestResponse, error) {
	if d == nil || d.client == nil {
		return nil, fmt.Errorf("foundry grpc dispatcher not configured")
	}
	criteria := make([]interface{}, len(req.Criteria))
	for i, c := range req.Criteria {
		criteria[i] = c
	}
	params := map[string]interface{}{
		"criteria":  criteria,
		"framework": req.Framework,
		"language":  req.Language,
		"context":   req.Context,
	}
	pb, err := structpb.NewStruct(params)
	if err != nil {
		return nil, fmt.Errorf("params -> struct: %w", err)
	}
	resp, err := d.client.Dispatch(ctx, &foundryv1.DispatchRequest{
		Operation:      "generate_tests",
		OrganizationId: orgID,
		Params:         pb,
	})
	if err != nil {
		return nil, fmt.Errorf("grpc FoundryService.Dispatch(generate_tests): %w", err)
	}
	if resp.Data == nil {
		return nil, fmt.Errorf("grpc generate_tests: empty data")
	}
	m := resp.Data.AsMap()
	out := &GenerateTestResponse{}
	if v, ok := m["framework"].(string); ok {
		out.Framework = v
	}
	if v, ok := m["language"].(string); ok {
		out.Language = v
	}
	if v, ok := m["code"].(string); ok {
		out.Code = v
	}
	if v, ok := m["notes"].(string); ok {
		out.Notes = v
	}
	return out, nil
}
