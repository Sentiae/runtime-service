package usecase

import (
	"context"

	"github.com/sentiae/runtime-service/internal/domain"
)

// TestRunDispatcherIface is the narrow surface consumed by the
// multi-type router. Every per-type executor + the fallback
// FirecrackerTestRunDispatcher satisfies this shape.
type TestRunDispatcherIface interface {
	DispatchInVM(ctx context.Context, run *domain.TestRun, profile TestRunnerProfile) error
}

// MultiTypeDispatcher routes a test run to the per-type executor
// registered for TestRun.TestType. Perf / security / contract flavours
// have bespoke setup (k6, trivy, pact-verifier) and need type-specific
// output parsing; everything else falls through to the generic
// Firecracker dispatcher which just packages the profile command
// into an ExecutionUseCase call.
type MultiTypeDispatcher struct {
	perf     TestRunDispatcherIface
	security TestRunDispatcherIface
	contract TestRunDispatcherIface
	visual   TestRunDispatcherIface
	a11y     TestRunDispatcherIface
	fallback TestRunDispatcherIface
}

// NewMultiTypeDispatcher wires the per-type router. Any nil executor
// is treated as "not configured" and routes that type through the
// fallback.
func NewMultiTypeDispatcher(perf, security, contract, fallback TestRunDispatcherIface) *MultiTypeDispatcher {
	return &MultiTypeDispatcher{perf: perf, security: security, contract: contract, fallback: fallback}
}

// WithVisualExecutor wires the visual-regression executor (Playwright +
// visual-diff plugin). §8.4. Returns the same receiver so DI reads
// linearly. Passing nil leaves the dispatcher in "no visual executor"
// mode; visual runs then fall through to the generic dispatcher.
func (d *MultiTypeDispatcher) WithVisualExecutor(visual TestRunDispatcherIface) *MultiTypeDispatcher {
	d.visual = visual
	return d
}

// WithA11yExecutor wires the accessibility executor (axe-core). §8.4.
// Mirrors WithVisualExecutor semantics.
func (d *MultiTypeDispatcher) WithA11yExecutor(a11y TestRunDispatcherIface) *MultiTypeDispatcher {
	d.a11y = a11y
	return d
}

// DispatchInVM routes to the appropriate executor based on run.TestType.
// Both canonical and alias test-type labels (visual/visual_regression,
// accessibility/a11y) funnel to the same per-type executor.
func (d *MultiTypeDispatcher) DispatchInVM(ctx context.Context, run *domain.TestRun, profile TestRunnerProfile) error {
	switch run.TestType {
	case domain.TestTypePerformance, domain.TestTypePerf, domain.TestTypeLoad:
		if d.perf != nil {
			return d.perf.DispatchInVM(ctx, run, profile)
		}
	case domain.TestTypeSecurity:
		if d.security != nil {
			return d.security.DispatchInVM(ctx, run, profile)
		}
	case domain.TestTypeContract:
		if d.contract != nil {
			return d.contract.DispatchInVM(ctx, run, profile)
		}
	case domain.TestTypeVisual, domain.TestTypeVisualRegression:
		if d.visual != nil {
			return d.visual.DispatchInVM(ctx, run, profile)
		}
	case domain.TestTypeA11y, domain.TestTypeAccessibility:
		if d.a11y != nil {
			return d.a11y.DispatchInVM(ctx, run, profile)
		}
	}
	if d.fallback != nil {
		return d.fallback.DispatchInVM(ctx, run, profile)
	}
	return nil
}
