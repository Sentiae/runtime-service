package grpc

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// WithCompiler attaches the multi-file project compile use case to the
// ExecutionServer so the Compile RPC becomes available. Safe to call after
// NewExecutionServer; last-write-wins.
func (s *ExecutionServer) WithCompiler(uc *usecase.CompileProject) *ExecutionServer {
	s.compileUC = uc
	return s
}

// Compile builds a multi-file project in an ephemeral build container and
// returns structured compiler diagnostics. The result is never executed.
func (s *ExecutionServer) Compile(ctx context.Context, req *runtimev1.CompileRequest) (*runtimev1.CompileResponse, error) {
	if s.compileUC == nil {
		return nil, status.Error(codes.Unavailable, "compile use case not configured")
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}

	files := make([]domain.SourceFile, 0, len(req.GetFiles()))
	for _, f := range req.GetFiles() {
		files = append(files, domain.SourceFile{
			Path:    f.GetPath(),
			Content: f.GetContent(),
		})
	}

	out, err := s.compileUC.Execute(ctx, usecase.CompileProjectInput{
		Language:   req.GetLanguage(),
		Files:      files,
		TimeoutSec: int(req.GetTimeoutSec()),
	})
	if err != nil {
		return nil, handleDomainError(err)
	}

	return compileResultToProto(out.Result), nil
}

// compileResultToProto maps a domain CompileResult to the proto response.
func compileResultToProto(r *domain.CompileResult) *runtimev1.CompileResponse {
	if r == nil {
		return &runtimev1.CompileResponse{}
	}
	diags := make([]*runtimev1.CompileDiagnostic, 0, len(r.Diagnostics))
	for _, d := range r.Diagnostics {
		diags = append(diags, &runtimev1.CompileDiagnostic{
			File:    d.File,
			Line:    int32(d.Line),
			Column:  int32(d.Column),
			Message: d.Message,
		})
	}
	return &runtimev1.CompileResponse{
		Ok:            r.OK,
		Diagnostics:  diags,
		RawOutput:     r.RawOutput,
		CompileTimeMs: r.CompileTimeMS,
	}
}
