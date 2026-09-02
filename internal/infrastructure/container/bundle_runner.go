package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sentiae/platform-kit/nodeabi"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/sentiae/runtime-service/pkg/config"
)

// nodeMountTarget is where a node mounts its invocation directory, read-only.
// It is the parent of nodeabi.BrokerSocketPath, and it is the ONLY thing a
// bundle ever has mounted.
const nodeMountTarget = "/run/sentiae"

// dockerFn runs one `docker <args…>` to completion. It exists so the argument
// list, the credential envelope and the failure paths are all testable without
// a daemon: the real one is dockerCLI.
type dockerFn func(ctx context.Context, env []string, stdin []byte, args ...string) (stdout, stderr string, exitCode int, err error)

// BundleRunner pulls and runs published node bundles BY DIGEST in the same
// hardened sandbox the language runner uses.
//
// Registry credentials never persist: every operation that needs them gets its
// own DOCKER_CONFIG directory, and that directory is logged out of and removed
// on success AND on failure. A shared credential store would leave the service
// API key on disk for anything in this container to read.
type BundleRunner struct {
	cfg      config.NodeRunnerConfig
	password string
	// self identifies this container to `docker inspect` for the mount check.
	self   string
	docker dockerFn
}

var _ usecase.BundleRunner = (*BundleRunner)(nil)

// NewBundleRunner builds the runner. password is the registry pull credential
// (the service API key — one credential, one source).
func NewBundleRunner(cfg config.NodeRunnerConfig, password string) (*BundleRunner, error) {
	self, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("node runner: read own hostname: %w", err)
	}
	return &BundleRunner{cfg: cfg, password: password, self: self, docker: dockerCLI}, nil
}

// Probe refuses to let the service serve node runs it could not actually
// perform: the registry credential must work, the runs volume must really be
// mounted where this process expects it, and anything left in the runs
// directory by a previous process is swept before the first invocation.
func (r *BundleRunner) Probe(ctx context.Context) error {
	if err := r.withRegistryAuth(ctx, func(context.Context, []string) error { return nil }); err != nil {
		return err
	}
	if err := r.checkRunsVolume(ctx); err != nil {
		return err
	}
	return r.sweepRunsDir()
}

// Pull fetches a bundle image. A reference without a digest is REFUSED rather
// than resolved: a tag can be moved after a graph is compiled, so pulling one
// would run bytes nobody pinned.
func (r *BundleRunner) Pull(ctx context.Context, image string) error {
	if !strings.Contains(image, "@sha256:") {
		return fmt.Errorf("%w: %s is not digest-pinned", domain.ErrBundlePullFailed, image)
	}
	pullCtx := ctx
	if r.cfg.PullTimeout > 0 {
		var cancel context.CancelFunc
		pullCtx, cancel = context.WithTimeout(ctx, r.cfg.PullTimeout)
		defer cancel()
	}
	return r.withRegistryAuth(pullCtx, func(ctx context.Context, env []string) error {
		_, stderr, code, err := r.docker(ctx, env, nil, "pull", image)
		if err != nil || code != 0 {
			return fmt.Errorf("%w: %s: %s", domain.ErrBundlePullFailed, image, failureText(stderr, err))
		}
		return nil
	})
}

// Run launches one bundle. The CALL goes in on stdin — never an argument, an
// environment variable or a file — and the image runs its OWN command, because
// what a bundle executes is the bundle's business.
func (r *BundleRunner) Run(ctx context.Context, launch usecase.BundleLaunch) (usecase.BundleRunResult, error) {
	name := nodeContainerName(launch.InvocationID)
	args := bundleRunArgs(launch, r.cfg.RunsVolume)

	timeout := time.Duration(launch.TimeoutSec) * time.Second
	execCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	started := time.Now()
	cmd := exec.CommandContext(execCtx, "docker", args...)
	cmd.Stdin = bytes.NewReader(launch.Call)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = nodeabi.NewCappedWriter(&stdout, nodeabi.MaxStdoutBytes)
	cmd.Stderr = nodeabi.NewCappedWriter(&stderr, nodeabi.MaxStderrBytes)
	// Killing the docker CLI does not stop the container it started, so
	// cancellation removes the container itself; WaitDelay bounds how long the
	// pipes may keep the process alive after that.
	cmd.Cancel = func() error {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return exec.CommandContext(rmCtx, "docker", "rm", "-f", name).Run()
	}
	cmd.WaitDelay = 5 * time.Second

	runErr := cmd.Run()
	result := usecase.BundleRunResult{
		Stdout:     stdout.Bytes(),
		Stderr:     stderr.String(),
		ExecTimeMS: time.Since(started).Milliseconds(),
	}
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
		return result, nil
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, fmt.Errorf("node runner: docker run %s: %w", name, runErr)
	}
	return result, nil
}

// nodeContainerName is the one place a node container's name is spelled.
func nodeContainerName(invocationID string) string { return "sentiae-node-" + invocationID }

// bundleRunArgs builds the launch line. It is pure so the exact flags — both
// labels, the read-only broker mount, the network, and the absence of a
// trailing command — are asserted without a daemon.
func bundleRunArgs(launch usecase.BundleLaunch, runsVolume string) []string {
	memMB := launch.MemoryMB
	if memMB <= 0 {
		memMB = defaultContainerMemMB
	}

	args := []string{
		"run", "--rm", "-i",
		"--name", nodeContainerName(launch.InvocationID),
		"--label", "sentiae.node.invocation=" + launch.InvocationID,
		"--label", "sentiae.node.run=" + launch.RunID.String(),
	}
	args = append(args, hardenedFlags(memMB, defaultContainerVCPU)...)
	if launch.BrokerSubpath != "" {
		args = append(args, "--mount",
			"type=volume,source="+runsVolume+",target="+nodeMountTarget+",readonly,volume-subpath="+launch.BrokerSubpath)
	}
	network := "none"
	if launch.Network != "" {
		network = launch.Network
	}
	args = append(args, "--network", network)
	// The image and NOTHING after it: the recipes carry no ENTRYPOINT, so the
	// image's own CMD ["/node/run"] is what must execute.
	return append(args, launch.Image)
}

// withRegistryAuth runs op with a registry login that exists only for the
// duration of the operation. The logout and the removal are unconditional:
// leaving the credential file behind on a failure path is the exact case a
// per-operation store exists to prevent.
func (r *BundleRunner) withRegistryAuth(ctx context.Context, op func(context.Context, []string) error) (err error) {
	dir, err := os.MkdirTemp("", "sentiae-node-registry-")
	if err != nil {
		return fmt.Errorf("node runner: create registry config dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("node runner: restrict registry config dir: %w", err)
	}
	env := append(os.Environ(), "DOCKER_CONFIG="+dir)

	defer func() {
		_, _, _, _ = r.docker(context.WithoutCancel(ctx), env, nil, "logout", r.cfg.RegistryHost)
		if rmErr := os.RemoveAll(dir); rmErr != nil && err == nil {
			err = fmt.Errorf("node runner: remove registry config dir: %w", rmErr)
			return
		}
		if _, statErr := os.Stat(dir); statErr == nil && err == nil {
			err = fmt.Errorf("node runner: registry config dir %s survived", dir)
		}
	}()

	_, stderr, code, loginErr := r.docker(ctx, env, []byte(r.password), "login",
		"--username", r.cfg.RegistryUser, "--password-stdin", r.cfg.RegistryHost)
	if loginErr != nil || code != 0 {
		return fmt.Errorf("node runner: docker login %s failed: %s", r.cfg.RegistryHost, failureText(stderr, loginErr))
	}
	return op(ctx, env)
}

// checkRunsVolume proves the broker socket directory this process hands to
// sidecars is the same one the node containers will mount. A mismatch would
// mean every secret-declaring node waits on a socket nobody serves.
func (r *BundleRunner) checkRunsVolume(ctx context.Context) error {
	stdout, _, code, err := r.docker(ctx, nil, nil, "inspect", r.self,
		"--format", "{{range .Mounts}}{{.Name}}={{.Destination}}\n{{end}}")
	if err != nil || code != 0 {
		return fmt.Errorf("node runner: volume %s is not mounted at %s on this container",
			r.cfg.RunsVolume, r.cfg.RunsDir)
	}
	want := r.cfg.RunsVolume + "=" + r.cfg.RunsDir
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == want {
			return nil
		}
	}
	return fmt.Errorf("node runner: volume %s is not mounted at %s on this container",
		r.cfg.RunsVolume, r.cfg.RunsDir)
}

// sweepRunsDir removes invocation directories a previous process left behind.
// They can only be orphans: an invocation directory outlives nothing.
func (r *BundleRunner) sweepRunsDir() error {
	entries, err := os.ReadDir(r.cfg.RunsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("node runner: read runs dir %s: %w", r.cfg.RunsDir, err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(r.cfg.RunsDir, entry.Name())); err != nil {
			return fmt.Errorf("node runner: sweep runs dir %s: %w", r.cfg.RunsDir, err)
		}
	}
	return nil
}

// dockerCLI is the real docker invocation.
func dockerCLI(ctx context.Context, env []string, stdin []byte, args ...string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if env != nil {
		cmd.Env = env
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			err = nil
		}
	}
	return stdout.String(), stderr.String(), exitCode, err
}

// failureText prefers what docker said over what os/exec said: the daemon's
// message names the fault, the exit status only reports that there was one.
func failureText(stderr string, err error) string {
	trimmed := strings.TrimSpace(stderr)
	if trimmed != "" {
		return trimmed
	}
	if err != nil {
		return err.Error()
	}
	return "exit status non-zero"
}
