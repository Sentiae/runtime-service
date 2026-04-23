// Package usecase — TestRunnerDispatch picks the command line and VM
// profile used to execute a test run based on (language, test type).
//
// The table is intentionally flat and exhaustive rather than
// heuristic-driven: adding a new stack means adding one row, not
// touching an if/else chain. Callers that don't know the type ask for
// TestTypeUnit and get the language's default unit-test runner.
package usecase

import (
	"fmt"

	"github.com/sentiae/runtime-service/internal/domain"
)

// TestRunnerProfile describes what the runtime should do to execute a
// classified test. Command is a shell template that the execution
// layer renders against the workspace root; VMProfile selects pool
// sizing ("small" for unit, "xlarge-gpu" for visual regression, etc).
type TestRunnerProfile struct {
	Command   string
	VMProfile string
	Network   bool // whether the VM needs outbound network access
	// EnvVars pin additional environment for the runner VM. Populated
	// by middleware layers (§8.3 db-provisioning) without mutating the
	// source profile table.
	EnvVars map[string]string
}

// defaultUnit is returned when no more specific row matches.
var defaultUnit = TestRunnerProfile{
	Command:   "echo 'no runner configured — override via API' && exit 2",
	VMProfile: "small",
	Network:   false,
}

// testRunnerTable is the source of truth. Keys are (language, type).
// Adding language support is a pure append; the dispatch function
// falls back to the unit row when a requested type is missing for a
// language so mismatches are surfaced to the caller but don't crash.
var testRunnerTable = map[string]map[domain.TestType]TestRunnerProfile{
	"go": {
		domain.TestTypeUnit:        {Command: "go test ./...", VMProfile: "small", Network: false},
		domain.TestTypeIntegration: {Command: "go test -tags=integration ./...", VMProfile: "medium", Network: true},
		domain.TestTypeE2E:         {Command: "go test -tags=e2e ./e2e/...", VMProfile: "large", Network: true},
		domain.TestTypePerformance: {Command: "go test -bench=. -benchmem ./...", VMProfile: "large", Network: false},
		domain.TestTypeSecurity:    {Command: "gosec -fmt=json ./...", VMProfile: "small", Network: false},
		domain.TestTypeContract:    {Command: "go test -tags=contract ./...", VMProfile: "small", Network: true},
	},
	"python": {
		domain.TestTypeUnit:        {Command: "pytest -q", VMProfile: "small"},
		domain.TestTypeIntegration: {Command: "pytest -q -m integration", VMProfile: "medium", Network: true},
		domain.TestTypeE2E:         {Command: "pytest -q -m e2e tests/e2e", VMProfile: "large", Network: true},
		domain.TestTypePerformance: {Command: "pytest --benchmark-only", VMProfile: "large"},
		domain.TestTypeSecurity:    {Command: "bandit -r . -f json", VMProfile: "small"},
		domain.TestTypeContract:    {Command: "schemathesis run --checks all openapi.json", VMProfile: "small", Network: true},
	},
	"typescript": {
		domain.TestTypeUnit:        {Command: "pnpm vitest run", VMProfile: "small"},
		domain.TestTypeIntegration: {Command: "pnpm vitest run --dir test/integration", VMProfile: "medium", Network: true},
		domain.TestTypeE2E:         {Command: "pnpm playwright test", VMProfile: "large", Network: true},
		domain.TestTypePerformance: {Command: "pnpm bench", VMProfile: "large"},
		domain.TestTypeSecurity:    {Command: "pnpm audit --json", VMProfile: "small", Network: true},
		domain.TestTypeContract:    {Command: "pnpm dredd", VMProfile: "small", Network: true},
		domain.TestTypeVisual:      {Command: "pnpm playwright test --project=visual", VMProfile: "large", Network: true},
		domain.TestTypeA11y:        {Command: "pnpm playwright test --project=a11y", VMProfile: "large", Network: true},
	},
	"javascript": {
		domain.TestTypeUnit:        {Command: "pnpm vitest run", VMProfile: "small"},
		domain.TestTypeIntegration: {Command: "pnpm vitest run --dir test/integration", VMProfile: "medium", Network: true},
		domain.TestTypeE2E:         {Command: "pnpm playwright test", VMProfile: "large", Network: true},
	},
	"rust": {
		domain.TestTypeUnit:        {Command: "cargo test --lib", VMProfile: "medium"},
		domain.TestTypeIntegration: {Command: "cargo test --test '*'", VMProfile: "medium", Network: true},
		domain.TestTypePerformance: {Command: "cargo bench", VMProfile: "large"},
	},
	"java": {
		domain.TestTypeUnit:        {Command: "mvn test", VMProfile: "medium"},
		domain.TestTypeIntegration: {Command: "mvn verify -P integration", VMProfile: "large", Network: true},
		domain.TestTypePerformance: {Command: "mvn gatling:test", VMProfile: "large", Network: true},
	},
	"ruby": {
		domain.TestTypeUnit:        {Command: "bundle exec rspec", VMProfile: "small"},
		domain.TestTypeIntegration: {Command: "bundle exec rspec spec/integration", VMProfile: "medium", Network: true},
		domain.TestTypeE2E:         {Command: "bundle exec cucumber", VMProfile: "large", Network: true},
	},
}

// ResolveTestRunner returns the profile for the given language+type.
// Falls back (in order): exact match → language's unit row → defaultUnit.
// The returned `matched` flag tells callers whether the dispatcher used
// the requested row or fell back, so orchestrators can surface "ran as
// unit because no $lang/$type runner exists" back to the user.
func ResolveTestRunner(language domain.Language, typ domain.TestType) (profile TestRunnerProfile, matched bool) {
	typ = typ.Normalize()
	row, ok := testRunnerTable[string(language)]
	if !ok {
		return defaultUnit, false
	}
	if p, ok := row[typ]; ok {
		return p, true
	}
	if p, ok := row[domain.TestTypeUnit]; ok {
		return p, false
	}
	return defaultUnit, false
}

// DescribeRunner formats the profile for log lines / UI badges.
func DescribeRunner(p TestRunnerProfile) string {
	net := "no-net"
	if p.Network {
		net = "net"
	}
	return fmt.Sprintf("%s [%s/%s]", p.Command, p.VMProfile, net)
}
