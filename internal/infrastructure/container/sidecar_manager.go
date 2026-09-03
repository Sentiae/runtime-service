package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/platform-kit/nodebroker"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/sentiae/runtime-service/pkg/config"
)

const (
	// The two labels every node container, sidecar and invocation network
	// carries (D-20). The invocation key is what the BOOT sweep enumerates; the
	// run key is what completion, cancellation and timeout sweep.
	labelInvocation = "sentiae.node.invocation"
	labelRun        = "sentiae.node.run"

	// sidecarMountTarget is where the sidecar mounts its invocation directory.
	// It is deliberately NOT the node's mount target: the node mounts the same
	// subpath READ-ONLY at /run/sentiae, and that asymmetry is the boundary.
	sidecarMountTarget = "/run/sentiae-inv"

	// sidecarBinary is the sidecar's path inside the runtime image. It is
	// supplied as --entrypoint because the runtime image's own ENTRYPOINT is the
	// server: a trailing command would be APPENDED to it and would boot a second
	// runtime-service inside every sidecar (R-23).
	sidecarBinary = "/app/node-sidecar"

	// sidecarHealthURL is polled through `docker exec` because the health
	// listener is bound to the sidecar's loopback and is reachable from nowhere
	// else — not from the invocation bridge, and not from the uplink.
	sidecarHealthURL = "http://127.0.0.1:3129/healthz"

	// sidecarProxyAlias is the DNS name the node resolves the proxy at. It
	// exists only on that invocation's own bridge.
	sidecarProxyAlias = "proxy"

	// readyPollEvery is the readiness poll interval (§3.7 step 6).
	readyPollEvery = 100 * time.Millisecond

	// sweepEvery is the orphan sweeper's period.
	sweepEvery = 60 * time.Second

	// dockerCLIMinMajor is the first docker CLI major that understands
	// `--mount type=volume,…,volume-subpath=`. That flag is load-bearing for
	// isolation — it confines every invocation to its own subdirectory of the
	// runs volume — so a CLI below this floor cannot open a sidecar at all.
	dockerCLIMinMajor = 26

	// sidecarMemMB / sidecarVCPU are the sidecar's hardened resources. The
	// sidecar is TRUSTED code, but it gets the same flags a hostile bundle does:
	// one flag list, one thing to weaken by accident.
	sidecarMemMB = 256
	sidecarVCPU  = 1

	// probeSecretName is the ONE secret the boot probe binds. Its answer is
	// Found:false with an empty value — a credential that redeems nothing, so
	// there is nothing to leak even if every hygiene control failed at once —
	// and its only job is to make len(b.Secrets) > 0 so the sidecar actually
	// starts a broker and nodebroker.Listen actually runs its predicate.
	probeSecretName = "boot-probe"

	// probeRedeemOK is the EXACT line the probe's node-shaped container must
	// print. 200 with no code and found=false is the whole path: the directory
	// was searchable from the node's uid class, the socket was connectable, the
	// invocation matched, the handle was bound, and the answer was the empty one
	// the probe bound. Anything else — a 403, a 409, a dial error, or a found
	// answer — is a different topology and must refuse boot.
	probeRedeemOK = `redeem: status=200 code="" found=false`
)

var (
	// sidecarSetupMS / sidecarTeardownMS are §3.10's lifecycle histograms. The
	// bridge label separates the two shapes of invocation, because an egress
	// invocation pays for a network create and an uplink connect that a
	// secrets-only invocation does not (D-22's cost bound is over the former).
	sidecarSetupMS = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "node_sidecar_setup_ms",
		Help:    "Milliseconds to open one invocation's sidecar, from directory creation to readiness.",
		Buckets: []float64{50, 100, 250, 500, 750, 1000, 1500, 2000, 3000, 5000},
	}, []string{"bridge"})

	sidecarTeardownMS = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "node_sidecar_teardown_ms",
		Help:    "Milliseconds to close one invocation's sidecar, including its bridge and directory.",
		Buckets: []float64{50, 100, 250, 500, 750, 1000, 1500, 2000, 3000, 5000},
	}, []string{"bridge"})
)

// liveInvocation is what the manager remembers about an invocation it opened.
// It is the sweeper's whole safety property: anything labelled that is NOT in
// here is an orphan, and anything in here is work in flight.
type liveInvocation struct {
	run    uuid.UUID
	bridge bool
}

// SidecarManager owns the per-invocation sidecar lifecycle: one trusted
// container for every node that declares secrets OR egress, and — only for
// egress — its own --internal bridge holding exactly that sidecar and that
// node.
//
// It never CREATES the uplink. The uplink is static infrastructure created or
// verified by deploy.sh (R-14/E6), so Probe is a pure verifier: its refusal
// fires only when someone removed or reshaped the uplink out of band, which is
// a signal an operator must see rather than one the runtime should silently
// repair.
type SidecarManager struct {
	cfg  config.NodeRunnerConfig
	pool *usecase.SubnetPool
	// self is this container's id/hostname, used to read our own image.
	self string
	// image is the image sidecars run: the runtime's OWN image, resolved at
	// Probe. There is no second supply chain to trust.
	image  string
	docker dockerFn

	mu   sync.Mutex
	live map[string]liveInvocation
	// sweeperStarted keeps Stop() honest: waiting on a loop that was never
	// started would deadlock shutdown for a service that merely built the
	// manager.
	sweeperStarted bool

	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

var _ usecase.SidecarManager = (*SidecarManager)(nil)

// NewSidecarManager builds the manager. The subnet pool is shared with the
// invoker: the pool is the only thing that keeps two live invocations off the
// same address range.
func NewSidecarManager(cfg config.NodeRunnerConfig, pool *usecase.SubnetPool) (*SidecarManager, error) {
	self, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("node runner: read own hostname: %w", err)
	}
	return &SidecarManager{
		cfg:    cfg,
		pool:   pool,
		self:   self,
		docker: dockerCLI,
		live:   map[string]liveInvocation{},
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}, nil
}

// Probe refuses to let this process serve node runs it could not isolate.
//
// Order matters, and every edge below is load-bearing (R-30):
//
//   - the docker CLI is checked FIRST (R-27), because it issues every other
//     line here: a CLI that cannot parse the sidecar launch invalidates every
//     answer any later step could read;
//   - the uplink SECOND (R-14). It is deploy-owned static infrastructure that
//     this process only verifies and never repairs, so it is proven before the
//     runtime mutates anything — and the sweep provably cannot disturb it,
//     because the uplink carries sentiae.node.uplink=1 and never
//     sentiae.node.invocation, which is the only label the sweep selects on;
//   - the orphan sweep THIRD, and this is the edge R-30 fixes. Every invocation
//     bridge is carved out of invocation_cidr by construction (subnet_pool.go),
//     so a verifier placed ahead of the sweep would see the runtime's OWN
//     leaked bridge, refuse on it, and never reach the one step that could
//     remove it: a transient crash became an outage no restart could end. The
//     survivor refusal sits with the sweep, because refusing is only legal
//     after removal was attempted;
//   - the address space FOURTH (R-25), against every network still present on
//     the post-sweep daemon — none exempt by label, since a survivor still
//     occupies the pool and a network appearing after the sweep must not slip
//     past the boot guard;
//   - one FULL egress cycle LAST (R-24), because it registers the first live
//     invocation — it must run on a daemon already proven clean, and the
//     failure it catches is a topology that looks right and does not work.
func (m *SidecarManager) Probe(ctx context.Context) error {
	if err := m.verifyDockerCLI(ctx); err != nil {
		return err
	}
	if err := m.verifyUplink(ctx); err != nil {
		return err
	}
	containers, networks, err := m.SweepAll(ctx)
	if err != nil {
		return err
	}
	if containers > 0 || networks > 0 {
		return fmt.Errorf("node runner: orphan sweep incomplete: %d container(s), %d network(s) remain",
			containers, networks)
	}
	if err := m.verifyInvocationCIDR(ctx); err != nil {
		return err
	}
	if err := m.bootProbeCycle(ctx); err != nil {
		return fmt.Errorf("node runner: boot probe cycle failed: %s", err)
	}
	return nil
}

// verifyDockerCLI refuses a docker CLI that cannot issue the sidecar launch
// line. The version that decides is the one INSIDE this image, never the host's:
// alpine 3.19 shipped 25.0.5 and rejected `volume-subpath` at boot with
// `unexpected key 'volume-subpath'` while the host CLI was 29.5.3 (P4 §9). The
// Dockerfile holds the build-time floor; this is the runtime one, because the
// image can be run against any daemon and its CLI can be replaced in place.
//
// A major skew against the daemon is LOGGED, never refused: docker's API
// negotiation makes any fixed maximum skew unsound, so a wide gap is a
// discovery signal rather than a rejection rule.
func (m *SidecarManager) verifyDockerCLI(ctx context.Context) error {
	out, stderr, code, err := m.docker(ctx, nil, nil,
		"version", "--format", "{{.Client.Version}} {{.Server.Version}}")
	if err != nil || code != 0 {
		return fmt.Errorf("node runner: read docker version: %s", failureText(stderr, err))
	}

	raw := strings.TrimSpace(out)
	client, server, paired := strings.Cut(raw, " ")
	clientMajor, clientOK := versionMajor(client)
	serverMajor, serverOK := versionMajor(server)
	if !paired || !clientOK || !serverOK {
		// Fail closed and quote what was read: a version this cannot parse is a
		// floor this cannot enforce, and the raw line is what names the cause.
		return fmt.Errorf("node runner: unreadable docker version %q: want a <client> <server> version pair", raw)
	}
	if clientMajor < dockerCLIMinMajor {
		return fmt.Errorf("node runner: docker CLI %s is older than the minimum %d.0 required for --mount volume-subpath",
			client, dockerCLIMinMajor)
	}

	skew := clientMajor - serverMajor
	if skew < 0 {
		skew = -skew
	}
	if skew > 1 {
		logger.FromContext(ctx).Warn("docker_cli_skew", "client", client, "server", server)
	}
	return nil
}

// versionMajor reads the leading major out of a docker version string
// ("28.3.3", "26.1.4-ce"). It reports false for anything that does not begin
// with a number, which its caller turns into a refusal.
func versionMajor(v string) (int, bool) {
	digits := strings.TrimSpace(v)
	if end := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
		digits = digits[:end]
	}
	major, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return major, true
}

// verifyUplink checks the shape of the network sidecars reach the internet
// through. All four refusals are verbatim (§3.9): each names the ONE property
// that is wrong, because "the uplink is bad" is not something an operator can act on.
func (m *SidecarManager) verifyUplink(ctx context.Context) error {
	name := m.cfg.UplinkNetwork
	out, _, code, err := m.docker(ctx, nil, nil, "network", "inspect", name,
		"--format", `{{.Driver}}|{{.Internal}}|{{index .Options "com.docker.network.bridge.enable_icc"}}`)
	if err != nil || code != 0 {
		return fmt.Errorf("node runner: network %s does not exist", name)
	}
	fields := strings.Split(strings.TrimSpace(out), "|")
	if len(fields) != 3 {
		return fmt.Errorf("node runner: network %s does not exist", name)
	}
	if fields[0] != "bridge" {
		return fmt.Errorf("node runner: network %s must use bridge driver", name)
	}
	if fields[1] == "true" {
		return fmt.Errorf("node runner: network %s must not be internal", name)
	}
	if fields[2] != "false" {
		return fmt.Errorf("node runner: network %s must set com.docker.network.bridge.enable_icc=false", name)
	}
	return nil
}

// verifyInvocationCIDR refuses a range that collides with ANY network on this
// daemon — not just the uplink (R-25). Docker rejects an overlapping --subnet
// at create time, so a collision that is not caught here does not show up as a
// boot refusal: it shows up as every egress invocation failing at run time,
// which is the same fault reported far away from its cause.
func (m *SidecarManager) verifyInvocationCIDR(ctx context.Context) error {
	out, _, code, err := m.docker(ctx, nil, nil, "network", "ls", "-q")
	if err != nil || code != 0 {
		return fmt.Errorf("node runner: list docker networks: %s", failureText("", err))
	}
	for _, id := range lines(out) {
		info, _, icode, ierr := m.docker(ctx, nil, nil, "network", "inspect", id,
			"--format", `{{.Name}}|{{range .IPAM.Config}}{{.Subnet}} {{end}}`)
		if ierr != nil || icode != 0 {
			// The network disappeared between ls and inspect. It cannot collide
			// with anything any more.
			continue
		}
		netName, subnets, ok := strings.Cut(strings.TrimSpace(info), "|")
		if !ok {
			continue
		}
		for _, subnet := range strings.Fields(subnets) {
			if m.pool.Overlaps(subnet) {
				return fmt.Errorf("node runner: invocation cidr %s overlaps docker network %s (%s)",
					m.cfg.InvocationCIDR, netName, subnet)
			}
		}
	}
	return nil
}

// bootProbeCycle drives one COMPLETE invocation — bridge, sidecar, binding,
// uplink, readiness, ONE SECRET REDEEMED FROM A NODE, teardown — before the
// service reports healthy. The empty pattern set means the proxy it stands up
// would refuse every host; what is being proven is the topology, not a grant.
//
// It binds exactly one secret, `boot-probe`, with a fresh handle, Found:false
// and an empty value. That is not decoration: the sidecar starts a broker only
// `if len(b.Secrets) > 0`, so the probe that carried no secrets could not reach
// the broker branch at all — it logged sidecar_opened and the service came up
// healthy at 02:17:38 on 2026-09-03 while the first real secret-bearing
// invocation failed at 02:25:39 on the socket directory (D-393). Guard coverage
// is the population at risk.
//
// And the redemption is driven from a node-shaped CONTAINER, not from this
// process: dir-search and connect(2) through the read-only subpath mount are
// decided by the accessor's uid class, and a runtime-side dial measures the
// wrong class on both inodes.
func (m *SidecarManager) bootProbeCycle(ctx context.Context) error {
	image, err := m.ownImage(ctx)
	if err != nil {
		return err
	}
	m.image = image

	subnet, err := m.pool.Acquire()
	if err != nil {
		return err
	}
	defer m.pool.Release(subnet)

	invocation := "inv-" + uuid.NewString()
	runID := uuid.New()
	token, err := probeToken()
	if err != nil {
		return err
	}
	handle := nodebroker.NewHandle()
	_, err = m.Open(ctx, usecase.SidecarOpen{
		InvocationID: invocation,
		RunID:        runID,
		Node:         probeSecretName,
		Binding: usecase.SidecarBinding{
			Invocation: invocation,
			Node:       probeSecretName,
			Secrets: map[string]usecase.SecretAnswer{
				probeSecretName: {Handle: handle, Found: false, Value: ""},
			},
			Egress: &usecase.EgressBinding{Patterns: []string{}, Token: token, Subnet: subnet},
		},
	})
	if err == nil {
		err = m.probeRedeem(ctx, invocation, runID, handle)
	}
	// Close on both paths: Open already tore down its own partial state, and a
	// second Close is a no-op by construction.
	closeErr := m.Close(context.WithoutCancel(ctx), invocation)
	if err != nil {
		return err
	}
	return closeErr
}

// probeRedeem redeems the probe's one handle from a container launched on the
// NODE's own line. The request travels on stdin — never argv, never the
// environment — because it carries a handle, and the container prints one line
// that carries a status and never a value.
func (m *SidecarManager) probeRedeem(ctx context.Context, invocation string, runID uuid.UUID, handle string) error {
	request, err := json.Marshal(nodebroker.Request{
		Handle:     handle,
		Invocation: invocation,
		Name:       probeSecretName,
		Node:       probeSecretName,
	})
	if err != nil {
		return fmt.Errorf("encode probe redemption: %w", err)
	}
	args := probeRedeemArgs(usecase.BundleLaunch{
		RunID:         runID,
		InvocationID:  invocation,
		Image:         m.image,
		BrokerSubpath: invocation,
	}, m.cfg.RunsVolume)

	out, stderr, code, err := m.docker(ctx, nil, request, args...)
	if err != nil || code != 0 {
		return fmt.Errorf("redeem the probe secret from a node: %s", failureText(stderr, err))
	}
	if got := strings.TrimSpace(out); got != probeRedeemOK {
		return fmt.Errorf("redeem the probe secret from a node: got %q, want %q", got, probeRedeemOK)
	}
	return nil
}

// ownImage reads the image THIS container runs, which is the image a sidecar
// runs. Sidecars are the runtime's own bytes by construction: nothing else has
// to be published, pinned or trusted.
func (m *SidecarManager) ownImage(ctx context.Context) (string, error) {
	out, stderr, code, err := m.docker(ctx, nil, nil, "inspect", m.self, "--format", "{{.Image}}")
	if err != nil || code != 0 {
		return "", fmt.Errorf("read own image id: %s", failureText(stderr, err))
	}
	image := strings.TrimSpace(out)
	if image == "" {
		return "", fmt.Errorf("read own image id: empty")
	}
	return image, nil
}

// Open stands up one invocation's sidecar and returns what the node needs to
// reach it. Every step is ordered for a reason (§3.7):
//
//	dir → [bridge] → sidecar → BINDING → [uplink] → readiness
//
// The binding is delivered BEFORE the sidecar has any route off its own bridge,
// so the credentials cross into a container that cannot yet talk to anything;
// and the node is launched only after readiness, so it never dials a socket
// nobody serves.
func (m *SidecarManager) Open(ctx context.Context, in usecase.SidecarOpen) (usecase.Sidecar, error) {
	if in.InvocationID == "" {
		return usecase.Sidecar{}, fmt.Errorf("node runner: sidecar open without an invocation id")
	}
	if m.image == "" {
		return usecase.Sidecar{}, domain.ErrNodeRunnerNotReady
	}

	bridge := in.Binding.Egress != nil
	name := sidecarContainerName(in.InvocationID)
	network := ""
	if bridge {
		network = invocationNetworkName(in.InvocationID)
	}

	// Registered BEFORE anything exists: the sweeper decides by this map, so an
	// invocation whose resources are half-created must already be live.
	m.mu.Lock()
	m.live[in.InvocationID] = liveInvocation{run: in.RunID, bridge: bridge}
	m.mu.Unlock()

	started := time.Now()
	sc, err := m.open(ctx, in, name, network)
	if err != nil {
		// Teardown gets a context that outlives a cancelled one; leaving a
		// half-open sidecar behind would leak a container AND a bridge.
		_ = m.Close(context.WithoutCancel(ctx), in.InvocationID)
		return usecase.Sidecar{}, err
	}

	setup := time.Since(started).Milliseconds()
	sidecarSetupMS.WithLabelValues(strconv.FormatBool(bridge)).Observe(float64(setup))
	subnet := ""
	if bridge {
		subnet = in.Binding.Egress.Subnet
	}
	logger.FromContext(ctx).Info("sidecar_opened",
		"run", in.RunID.String(), "invocation_id", in.InvocationID, "node", in.Node,
		"network", network, "subnet", subnet, "setup_ms", setup)
	return sc, nil
}

func (m *SidecarManager) open(ctx context.Context, in usecase.SidecarOpen, name, network string) (usecase.Sidecar, error) {
	dir := filepath.Join(m.cfg.RunsDir, in.InvocationID)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return usecase.Sidecar{}, fmt.Errorf("node runner: create invocation dir: %w", err)
	}
	// MkdirAll's mode is narrowed by the umask, so the mode is set explicitly
	// HERE, by the only process that can set it: this directory is owned by the
	// runtime's uid, and every other party to it — the sidecar at 65534 with
	// CAP_FOWNER dropped, and the node — can only read it. The sidecar must be
	// able to create broker.sock and the node must be able to search through to
	// it, and nodebroker.Listen only VERIFIES that from its side, because a
	// chmod from there is EPERM by construction (D-393).
	// The NODE's view of this same directory is a read-only mount, and that is
	// where the boundary is — not in this mode.
	if err := os.Chmod(dir, 0o777); err != nil {
		return usecase.Sidecar{}, fmt.Errorf("node runner: open invocation dir: %w", err)
	}

	if network != "" {
		args := networkCreateArgs(network, in.Binding.Egress.Subnet, in.InvocationID, in.RunID)
		if _, stderr, code, err := m.docker(ctx, nil, nil, args...); err != nil || code != 0 {
			return usecase.Sidecar{}, fmt.Errorf("node runner: create invocation network %s: %s",
				network, failureText(stderr, err))
		}
	}

	runArgs := sidecarRunArgs(in.InvocationID, in.RunID, network, m.image, m.cfg.RunsVolume)
	if _, stderr, code, err := m.docker(ctx, nil, nil, runArgs...); err != nil || code != 0 {
		return usecase.Sidecar{}, fmt.Errorf("node runner: run sidecar %s: %s", name, failureText(stderr, err))
	}

	// THE BINDING. It travels on the attached stdin stream of this exec and
	// nowhere else: not argv (visible in docker inspect and /proc/1/cmdline),
	// not the environment (visible in docker inspect and `docker exec env`), not
	// a file. No error path below ever includes the document in its message.
	document, err := json.Marshal(in.Binding)
	if err != nil {
		return usecase.Sidecar{}, fmt.Errorf("node runner: encode sidecar binding: %w", err)
	}
	if _, stderr, code, err := m.docker(ctx, nil, document, bindExecArgs(name)...); err != nil || code != 0 {
		return usecase.Sidecar{}, fmt.Errorf("node runner: bind sidecar %s: %s", name, failureText(stderr, err))
	}

	// The uplink is connected AFTER the binding: until this line the sidecar has
	// no route anywhere, so the one moment it holds credentials and no policy is
	// a moment it cannot reach anything.
	if network != "" {
		if _, stderr, code, err := m.docker(ctx, nil, nil,
			"network", "connect", m.cfg.UplinkNetwork, name); err != nil || code != 0 {
			return usecase.Sidecar{}, fmt.Errorf("node runner: connect %s to %s: %s",
				name, m.cfg.UplinkNetwork, failureText(stderr, err))
		}
	}

	if err := m.awaitReady(ctx, name); err != nil {
		return usecase.Sidecar{}, err
	}

	proxyURL := ""
	if network != "" {
		proxyURL = "http://" + sidecarProxyAlias + ":" + strconv.Itoa(sidecarProxyPort)
	}
	return usecase.Sidecar{BrokerSubpath: in.InvocationID, Network: network, ProxyURL: proxyURL}, nil
}

// awaitReady polls the sidecar's own health endpoint until it reports ok. "ok"
// means the binding is applied and every listener it asked for is serving —
// container "running" is not readiness, and launching a node on it would race
// the broker socket into existence.
func (m *SidecarManager) awaitReady(ctx context.Context, name string) error {
	timeout := m.cfg.SidecarReadyTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		out, _, code, err := m.docker(ctx, nil, nil, "exec", name, "wget", "-qO-", sidecarHealthURL)
		if err == nil && code == 0 && strings.TrimSpace(out) == "ok" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: sidecar %s did not become ready in %s",
				domain.ErrNodeRunnerNotReady, name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPollEvery):
		}
	}
}

// Close removes one invocation's sidecar, its bridge and its directory, in that
// order and idempotently: it runs on the success path, on every failure path,
// and again from a sweep, and must be correct from any of them.
func (m *SidecarManager) Close(ctx context.Context, invocationID string) error {
	if invocationID == "" {
		return nil
	}
	started := time.Now()

	m.mu.Lock()
	entry, known := m.live[invocationID]
	delete(m.live, invocationID)
	m.mu.Unlock()

	name := sidecarContainerName(invocationID)
	_, _, _, _ = m.docker(ctx, nil, nil, "rm", "-f", name)
	// The network removal is attempted whether or not this process remembers a
	// bridge: after a restart it remembers nothing, and a leaked bridge holds an
	// address range the pool believes is free.
	if !known || entry.bridge {
		_, _, _, _ = m.docker(ctx, nil, nil, "network", "rm", invocationNetworkName(invocationID))
	}

	var err error
	if rmErr := os.RemoveAll(filepath.Join(m.cfg.RunsDir, invocationID)); rmErr != nil {
		err = fmt.Errorf("node runner: remove invocation dir: %w", rmErr)
	}

	teardown := time.Since(started).Milliseconds()
	sidecarTeardownMS.WithLabelValues(strconv.FormatBool(entry.bridge)).Observe(float64(teardown))
	logger.FromContext(ctx).Info("sidecar_closed",
		"run", entry.run.String(), "invocation_id", invocationID, "teardown_ms", teardown)
	return err
}

// SweepRun removes everything one graph execution left behind: containers
// first, then networks, then the directories — the order matters because a
// directory is only an orphan once nothing can still be writing to it.
func (m *SidecarManager) SweepRun(ctx context.Context, runID uuid.UUID) error {
	selector := "label=" + labelRun + "=" + runID.String()
	m.removeContainers(ctx, m.listContainers(ctx, selector))
	m.removeNetworks(ctx, m.listNetworks(ctx, selector))

	m.mu.Lock()
	var dirs []string
	for invocation, entry := range m.live {
		if entry.run == runID {
			dirs = append(dirs, invocation)
			delete(m.live, invocation)
		}
	}
	m.mu.Unlock()

	for _, invocation := range dirs {
		if err := os.RemoveAll(filepath.Join(m.cfg.RunsDir, invocation)); err != nil {
			return fmt.Errorf("node runner: remove invocation dir: %w", err)
		}
	}
	return nil
}

// SweepAll is the BOOT sweep: every resource carrying the invocation label,
// containers before networks, then a re-enumeration — and it returns what
// REMAINS, not what it removed, because the only number worth refusing on is
// the one measured after the attempt.
//
// It is boot-only. Nothing is live at boot, so removing by label alone is safe
// here; the periodic sweeper (StartSweeper) is the one that must respect the
// live set.
//
// Probe runs it BEFORE the verifier that could refuse on its own leftovers
// (R-30): every invocation bridge is carved out of invocation_cidr by
// construction, so verifyInvocationCIDR ahead of this sweep would refuse on a
// bridge this sweep exists to remove, and no restart could ever clear it.
func (m *SidecarManager) SweepAll(ctx context.Context) (int, int, error) {
	filter := "label=" + labelInvocation
	m.removeContainers(ctx, m.listContainers(ctx, filter))
	m.removeNetworks(ctx, m.listNetworks(ctx, filter))

	containers := len(m.listContainers(ctx, filter))
	networks := len(m.listNetworks(ctx, filter))
	if containers > 0 || networks > 0 {
		return containers, networks, nil
	}

	entries, err := os.ReadDir(m.cfg.RunsDir)
	if err != nil && !os.IsNotExist(err) {
		return containers, networks, fmt.Errorf("node runner: read runs dir %s: %w", m.cfg.RunsDir, err)
	}
	for _, entry := range entries {
		if rmErr := os.RemoveAll(filepath.Join(m.cfg.RunsDir, entry.Name())); rmErr != nil {
			return containers, networks, fmt.Errorf("node runner: sweep runs dir %s: %w", m.cfg.RunsDir, rmErr)
		}
	}
	return containers, networks, nil
}

// StartSweeper runs the orphan sweep on a ticker for as long as ctx lives.
//
// ⚠ It removes ONLY sidecars — containers matching the invocation label AND the
// sentiae-sc- name — plus invocation networks and directories, and only those
// whose invocation this process does not have live. A node container carries the
// same invocation label but is NOT a sidecar, and a node that declares neither
// secrets nor egress never reaches this manager at all: sweeping by label alone
// would force-remove live work mid-run (R-24). Leaked node containers run --rm
// and are the boot sweep's business, when nothing is live.
func (m *SidecarManager) StartSweeper(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sweeperStarted {
		return
	}
	m.sweeperStarted = true
	go m.sweepLoop(ctx)
}

// Stop ends the sweeper and waits for it (§21's shutdown group).
func (m *SidecarManager) Stop() {
	m.stopOnce.Do(func() { close(m.stopCh) })
	m.mu.Lock()
	started := m.sweeperStarted
	m.mu.Unlock()
	if !started {
		return
	}
	<-m.doneCh
}

func (m *SidecarManager) sweepLoop(ctx context.Context) {
	defer close(m.doneCh)
	defer func() {
		if r := recover(); r != nil {
			logger.FromContext(ctx).Error("node sidecar sweeper panicked", "panic", r)
		}
	}()
	ticker := time.NewTicker(sweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.sweepOrphans(ctx)
		}
	}
}

// sweepOrphans is one pass. Failures are logged, never propagated: a sweep that
// could not reach the daemon must not take the service down with it.
func (m *SidecarManager) sweepOrphans(ctx context.Context) {
	for _, name := range m.listContainers(ctx, "label="+labelInvocation, "name="+sidecarNamePrefix) {
		invocation := strings.TrimPrefix(name, sidecarNamePrefix)
		if m.isLive(invocation) {
			continue
		}
		if _, stderr, code, err := m.docker(ctx, nil, nil, "rm", "-f", name); err != nil || code != 0 {
			logger.FromContext(ctx).Warn("sidecar_orphan_remove_failed",
				"container", name, "err", failureText(stderr, err))
			continue
		}
		logger.FromContext(ctx).Info("sidecar_orphan_removed", "container", name, "invocation_id", invocation)
	}

	for _, name := range m.listNetworks(ctx, "label="+labelInvocation) {
		invocation := strings.TrimPrefix(name, invocationNetworkPrefix)
		if m.isLive(invocation) {
			continue
		}
		if _, stderr, code, err := m.docker(ctx, nil, nil, "network", "rm", name); err != nil || code != 0 {
			logger.FromContext(ctx).Warn("sidecar_orphan_network_remove_failed",
				"network", name, "err", failureText(stderr, err))
		}
	}

	entries, err := os.ReadDir(m.cfg.RunsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.FromContext(ctx).Warn("sidecar_orphan_dir_scan_failed", "err", err)
		}
		return
	}
	for _, entry := range entries {
		if m.isLive(entry.Name()) {
			continue
		}
		if rmErr := os.RemoveAll(filepath.Join(m.cfg.RunsDir, entry.Name())); rmErr != nil {
			logger.FromContext(ctx).Warn("sidecar_orphan_dir_remove_failed", "dir", entry.Name(), "err", rmErr)
		}
	}
}

func (m *SidecarManager) isLive(invocationID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.live[invocationID]
	return ok
}

// listContainers returns container NAMES matching every filter, including
// stopped ones: an exited orphan still holds its name and its network.
func (m *SidecarManager) listContainers(ctx context.Context, filters ...string) []string {
	args := []string{"ps", "-a"}
	for _, f := range filters {
		args = append(args, "--filter", f)
	}
	args = append(args, "--format", "{{.Names}}")
	out, _, code, err := m.docker(ctx, nil, nil, args...)
	if err != nil || code != 0 {
		return nil
	}
	return lines(out)
}

// listNetworks returns network NAMES matching every filter.
func (m *SidecarManager) listNetworks(ctx context.Context, filters ...string) []string {
	args := []string{"network", "ls"}
	for _, f := range filters {
		args = append(args, "--filter", f)
	}
	args = append(args, "--format", "{{.Name}}")
	out, _, code, err := m.docker(ctx, nil, nil, args...)
	if err != nil || code != 0 {
		return nil
	}
	return lines(out)
}

func (m *SidecarManager) removeContainers(ctx context.Context, names []string) {
	for _, name := range names {
		if _, stderr, code, err := m.docker(ctx, nil, nil, "rm", "-f", name); err != nil || code != 0 {
			logger.FromContext(ctx).Warn("sidecar_sweep_container_failed",
				"container", name, "err", failureText(stderr, err))
		}
	}
}

func (m *SidecarManager) removeNetworks(ctx context.Context, names []string) {
	for _, name := range names {
		if _, stderr, code, err := m.docker(ctx, nil, nil, "network", "rm", name); err != nil || code != 0 {
			logger.FromContext(ctx).Warn("sidecar_sweep_network_failed",
				"network", name, "err", failureText(stderr, err))
		}
	}
}

// ── the pinned launch lines ────────────────────────────────────────────────
//
// These four builders are pure so the exact argv — the labels, the subnet, the
// read-write invocation mount, the network, the alias, the entrypoint and the
// ABSENCE of anything after the image — is asserted without a daemon (T4.10).

const (
	// sidecarNamePrefix and invocationNetworkPrefix are the only places these
	// names are spelled. The sweeper's safety property depends on the sidecar
	// prefix being distinguishable from a node container's.
	sidecarNamePrefix       = "sentiae-sc-"
	invocationNetworkPrefix = "sentiae-inv-"

	// sidecarProxyPort is the port the sidecar's proxy listens on, inside the
	// invocation bridge.
	sidecarProxyPort = 3128
)

func sidecarContainerName(invocationID string) string { return sidecarNamePrefix + invocationID }

func invocationNetworkName(invocationID string) string { return invocationNetworkPrefix + invocationID }

// networkCreateArgs builds the per-invocation bridge: --internal, so nothing on
// it can reach anything the sidecar does not proxy, with an explicitly
// allocated subnet so two invocations can never share an address range.
func networkCreateArgs(network, subnet, invocationID string, runID uuid.UUID) []string {
	return []string{
		"network", "create",
		"--internal",
		"--subnet", subnet,
		"--label", labelInvocation + "=" + invocationID,
		"--label", labelRun + "=" + runID.String(),
		network,
	}
}

// sidecarRunArgs builds the sidecar launch (R-23's pinned order).
//
// --entrypoint is load-bearing: the runtime image's own ENTRYPOINT is the
// server, so a trailing /app/node-sidecar would be APPENDED to it and would
// boot a second runtime-service — a container that reports running and never
// answers /healthz. NOTHING follows the image.
func sidecarRunArgs(invocationID string, runID uuid.UUID, network, image, runsVolume string) []string {
	args := []string{
		"run", "-d",
		"--name", sidecarContainerName(invocationID),
		"--label", labelInvocation + "=" + invocationID,
		"--label", labelRun + "=" + runID.String(),
	}
	args = append(args, hardenedFlags(sidecarMemMB, sidecarVCPU)...)
	args = append(args, "--mount",
		"type=volume,source="+runsVolume+",target="+sidecarMountTarget+",volume-subpath="+invocationID)

	if network == "" {
		args = append(args, "--network", "none")
	} else {
		args = append(args, "--network", network, "--network-alias", sidecarProxyAlias)
	}
	return append(args, "--entrypoint", sidecarBinary, image)
}

// bindExecArgs builds the ONE channel the binding travels on. There is nothing
// on this argv but the container and the subcommand, and the caller passes a
// nil environment: the document goes in on stdin (§3.7, A10).
func bindExecArgs(name string) []string {
	return []string{"exec", "-i", name, sidecarBinary, "bind"}
}

// probeRedeemArgs is the boot probe's redemption container, DERIVED from
// bundleRunArgs rather than written out: the whole point of the probe is to
// measure the socket from the node's uid class, so the probe container that
// cannot drift from the real node launch line is the only one worth trusting.
// The two differences are the entrypoint (the runtime image's own ENTRYPOINT is
// the server, and the redeeming binary is the sidecar's) and the `redeem`
// subcommand after the image.
func probeRedeemArgs(launch usecase.BundleLaunch, runsVolume string) []string {
	args := bundleRunArgs(launch, runsVolume)
	image := args[len(args)-1]
	return append(args[:len(args)-1], "--entrypoint", sidecarBinary, image, "redeem")
}

// probeToken mints the boot probe's bearer with the same entropy a real
// invocation gets: 32 crypto/rand bytes as 64 lower-hex, and NO `handle:`
// prefix, so grepping for `handle:` stays a secret-only signal (R-21 F-3(i)).
func probeToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mint probe token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// lines splits command output into non-empty trimmed lines.
func lines(out string) []string {
	var result []string
	for _, line := range strings.Split(out, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
