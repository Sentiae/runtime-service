package usecase

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/domain"
)

// BundleLaunch is one sandboxed run of a built node bundle.
type BundleLaunch struct {
	InvocationID string
	Image        string
	// Call is the ABI call document handed to the bundle on stdin.
	Call       []byte
	MemoryMB   int
	TimeoutSec int
	// BrokerSubpath is the invocation subpath the node mounts read-only to reach
	// its broker socket. "" ⇒ no /run/sentiae mount at all.
	BrokerSubpath string
	// Network is the invocation bridge to join. "" ⇒ --network none.
	Network string
}

// BundleRunResult is what the sandbox produced.
type BundleRunResult struct {
	Stdout     []byte
	Stderr     string
	ExitCode   int
	TimedOut   bool
	ExecTimeMS int64
}

// BundleRunner runs a published bundle by digest in a hardened sandbox.
type BundleRunner interface {
	Probe(ctx context.Context) error
	Pull(ctx context.Context, image string) error
	Run(ctx context.Context, launch BundleLaunch) (BundleRunResult, error)
}

// SecretAnswer is one resolved secret as it travels to the sidecar: the handle
// the node redeems, and the value only the sidecar ever holds.
type SecretAnswer struct {
	Handle string
	Found  bool
	Value  string
}

// SidecarBinding is the ONE document the runtime hands a sidecar on its
// attached stdin. It is never an argument, an environment variable, a file, or
// a log line — T4.13 and §9.6 prove it.
type SidecarBinding struct {
	Invocation string `json:"invocation"`
	Run        string `json:"run"`
	Node       string `json:"node"`
	// Secrets is keyed by declared secret name; empty when the node declares none.
	Secrets map[string]SecretAnswer `json:"secrets"`
	// Egress nil ⇒ no proxy and no bridge for this invocation.
	Egress *EgressBinding `json:"egress,omitempty"`
}

// EgressBinding is the proxy half of a binding: what the node may reach, the
// bearer token its requests must carry, and the invocation subnet the proxy
// binds inside.
type EgressBinding struct {
	Patterns []string `json:"patterns"`
	Token    string   `json:"token"`
	Subnet   string   `json:"subnet"`
}

// SidecarOpen is the request to stand up one invocation's sidecar.
type SidecarOpen struct {
	InvocationID string
	RunID        uuid.UUID
	Node         string
	Binding      SidecarBinding
}

// Sidecar is what an opened sidecar exposes to the node it serves.
type Sidecar struct {
	// BrokerSubpath is the invocation subpath the node mounts read-only.
	BrokerSubpath string
	// Network is the invocation bridge; "" when the node declared no egress.
	Network string
	// ProxyURL is "http://proxy:3128" when egress is bound, else "".
	ProxyURL string
}

// SidecarManager owns the per-invocation sidecar lifecycle: one trusted
// container per invocation that declares secrets OR egress, with its own
// --internal bridge only when egress is declared.
type SidecarManager interface {
	Probe(ctx context.Context) error
	Open(ctx context.Context, in SidecarOpen) (Sidecar, error)
	Close(ctx context.Context, invocationID string) error
	SweepRun(ctx context.Context, runID uuid.UUID) error
	// SweepAll returns what REMAINS after re-enumeration, never what it removed.
	SweepAll(ctx context.Context) (containers, networks int, err error)
}

// SecretValueSource resolves a declared secret's value for one organization
// under a handed token, and revokes that token when the run ends.
type SecretValueSource interface {
	Resolve(ctx context.Context, org uuid.UUID, handedToken, environment, name string) (value string, found bool, err error)
	Revoke(ctx context.Context, handedToken string) error
}

// Clock is the injected time source (CLAUDE.md §30.6).
type Clock interface{ Now() time.Time }

// SystemClock is the real one.
type SystemClock struct{}

// Now returns the current UTC time.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// NotConfiguredBundleRunner is what the container holds until the real runner
// is wired. It fails CLOSED on every method: an unconfigured node runner must
// refuse the invocation, never fall through to some other execution path.
type NotConfiguredBundleRunner struct{}

// Probe refuses.
func (NotConfiguredBundleRunner) Probe(context.Context) error { return domain.ErrNodeRunnerNotReady }

// Pull refuses.
func (NotConfiguredBundleRunner) Pull(context.Context, string) error {
	return domain.ErrNodeRunnerNotReady
}

// Run refuses.
func (NotConfiguredBundleRunner) Run(context.Context, BundleLaunch) (BundleRunResult, error) {
	return BundleRunResult{}, domain.ErrNodeRunnerNotReady
}

// NotConfiguredSidecarManager is the SidecarManager the container holds until
// the real one is wired. Same fail-closed rule as the runner.
type NotConfiguredSidecarManager struct{}

// Probe refuses.
func (NotConfiguredSidecarManager) Probe(context.Context) error {
	return domain.ErrNodeRunnerNotReady
}

// Open refuses.
func (NotConfiguredSidecarManager) Open(context.Context, SidecarOpen) (Sidecar, error) {
	return Sidecar{}, domain.ErrNodeRunnerNotReady
}

// Close refuses.
func (NotConfiguredSidecarManager) Close(context.Context, string) error {
	return domain.ErrNodeRunnerNotReady
}

// SweepRun refuses.
func (NotConfiguredSidecarManager) SweepRun(context.Context, uuid.UUID) error {
	return domain.ErrNodeRunnerNotReady
}

// SweepAll refuses.
func (NotConfiguredSidecarManager) SweepAll(context.Context) (int, int, error) {
	return 0, 0, domain.ErrNodeRunnerNotReady
}

var (
	_ BundleRunner   = NotConfiguredBundleRunner{}
	_ SidecarManager = NotConfiguredSidecarManager{}
)
