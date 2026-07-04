package usecase

import (
	"context"
	"errors"
	"testing"

	"github.com/sentiae/runtime-service/internal/domain"
)

// fakeProjectCompiler is a test double for ProjectCompiler. It records the
// last call and returns a canned result/error.
type fakeProjectCompiler struct {
	result    *domain.CompileResult
	err       error
	gotLang   string
	gotFiles  []domain.SourceFile
	gotTimeout int
	called    bool
}

func (f *fakeProjectCompiler) Compile(_ context.Context, language string, files []domain.SourceFile, timeoutSec int) (*domain.CompileResult, error) {
	f.called = true
	f.gotLang = language
	f.gotFiles = files
	f.gotTimeout = timeoutSec
	return f.result, f.err
}

func TestCompileProject_Execute(t *testing.T) {
	happyResult := &domain.CompileResult{
		OK:            true,
		RawOutput:     "",
		CompileTimeMS: 42,
	}

	tests := []struct {
		name        string
		in          CompileProjectInput
		fake        *fakeProjectCompiler
		wantErr     error
		wantCalled  bool
		wantOK      bool
	}{
		{
			name: "unsupported language",
			in: CompileProjectInput{
				Language: "rust",
				Files:    []domain.SourceFile{{Path: "main.rs", Content: ""}},
			},
			fake:       &fakeProjectCompiler{result: happyResult},
			wantErr:    domain.ErrUnsupportedCompileLanguage,
			wantCalled: false,
		},
		{
			name: "empty files",
			in: CompileProjectInput{
				Language: "go",
				Files:    nil,
			},
			fake:       &fakeProjectCompiler{result: happyResult},
			wantErr:    domain.ErrNoSourceFiles,
			wantCalled: false,
		},
		{
			name: "happy path go",
			in: CompileProjectInput{
				Language:   "go",
				Files:      []domain.SourceFile{{Path: "main.go", Content: "package main"}},
				TimeoutSec: 30,
			},
			fake:       &fakeProjectCompiler{result: happyResult},
			wantErr:    nil,
			wantCalled: true,
			wantOK:     true,
		},
		{
			name: "happy path typescript alias",
			in: CompileProjectInput{
				Language: "ts",
				Files:    []domain.SourceFile{{Path: "index.ts", Content: "export {}"}},
			},
			fake:       &fakeProjectCompiler{result: happyResult},
			wantErr:    nil,
			wantCalled: true,
			wantOK:     true,
		},
		{
			name: "infra error wrapped",
			in: CompileProjectInput{
				Language: "go",
				Files:    []domain.SourceFile{{Path: "main.go", Content: "package main"}},
			},
			fake:       &fakeProjectCompiler{err: domain.ErrCompileToolchainUnavailable},
			wantErr:    domain.ErrCompileToolchainUnavailable,
			wantCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uc := NewCompileProject(tt.fake)
			out, err := uc.Execute(context.Background(), tt.in)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Execute() err = %v, want %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Execute() unexpected err = %v", err)
			}

			if tt.fake.called != tt.wantCalled {
				t.Fatalf("compiler called = %v, want %v", tt.fake.called, tt.wantCalled)
			}

			if tt.wantErr == nil {
				if out.Result == nil {
					t.Fatalf("expected non-nil result")
				}
				if out.Result.OK != tt.wantOK {
					t.Fatalf("result.OK = %v, want %v", out.Result.OK, tt.wantOK)
				}
				if tt.fake.gotTimeout != tt.in.TimeoutSec {
					t.Fatalf("timeout passed = %d, want %d", tt.fake.gotTimeout, tt.in.TimeoutSec)
				}
			}
		})
	}
}

// TestCompileProject_NilCompiler ensures a missing backend degrades to the
// toolchain error rather than panicking.
func TestCompileProject_NilCompiler(t *testing.T) {
	uc := NewCompileProject(nil)
	_, err := uc.Execute(context.Background(), CompileProjectInput{
		Language: "go",
		Files:    []domain.SourceFile{{Path: "main.go", Content: "package main"}},
	})
	if !errors.Is(err, domain.ErrCompileToolchainUnavailable) {
		t.Fatalf("expected ErrCompileToolchainUnavailable, got %v", err)
	}
}
