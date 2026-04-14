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
)

// IsValid reports whether the value is one of the known test types.
func (t TestType) IsValid() bool {
	switch t {
	case TestTypeUnit, TestTypeIntegration, TestTypeE2E,
		TestTypePerformance, TestTypeSecurity, TestTypeContract,
		TestTypeVisual, TestTypeA11y:
		return true
	}
	return false
}

// Normalize returns the canonical value, defaulting empty/invalid
// inputs to TestTypeUnit. Callers use this when ingesting a type from
// untrusted sources (HTTP body, event payload).
func (t TestType) Normalize() TestType {
	if t.IsValid() {
		return t
	}
	return TestTypeUnit
}
