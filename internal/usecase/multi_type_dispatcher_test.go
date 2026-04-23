package usecase

import (
	"context"
	"testing"

	"github.com/sentiae/runtime-service/internal/domain"
)

type trackingDispatcher struct {
	called int
	label  string
	record *[]string
}

func (t *trackingDispatcher) DispatchInVM(ctx context.Context, run *domain.TestRun, profile TestRunnerProfile) error {
	t.called++
	if t.record != nil {
		*t.record = append(*t.record, t.label)
	}
	return nil
}

func TestMultiTypeDispatcher_RoutesByType(t *testing.T) {
	var trace []string
	perf := &trackingDispatcher{label: "perf", record: &trace}
	sec := &trackingDispatcher{label: "security", record: &trace}
	contract := &trackingDispatcher{label: "contract", record: &trace}
	fallback := &trackingDispatcher{label: "fallback", record: &trace}
	d := NewMultiTypeDispatcher(perf, sec, contract, fallback)

	cases := []struct {
		typ   domain.TestType
		label string
	}{
		{domain.TestTypePerformance, "perf"},
		{domain.TestTypeSecurity, "security"},
		{domain.TestTypeContract, "contract"},
		{domain.TestTypeUnit, "fallback"},
		{domain.TestTypeE2E, "fallback"},
	}

	for _, c := range cases {
		run := &domain.TestRun{TestType: c.typ}
		if err := d.DispatchInVM(context.Background(), run, TestRunnerProfile{}); err != nil {
			t.Fatalf("dispatch %s: %v", c.typ, err)
		}
	}

	if len(trace) != len(cases) {
		t.Fatalf("trace length mismatch: got %d want %d", len(trace), len(cases))
	}
	for i, want := range []string{"perf", "security", "contract", "fallback", "fallback"} {
		if trace[i] != want {
			t.Fatalf("trace[%d]=%s want %s", i, trace[i], want)
		}
	}
}

// TestMultiTypeDispatcher_RoutesVisualAndA11y asserts §8.4 extended
// routing: visual + accessibility executors, when wired via the
// With*Executor setters, catch their respective test types (including
// canonical + alias labels). Unwired types fall through to the
// fallback.
func TestMultiTypeDispatcher_RoutesVisualAndA11y(t *testing.T) {
	var trace []string
	visual := &trackingDispatcher{label: "visual", record: &trace}
	a11y := &trackingDispatcher{label: "a11y", record: &trace}
	fallback := &trackingDispatcher{label: "fallback", record: &trace}
	d := NewMultiTypeDispatcher(nil, nil, nil, fallback).
		WithVisualExecutor(visual).
		WithA11yExecutor(a11y)

	cases := []struct {
		typ   domain.TestType
		label string
	}{
		{domain.TestTypeVisual, "visual"},
		{domain.TestTypeVisualRegression, "visual"},
		{domain.TestTypeA11y, "a11y"},
		{domain.TestTypeAccessibility, "a11y"},
	}
	for _, c := range cases {
		run := &domain.TestRun{TestType: c.typ}
		if err := d.DispatchInVM(context.Background(), run, TestRunnerProfile{}); err != nil {
			t.Fatalf("dispatch %s: %v", c.typ, err)
		}
	}
	if len(trace) != len(cases) {
		t.Fatalf("trace length mismatch: got %d want %d", len(trace), len(cases))
	}
	for i, want := range []string{"visual", "visual", "a11y", "a11y"} {
		if trace[i] != want {
			t.Fatalf("trace[%d]=%s want %s", i, trace[i], want)
		}
	}
}

// TestMultiTypeDispatcher_VisualFallsThroughWhenUnwired confirms the
// safe-default posture: a visual run dispatched against a dispatcher
// without a wired visual executor falls through to the generic
// fallback instead of erroring.
func TestMultiTypeDispatcher_VisualFallsThroughWhenUnwired(t *testing.T) {
	var trace []string
	fallback := &trackingDispatcher{label: "fallback", record: &trace}
	d := NewMultiTypeDispatcher(nil, nil, nil, fallback)
	run := &domain.TestRun{TestType: domain.TestTypeVisual}
	if err := d.DispatchInVM(context.Background(), run, TestRunnerProfile{}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(trace) != 1 || trace[0] != "fallback" {
		t.Fatalf("visual should fall through to fallback; trace=%v", trace)
	}
}

func TestMultiTypeDispatcher_NilFallbackNoop(t *testing.T) {
	d := NewMultiTypeDispatcher(nil, nil, nil, nil)
	run := &domain.TestRun{TestType: domain.TestTypeUnit}
	if err := d.DispatchInVM(context.Background(), run, TestRunnerProfile{}); err != nil {
		t.Fatalf("nil fallback should noop, got %v", err)
	}
}

func TestDBProvisioningMiddleware_InjectsURL(t *testing.T) {
	var captured TestRunnerProfile
	inner := trackingDispatcher{label: "inner"}
	innerWrap := &captureWrap{inner: &inner, capture: &captured}
	cleanupCalled := false
	prov := ProvisionerFunc(func(ctx context.Context, run *domain.TestRun) (*DBLease, error) {
		return &DBLease{URL: "postgres://test", Cleanup: func() { cleanupCalled = true }}, nil
	})
	mw := NewDBProvisioningMiddleware(innerWrap, prov)

	run := &domain.TestRun{DBMode: domain.TestDBModeEphemeralPg}
	if err := mw.DispatchInVM(context.Background(), run, TestRunnerProfile{}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if captured.EnvVars["DATABASE_URL"] != "postgres://test" {
		t.Fatalf("DATABASE_URL not injected: %+v", captured.EnvVars)
	}
	if !cleanupCalled {
		t.Fatalf("cleanup should be invoked on completion")
	}
}

func TestDBProvisioningMiddleware_SkipsWhenNoneMode(t *testing.T) {
	inner := trackingDispatcher{label: "inner"}
	mw := NewDBProvisioningMiddleware(&inner, ProvisionerFunc(func(ctx context.Context, run *domain.TestRun) (*DBLease, error) {
		t.Fatalf("provisioner must not be called for DBModeNone")
		return nil, nil
	}))
	if err := mw.DispatchInVM(context.Background(), &domain.TestRun{DBMode: domain.TestDBModeNone}, TestRunnerProfile{}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if inner.called != 1 {
		t.Fatalf("inner dispatcher must run once")
	}
}

type captureWrap struct {
	inner   TestRunDispatcherIface
	capture *TestRunnerProfile
}

func (c *captureWrap) DispatchInVM(ctx context.Context, run *domain.TestRun, profile TestRunnerProfile) error {
	*c.capture = profile
	return c.inner.DispatchInVM(ctx, run, profile)
}
