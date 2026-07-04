package usecase

import (
	"context"

	"github.com/sentiae/runtime-service/internal/domain"
)

// WarmCodeRunner runs code on a fast warm CLONE of a pre-snapshotted template
// VM (~160ms) instead of a single-shot cold boot (~13s). It is an outbound port
// implemented in the firecracker adapter (WarmPool). The GraphExecutionEngine
// uses it for code nodes when wired; when nil the engine falls back to the cold
// single-shot ExecuteSync path. The clone has no per-execution Execution record,
// so there is no execID to return — only the run result.
type WarmCodeRunner interface {
	RunCode(ctx context.Context, language domain.Language, code, stdin string) (WarmRunResult, error)
}

// WarmRunResult is the flattened result of a warm-clone run: the guest-agent's
// stdout / stderr / exit code. It mirrors the cold-path Execution fields the
// output-shaping helper consumes, so warm and cold produce identical node output.
type WarmRunResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}
