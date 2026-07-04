package grpc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// stubCompiler is a ProjectCompiler test double for the handler tests. It
// never touches docker — it returns a canned result so the gRPC mapping
// is exercised in isolation.
type stubCompiler struct {
	result *domain.CompileResult
	err    error
}

func (s *stubCompiler) Compile(_ context.Context, _ string, _ []domain.SourceFile, _ int) (*domain.CompileResult, error) {
	return s.result, s.err
}

func TestRuntime_Compile_Unavailable_WithoutUC(t *testing.T) {
	client, _, _, cleanup := newTestServerExec(t)
	defer cleanup()

	_, err := client.Compile(context.Background(), &runtimev1.CompileRequest{
		Language: "go",
		Files:    []*runtimev1.CompileSourceFile{{Path: "main.go", Content: "package main"}},
	})
	if code := status.Code(err); code != codes.Unavailable {
		t.Fatalf("expected Unavailable without compile UC, got %s", code)
	}
}

func TestRuntime_Compile_HappyPath(t *testing.T) {
	client, _, srv, cleanup := newTestServerExec(t)
	defer cleanup()

	stub := &stubCompiler{result: &domain.CompileResult{
		OK: true,
		Diagnostics: []domain.CompileDiagnostic{
			{File: "internal/x.go", Line: 4, Column: 2, Message: "boom"},
		},
		RawOutput:     "raw build output",
		CompileTimeMS: 77,
	}}
	srv.ExecutionServer().WithCompiler(usecase.NewCompileProject(stub))

	resp, err := client.Compile(context.Background(), &runtimev1.CompileRequest{
		OrganizationId: uuid.New().String(),
		RequestedBy:    uuid.New().String(),
		Language:       "go",
		Files:          []*runtimev1.CompileSourceFile{{Path: "internal/x.go", Content: "package internal"}},
		TimeoutSec:     30,
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !resp.GetOk() {
		t.Fatalf("expected ok=true")
	}
	if resp.GetCompileTimeMs() != 77 {
		t.Fatalf("compile_time_ms = %d, want 77", resp.GetCompileTimeMs())
	}
	if resp.GetRawOutput() != "raw build output" {
		t.Fatalf("raw_output = %q", resp.GetRawOutput())
	}
	diags := resp.GetDiagnostics()
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(diags))
	}
	if diags[0].GetFile() != "internal/x.go" || diags[0].GetLine() != 4 || diags[0].GetColumn() != 2 || diags[0].GetMessage() != "boom" {
		t.Fatalf("unexpected diagnostic: %+v", diags[0])
	}
}

func TestRuntime_Compile_UnsupportedLanguage(t *testing.T) {
	client, _, srv, cleanup := newTestServerExec(t)
	defer cleanup()

	srv.ExecutionServer().WithCompiler(usecase.NewCompileProject(&stubCompiler{}))

	_, err := client.Compile(context.Background(), &runtimev1.CompileRequest{
		Language: "cobol",
		Files:    []*runtimev1.CompileSourceFile{{Path: "x.cob", Content: ""}},
	})
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s", code)
	}
}

func TestRuntime_Compile_NoFiles(t *testing.T) {
	client, _, srv, cleanup := newTestServerExec(t)
	defer cleanup()

	srv.ExecutionServer().WithCompiler(usecase.NewCompileProject(&stubCompiler{}))

	_, err := client.Compile(context.Background(), &runtimev1.CompileRequest{
		Language: "go",
	})
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s", code)
	}
}

func TestRuntime_Compile_ToolchainUnavailable(t *testing.T) {
	client, _, srv, cleanup := newTestServerExec(t)
	defer cleanup()

	srv.ExecutionServer().WithCompiler(usecase.NewCompileProject(
		&stubCompiler{err: domain.ErrCompileToolchainUnavailable},
	))

	_, err := client.Compile(context.Background(), &runtimev1.CompileRequest{
		Language: "go",
		Files:    []*runtimev1.CompileSourceFile{{Path: "main.go", Content: "package main"}},
	})
	if code := status.Code(err); code != codes.Unavailable {
		t.Fatalf("expected Unavailable, got %s", code)
	}
}
