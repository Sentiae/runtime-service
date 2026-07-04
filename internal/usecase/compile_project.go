package usecase

import (
	"context"
	"fmt"

	"github.com/sentiae/runtime-service/internal/domain"
)

// ProjectCompiler is the port for compiling a multi-file project and
// returning structured diagnostics. The implementing adapter builds the
// file set in an ephemeral build container and never executes the result.
type ProjectCompiler interface {
	// Compile builds the given file set for language. timeoutSec bounds the
	// whole build; zero lets the adapter pick a default. A non-nil result
	// with OK=false represents a clean compile failure (diagnostics
	// populated); a non-nil error represents an infrastructure failure
	// (e.g. domain.ErrCompileToolchainUnavailable).
	Compile(ctx context.Context, language string, files []domain.SourceFile, timeoutSec int) (*domain.CompileResult, error)
}

// CompileProjectInput is the wire-agnostic input for the compile use case.
type CompileProjectInput struct {
	Language   string
	Files      []domain.SourceFile
	TimeoutSec int
}

// CompileProjectOutput wraps the compile result.
type CompileProjectOutput struct {
	Result *domain.CompileResult
}

// CompileProject validates a compile request and delegates to the injected
// ProjectCompiler. It owns no build logic itself — the backend lives in
// the adapter.
type CompileProject struct {
	compiler ProjectCompiler
}

// NewCompileProject constructs the compile use case.
func NewCompileProject(compiler ProjectCompiler) *CompileProject {
	return &CompileProject{compiler: compiler}
}

// Execute validates the request and runs the compile.
func (uc *CompileProject) Execute(ctx context.Context, in CompileProjectInput) (CompileProjectOutput, error) {
	if uc.compiler == nil {
		return CompileProjectOutput{}, domain.ErrCompileToolchainUnavailable
	}
	if len(in.Files) == 0 {
		return CompileProjectOutput{}, domain.ErrNoSourceFiles
	}
	if !compileLanguageSupported(in.Language) {
		return CompileProjectOutput{}, domain.ErrUnsupportedCompileLanguage
	}

	result, err := uc.compiler.Compile(ctx, in.Language, in.Files, in.TimeoutSec)
	if err != nil {
		return CompileProjectOutput{}, fmt.Errorf("compile project: %w", err)
	}
	return CompileProjectOutput{Result: result}, nil
}

// compileLanguageSupported reports whether the compile path has a toolchain
// for the language. Kept in the use case so the empty/unsupported request
// is rejected before touching the build backend; the adapter enforces the
// same set as a defence in depth.
func compileLanguageSupported(language string) bool {
	switch language {
	case "go", "golang", "typescript", "ts":
		return true
	}
	return false
}
