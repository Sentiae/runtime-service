package domain

// TestType categorizes a test run so the runtime can pick the right
// executor (unit vs perf vs visual-regression use different binaries
// and sandboxing policies). The values intentionally mirror the
// canonical set in ops-service so runs originating anywhere in the
// platform share one vocabulary.
type TestType string

const (
	TestTypeUnit        TestType = "unit"
	TestTypeIntegration TestType = "integration"
	TestTypeE2E         TestType = "e2e"
	TestTypePerformance TestType = "performance"
	TestTypeSecurity    TestType = "security"
	TestTypeContract    TestType = "contract"
	TestTypeVisual      TestType = "visual"
	TestTypeA11y        TestType = "accessibility"

	// CS-2 G2.5 — extended test types. These sit alongside the
	// original Performance / Visual / A11y values because external
	// callers (CI pipelines, CLI) use the more idiomatic names
	// "perf", "visual_regression", "a11y", "load", "contract" and we
	// want the enum to accept them without normalizing silently.
	TestTypePerf             TestType = "perf"
	TestTypeLoad             TestType = "load"
	TestTypeVisualRegression TestType = "visual_regression"
	TestTypeAccessibility    TestType = "a11y"
)

// IsValid reports whether the value is one of the known test types.
func (t TestType) IsValid() bool {
	switch t {
	case TestTypeUnit, TestTypeIntegration, TestTypeE2E,
		TestTypePerformance, TestTypeSecurity, TestTypeContract,
		TestTypeVisual, TestTypeA11y,
		// CS-2 G2.5 extensions.
		TestTypePerf, TestTypeLoad, TestTypeVisualRegression, TestTypeAccessibility:
		return true
	}
	return false
}

// Normalize returns the canonical value, defaulting empty/invalid
// inputs to TestTypeUnit. Callers use this when ingesting a type from
// untrusted sources (HTTP body, event payload).
//
// Aliases (e.g. "perf" → "performance", "visual_regression" → "visual",
// "a11y" → "accessibility") are collapsed to their canonical form so
// downstream dispatch tables stay small. "load" stays distinct because
// it has its own executor semantics (sustained VU pattern, no p99
// assertion).
func (t TestType) Normalize() TestType {
	switch t {
	case TestTypePerf:
		return TestTypePerformance
	case TestTypeVisualRegression:
		return TestTypeVisual
	case TestTypeAccessibility:
		return TestTypeA11y
	}
	if t.IsValid() {
		return t
	}
	return TestTypeUnit
}
