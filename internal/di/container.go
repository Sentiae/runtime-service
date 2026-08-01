package di

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	pkconfig "github.com/sentiae/platform-kit/config"
	"github.com/sentiae/platform-kit/grpcclient"
	kafka "github.com/sentiae/platform-kit/kafka"
	"github.com/sentiae/platform-kit/posture"
	"github.com/sentiae/platform-kit/secret"
	timetravel "github.com/sentiae/platform-kit/timetravel"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	"github.com/sentiae/runtime-service/internal/domain"

	eventhandler "github.com/sentiae/runtime-service/internal/handler/event"
	grpchandler "github.com/sentiae/runtime-service/internal/handler/grpc"
	httphandler "github.com/sentiae/runtime-service/internal/handler/http"
	"github.com/sentiae/runtime-service/internal/infrastructure/agent"
	"github.com/sentiae/runtime-service/internal/infrastructure/caddy"
	"github.com/sentiae/runtime-service/internal/infrastructure/canvasservice"
	"github.com/sentiae/runtime-service/internal/infrastructure/compiler"
	"github.com/sentiae/runtime-service/internal/infrastructure/container"
	"github.com/sentiae/runtime-service/internal/infrastructure/executors"
	"github.com/sentiae/runtime-service/internal/infrastructure/executors/a11y"
	"github.com/sentiae/runtime-service/internal/infrastructure/executors/visual"
	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker"
	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker/vmcomm"
	"github.com/sentiae/runtime-service/internal/infrastructure/foundry"
	"github.com/sentiae/runtime-service/internal/infrastructure/gitservice"
	"github.com/sentiae/runtime-service/internal/infrastructure/hostcapacity"
	"github.com/sentiae/runtime-service/internal/infrastructure/messaging"
	"github.com/sentiae/runtime-service/internal/infrastructure/netfabric"
	"github.com/sentiae/runtime-service/internal/infrastructure/objectstore"
	"github.com/sentiae/runtime-service/internal/infrastructure/oci"
	"github.com/sentiae/runtime-service/internal/infrastructure/simulated"
	"github.com/sentiae/runtime-service/internal/infrastructure/vaulttoken"
	volumebackend "github.com/sentiae/runtime-service/internal/infrastructure/volume"
	"github.com/sentiae/runtime-service/internal/port/gateway"
	"github.com/sentiae/runtime-service/internal/repository"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/sentiae/runtime-service/pkg/config"
)

// Container holds all application dependencies
type Container struct {
	// Database
	DB *gorm.DB

	// Configuration
	Config *config.Config

	// Phase 2 mTLS mesh — shared SPIFFE X509 source for OUTBOUND client dials
	// (canvas-service push). Built only when APP_GRPC_MTLS_MODE != "off";
	// nil on any error (degrade to insecure) or when mode is off. Distinct
	// from the gRPC server's source — do not share across that boundary.
	canvasClientSource *workloadapi.X509Source

	// Repositories
	ExecutionRepo       repository.ExecutionRepository
	VMRepo              repository.MicroVMRepository
	SnapshotRepo        repository.SnapshotRepository
	MetricsRepo         repository.ExecutionMetricsRepository
	VMInstanceRepo      repository.VMInstanceRepository
	TerminalSessionRepo repository.TerminalSessionRepository

	// Graph Repositories
	GraphDefRepo      repository.GraphDefinitionRepository
	GraphNodeRepo     repository.GraphNodeRepository
	GraphEdgeRepo     repository.GraphEdgeRepository
	GraphExecRepo     repository.GraphExecutionRepository
	NodeExecRepo      repository.NodeExecutionRepository
	DebugSessionRepo  repository.GraphDebugSessionRepository
	GraphTraceRepo    repository.GraphTraceRepository
	TraceSnapshotRepo repository.GraphTraceSnapshotRepository

	// Test tracking
	TestRunRepo          *postgres.TestRunRepo
	HermeticBuildRepo    *postgres.HermeticBuildRepo
	HermeticBuildUC      *usecase.HermeticBuildUseCase
	StepArtifactHashRepo *postgres.StepArtifactHashRepo

	// Regression test templates generated from production traces
	RegressionTestRepo *postgres.RegressionTestRepo

	// 9.4 — customer-hosted Firecracker agent registry + dispatcher
	RuntimeAgentRepo *postgres.RuntimeAgentRepository
	AgentRegistry    *usecase.AgentRegistry
	RemoteExecutor   usecase.RemoteExecutor
	AgentDispatcher  *usecase.Dispatcher

	// Infrastructure
	FCProvider        *firecracker.Provider
	ContainerProvider *container.Provider
	VMProvider        usecase.VMProvider
	ExecutionRunner   usecase.ExecutionRunner
	EventPublisher    messaging.EventPublisher
	EventConsumer     *messaging.EventConsumer

	// VsockPool is the vsock warm VM pool behind ExecutionRunner when the
	// executor is firecracker + vsock. Kept on the container so Close() can
	// cancel its idle-cleanup loop and terminate its VMs; without this the
	// loop would outlive shutdown and run to process exit.
	VsockPool *vmcomm.Pool

	// Posture is runtime's declared security posture (D-179 Wave-8). It carries
	// the service's one real fail-closed boot control — the gRPC mesh must not be
	// configured with mTLS off — and backs the /posture ops endpoint. Declared and
	// proven at boot (initPosture); a failed assertion refuses boot.
	Posture *posture.Set

	// §9.1 — warm Firecracker VM pools, one per language profile. Kept
	// on the container so the background controller hook can Start()
	// them at service start and Close() them at shutdown.
	FCPools []*firecracker.VMPool

	// §9.3 — per-VM auto-snapshot scheduler. Distinct from the
	// usecase-layer CheckpointScheduler (DB poller); this one drives
	// Pause→CreateSnapshot→Resume from Boot-time registration.
	FCCheckpointScheduler *firecracker.CheckpointScheduler

	// Warm-VM / snapshot-clone code-execution path. When wired (executor
	// firecracker + APP_WARM_POOL_ENABLED), code nodes run on a fast warm
	// clone (~160ms) instead of a single-shot cold boot. Nil → cold path.
	WarmPool *firecracker.WarmPool

	// Inbound event handler service (canvas → runtime execution requests).
	InboundEventHandlerSvc *usecase.InboundEventHandlerService

	// A1 — Tier 1A Canvas → Runtime event-driven consumer for
	// `sentiae.canvas.node.executed`. Complements the existing
	// `canvas.code.execute_requested` handler by reacting to the
	// canvas-domain "node was run" fact so Run-on-canvas is resilient
	// across runtime restarts.
	CanvasExecutedConsumer *messaging.CanvasExecutedConsumer

	// §8.6 — git-service events drive affected-test triggering and
	// post-merge full suite runs. Both handlers are best-effort: they
	// log and return nil on any failure so the consumer loop keeps
	// chewing through the topic.
	AffectedTestTrigger     *usecase.AffectedTestTrigger
	SessionLifecycleHandler *usecase.SessionLifecycleHandler

	// §8.6 — session.commit_added consumer. Routes every commit
	// landing on a session to the PoolScheduler for a fresh test run
	// across the session's linked canvas.
	SessionCommitAddedConsumer *messaging.SessionCommitAddedConsumer

	// CS-2 G2.7 — git.push.received + git.session.created trigger that
	// enqueues affected / smoke runs.
	ContinuousTestTrigger *eventhandler.ContinuousTestTrigger

	// §8.3 test pool scheduler — worker pool that runs TestRun jobs
	// against freshly acquired VMs. Wired here so the commit-added
	// consumer can submit jobs as they arrive.
	TestPool *usecase.PoolScheduler

	// Multi-file project compile (ephemeral build container, no execution).
	ProjectCompiler usecase.ProjectCompiler
	CompileUC       *usecase.CompileProject

	// runtime-fleet CP3 — OCI→ext4 image boot + FleetOrchestration.
	ImageWorkloadRepo repository.ImageWorkloadRepository

	// runtime-fleet CP4 — durable fleet control-plane store.
	HostRepo      repository.HostRepository
	FleetAppRepo  repository.FleetAppRepository
	ReplicaRepo   repository.ReplicaRepository
	PlacementRepo repository.PlacementRepository
	RouteRepo     repository.RouteRepository
	VolumeRepo    repository.VolumeRepository
	// SentiaeDB Phase 0 — the durable microVM addressing plane's allocation store.
	// It is also what the host registry assigns net ordinals through. The allocator
	// built over it is host-scoped and lives inside initNetPlane, which hands it
	// straight to the booter — no other component may reach it.
	NetLeaseRepo      repository.NetLeaseRepository
	ImageBooter       usecase.ImageBooter
	ImageMaterializer usecase.ImageMaterializer
	FleetProvisionUC  *usecase.FleetProvision

	// D-185a — the post-boot host->guest control channel (SYNCFS/FREEZE/THAW/
	// SHUTDOWN). Real off the firecracker host is impossible, so the fail-loud
	// implementation is wired there: a silently skipped quiesce would report a
	// consistent snapshot of a filesystem that was never flushed.
	GuestControl       gateway.GuestControl
	guestControlTokens *firecracker.GuestControlTokens

	// runtime-fleet CP4 rt#9 — persistent-volume backing-file backend + manager.
	// The backend is fail-loud off the firecracker host so a volume is never
	// silently faked.
	VolumeBackend      usecase.VolumeBackend
	FleetVolumeManager *usecase.FleetVolumeManager

	// CP4.5 §9#3 (D-183) — P19 durable resource control plane. The repo + the
	// dedicated provisioner are wired on every host: off the firecracker host the
	// dedicated path fails loud through the composed FleetProvision
	// (FailLoudImageBooter), never a silent fake. The volume snapshotter is
	// firecracker-host-only (it needs a live VMPauser). The shared-tier provisioner
	// + its TTL reaper are non-nil only when the shared logical engine is wired.
	// The in-place restorer (D-184) needs BOTH the orchestrator (to stop/start the
	// data VM) and a configured artifact store (to fetch the recovery point), so
	// it stays nil elsewhere and Capabilities reports supports_restore=false.
	FleetResourceRepo     repository.FleetResourceRepository
	ResourceSnapshotter   *usecase.FleetVolumeSnapshotter
	ResourceRestorer      *usecase.FleetVolumeRestorer
	FleetResourceUC       *usecase.FleetResourceProvisioner
	FleetResourceSharedUC *usecase.FleetResourceSharedProvisioner
	ResourceServer        *grpchandler.ResourceServer
	snapshotArtifactStore usecase.ArtifactStore

	// CP4.5 §9#5 — P21 fleet network fabric (per-system×env policy scope compiled
	// to iptables). The enforcer is fail-loud off the firecracker host, and also
	// whenever the real one could not install or PROVE its FORWARD program at boot
	// — a control that cannot prove itself must prevent the operation.
	FleetNetworkRepo       repository.FleetNetworkRepository
	FleetNetworkPolicyRepo repository.FleetNetworkPolicyRepository
	NetworkEnforcer        usecase.NetworkEnforcer
	FleetNetworkFabricUC   *usecase.FleetNetworkFabric

	// runtime-fleet CP4 §9#6 — resident replica boot/teardown/health. Non-nil
	// only on the firecracker executor (the fleet host); nil otherwise.
	FleetReplicaRuntimeUC *usecase.FleetReplicaRuntime

	// Reclaims orphaned materialize staging directories under the image-boot work
	// root (#fleet-image-staging-dirs-no-gc). Wired wherever a work root is
	// configured — BOTH boot paths stage under it, on every executor.
	FleetStagingSweeper *usecase.FleetStagingSweeper

	// Report-only two-directional audit of the volume + recovery-point ledger
	// against physical reality (row-without-file, file-without-row,
	// recovery-point-without-object). Wired everywhere, but scoped to the volumes
	// pinned to THIS host, so it is a no-op until self-registration hands it a
	// host identity. It never deletes, moves or repairs anything.
	FleetLedgerReconciler *usecase.FleetLedgerReconciler

	// The durability metric collector: the timer that keeps the recovery-point,
	// snapshot-health, addressing-plane and host-attestation gauges true. It is a
	// TIMER rather than a write-path hook on purpose — a gauge written only when
	// work succeeds can never report that the work stopped, which is the exact
	// false-green the durability surface exists to remove. Stopped by Close.
	FleetDurabilityMetrics *usecase.FleetDurabilityCollector

	// D-200 — the control-plane recovery-point mirror: it copies primary_only
	// recovery points into the SECOND failure domain and promotes the ledger row
	// only on a checksum-confirmed copy. Non-nil ONLY on the single mesh instance
	// (see newRecoveryPointMirrorWorker) — a Firecracker fleet host holds no
	// off-chassis object-store credential at all, which is the point of the
	// decision. Stopped by Close.
	RecoveryPointMirror *usecase.RecoveryPointMirrorWorker

	// runtime-fleet P3.4 — the Vault client backing the per-tenant secret
	// resolver on the fleet host. Non-nil only when the firecracker executor is
	// selected AND VAULT_ADDR/VAULT_AUTH_MODE are set and the client built.
	// Closed by Close.
	vaultClient *pkconfig.VaultClient

	// runtime-fleet D-125 — in-memory store of handed per-deployment Vault
	// tokens (never persisted); renews each for the deployment lifetime and
	// revokes on Decommission. Non-nil only alongside the handed-token resolver.
	fleetTokenStore *usecase.FleetSecretTokenStore

	// runtime-fleet D-124 — in-memory store of handed per-deployment registry PULL
	// tokens (never persisted); read at materialize (the image pull) and dropped on
	// Decommission. Always constructed (registry pulls are always needed); nil-safe
	// consumers fall back to the shared service key when a deploy handed no token.
	fleetRegistryTokenStore *usecase.FleetRegistryTokenStore

	// runtime-fleet CP4 §9#4 — host registry + this instance's self-host.
	FleetHostRegistry   *usecase.FleetHostRegistry
	fleetSelf           *domain.Host // non-nil only on the firecracker executor
	fleetHeartbeatEvery time.Duration

	// runtime-fleet CP4 §9#5 — placement decision function (bin_pack / spread /
	// affinity). Read-only; the #7 reconciler calls SelectHost.
	FleetScheduler *usecase.FleetScheduler

	// runtime-fleet CP4 §9#7 — reconciler-backed app→replicas orchestrator.
	// Non-nil only on the firecracker executor (needs the replica runtime).
	FleetOrchestratorUC *usecase.FleetOrchestrator

	// runtime-fleet CP4 rt#11 — scale-to-zero wake path. Non-nil only when the
	// orchestrator is wired (firecracker executor).
	FleetActivatorUC *usecase.FleetActivator

	// #fleet-scale-to-zero-activity-feed (D-122) — tails the fleet Caddy access
	// log so SweepIdle does not scale a directly-served app to zero. Non-nil only
	// on the firecracker executor with an access-log path configured; its Run
	// goroutine is started in the lifecycle.
	FleetActivityFeed *caddy.AccessLogFeed

	// Use Cases
	ExecutionUC  usecase.ExecutionUseCase
	VMUC         usecase.VMUseCase
	SnapshotUC   usecase.SnapshotUseCase
	VMInstanceUC usecase.VMInstanceUseCase
	SchedulerUC  usecase.SchedulerUseCase
	TerminalUC   usecase.TerminalUseCase
	TestGenUC    usecase.TestGenerationUseCase
	AffectedUC   usecase.AffectedTestResolver

	// Graph Use Cases
	GraphUC       usecase.GraphUseCase
	GraphEngine   *usecase.GraphExecutionEngine
	GraphDebugUC  usecase.GraphDebugUseCase
	GraphReplayUC usecase.GraphReplayUseCase
	TraceRecorder *usecase.GraphTraceRecorder

	// Controllers
	ReconciliationController *usecase.ReconciliationController
	PendingProcessor         *usecase.PendingProcessor
	CheckpointScheduler      *usecase.CheckpointScheduler
	QuarantineUC             *usecase.QuarantineUseCase
	PostMergeFullRunTrigger  *usecase.PostMergeFullRunTrigger

	// Regression test generator (wires traces → pytest/junit scaffolds).
	RegressionGenerator *usecase.RegressionTestGenerator

	// Handlers
	HTTPServer *httphandler.Server
	GRPCServer *grpchandler.Server
}

// NewContainer creates and initializes the DI container
func NewContainer(cfg *config.Config) (*Container, error) {
	c := &Container{
		Config: cfg,
	}

	// Initialize database
	if err := c.initDatabase(cfg); err != nil {
		return nil, fmt.Errorf("failed to initialize database: %w", err)
	}

	// Initialize infrastructure
	c.initInfrastructure(cfg)

	// Initialize repositories
	c.initRepositories()

	// Initialize use cases
	c.initUseCases(cfg)

	// runtime-fleet CP3 — image-boot materializer, booter, FleetOrchestration.
	// Runs BEFORE initHandlers so FleetActivatorUC (rt#11) exists when
	// initHandlers gates the /_activate mount and builds the chi router in
	// SetupRoutes. The fleet's one back-edge into handlers — the FleetOrchestration
	// gRPC registration, which needs the GRPCServer that initHandlers builds — is
	// relocated to the end of initHandlers to break the init cycle.
	if err := c.initFleet(cfg); err != nil {
		return nil, fmt.Errorf("failed to initialize fleet: %w", err)
	}

	// Declare + prove the service's fail-closed security posture (D-179 Wave-8)
	// before any handler serves. A failed assertion refuses boot.
	if err := c.initPosture(); err != nil {
		return nil, fmt.Errorf("boot posture assertion failed: %w", err)
	}

	// Phase 2 mTLS mesh: build the shared client-side SPIFFE source for runtime's
	// outbound gRPC dials (the canvas push) via the fail-closed platform-kit helper
	// (v0.3.4). NewMeshSource applies posture centrally: off ⇒ nil source; strict +
	// unreachable workload API ⇒ error (runtime REFUSES to boot rather than come up
	// with a nil source that silently plaintext-dials — the
	// #pulse-outbound-mtls-fail-open fix); permissive + SPIRE down ⇒ nil source +
	// loud warn (the dial then degrades to insecure via grpcclient.Dial).
	if err := c.initMTLSSource(); err != nil {
		return nil, err
	}

	// Initialize handlers
	c.initHandlers()

	return c, nil
}

// initMTLSSource builds the one shared SPIFFE X509 source for runtime's outbound
// gRPC dials via the fail-closed platform-kit helper (v0.3.4). Posture is applied
// centrally in grpcclient.NewMeshSource: off ⇒ nil source; strict + unreachable
// workload API ⇒ error (runtime REFUSES to boot rather than serve with a nil
// source that silently plaintext-dials — the #pulse-outbound-mtls-fail-open fix);
// permissive + SPIRE down ⇒ nil source + loud warn (dials degrade via Dial).
func (c *Container) initMTLSSource() error {
	src, err := grpcclient.NewMeshSource(context.Background(), pkconfig.MTLSMode())
	if err != nil {
		return fmt.Errorf("init mtls mesh source (strict mesh dialing): %w", err)
	}
	c.canvasClientSource = src
	return nil
}

// initPosture declares runtime's real fail-closed security controls in the
// posture framework and proves them at boot (D-179 Wave-8). runtime runs a gRPC
// mesh listener (grpcserver.New), so its one named control is that the mesh must
// not be configured with mTLS off. The declared *posture.Set is retained for the
// /posture ops surface. MustHold fails closed: a control that does not hold, or
// an empty/invalid declaration, is itself a boot error.
func (c *Container) initPosture() error {
	set, err := posture.Declare(posture.Control{
		Name: "mesh-mtls",
		Assert: func(ctx context.Context) error {
			if pkconfig.MTLSMode() == pkconfig.MTLSModeOff {
				return fmt.Errorf("mesh service configured with mTLS off")
			}
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("declare mesh posture: %w", err)
	}
	c.Posture = set
	return set.MustHold(context.Background())
}

// initFleet wires the OCI→ext4 image-boot path and the FleetOrchestration gRPC
// service (runtime-fleet CP3). The Firecracker ImageBooter is only constructed
// when the firecracker executor is selected and a live FCProvider exists;
// otherwise the fail-loud booter rejects every call so image boot is never
// silently faked on a host without KVM.
func (c *Container) initFleet(cfg *config.Config) error {
	client := oci.NewClient(oci.Config{
		Host:     cfg.Registry.Host,
		Password: cfg.Registry.ServiceKey,
	})
	c.ImageMaterializer = oci.NewMaterializerAdapter(
		oci.NewMaterializer(client, cfg.ImageBoot.InitPath),
	)

	// D-124 — in-memory store of handed per-deployment registry PULL tokens. Always
	// constructed (independent of Vault): ProvisionApp stashes each app's token,
	// the replica runtime reads it at materialize, DecommissionApp drops it. Empty
	// (pre-cutover) leaves the pull on the shared registry service key (back-compat).
	c.fleetRegistryTokenStore = usecase.NewFleetRegistryTokenStore()

	// D-185a — one control-token store shared by the booter (which mints a token
	// per resident VM and delivers it inside the sealed secret push) and the
	// control client (which spends it). Two instances would mean a client that
	// finds no token for any VM.
	c.guestControlTokens = firecracker.NewGuestControlTokens()

	// CP4 §9#4 — durable host registry + this instance's self-registration. Both
	// run FIRST now, because the microVM addressing plane below cannot allocate an
	// address until this host knows its own identity AND its assigned net ordinal:
	// the ordinal is the high half of every net index, and there is no safe default
	// for it (0 is a real block another host may own). Self-registration is
	// firecracker-only, and so is the fatal host-identity check inside it — a
	// container-executor instance has no fleet identity to mint.
	c.FleetHostRegistry = usecase.NewFleetHostRegistry(c.HostRepo, c.NetLeaseRepo)
	if cfg.App.ExecutorType == "firecracker" {
		if err := c.registerSelfHost(cfg); err != nil {
			return err
		}
	} else {
		log.Println("Fleet self-registration skipped (executor is not firecracker)")
	}

	if cfg.App.ExecutorType == "firecracker" && c.FCProvider != nil {
		booter, verifyPlane, planeErr := c.initNetPlane(cfg)
		if planeErr != nil {
			log.Printf("ERROR: fleet microVM addressing plane UNRECONCILED — boots on this host are REFUSED while it stays that way (teardown, health and ingress still served; each boot re-checks, so the refusal clears itself when the cause does): %v", planeErr)
		}
		// The one boot fact /posture structurally cannot carry (see initNetPlane: a
		// posture control REFUSES boot, which is a worse posture than "refuse boots,
		// keep serving teardown"). As a gauge it is watchable without being fatal, and
		// each boot's precondition check re-publishes it.
		usecase.PublishNetPlaneReconciled(planeErr == nil)
		c.ImageBooter = publishImageBooter(booter, verifyPlane, planeErr)
		c.GuestControl = firecracker.NewGuestControlClient(c.guestControlTokens)
		log.Println("Image-boot Firecracker booter initialized")
	} else {
		c.ImageBooter = usecase.FailLoudImageBooter{}
		c.GuestControl = gateway.FailLoudGuestControl{}
		// Not 0: 0 means "customer boots are being refused on a fleet host", which
		// would be a false alarm on an instance that never boots a microVM at all.
		usecase.PublishNetPlaneNotApplicable()
		log.Println("Image-boot booter: fail-loud (firecracker executor not selected)")
	}

	// CP4 rt#9 — persistent-volume backing-file backend + manager. Real backend on
	// the firecracker host (mkfs.ext4 backing files); fail-loud otherwise so a
	// volume is never silently faked on a host without KVM.
	volumeDir := fleetVolumeDir(cfg)
	if cfg.App.ExecutorType == "firecracker" && c.FCProvider != nil {
		c.VolumeBackend = volumebackend.NewBackingStore()
		log.Printf("Fleet volume backend: backing files under %s", volumeDir)
	} else {
		c.VolumeBackend = usecase.FailLoudVolumeBackend{}
		log.Println("Fleet volume backend: fail-loud (firecracker executor not selected)")
	}
	c.FleetVolumeManager = usecase.NewFleetVolumeManager(c.VolumeRepo, c.VolumeBackend, volumeDir, c.FleetResourceRepo)

	// Report-only ledger↔reality audit over the SAME volume root. Deliberately not
	// gated on the executor: it reads, so there is nothing to fake, and a host that
	// once ran as a fleet host still carries the files whose rows are wrong. It is
	// scoped to this host's volumes by SetHostScope (below, after
	// self-registration) and does nothing without that identity. The object store
	// may be nil — the recovery-point direction then reports itself unaudited
	// rather than calling every backup missing.
	c.FleetLedgerReconciler = usecase.NewFleetLedgerReconciler(
		c.VolumeRepo,
		c.FleetResourceRepo,
		c.FleetAppRepo,
		c.snapshotArtifactStore,
		volumeDir,
	)

	// The durability gauges' collector. Deliberately not gated on the executor
	// either: the ledger it reads is the SHARED control plane, so any instance can
	// answer "how old is every resource's newest recovery point" — and an instance
	// that published nothing would leave the whole set absent, which reads as
	// healthy.
	c.FleetDurabilityMetrics = usecase.NewFleetDurabilityCollector(
		c.FleetResourceRepo,
		c.HostRepo,
		c.NetLeaseRepo,
	)

	// CP4.5 §9#5 — P21 fleet network fabric. The enforcer is the SINGLE WRITER of
	// the fleet's FORWARD program (see the netfabric package doc): it installs the
	// complete ordered program, restores every active system's chain from the DB
	// (the kernel does not remember our chains across a host reboot), and then
	// PROVES the result. Any failure flips the host to the fail-loud enforcer, so
	// every EnsureNetwork/ApplyPolicies and every Provision carrying a system_id is
	// refused. There is deliberately NO config flag here: membership is data and
	// enforcement is structure, so there is no name to typo into the permissive
	// branch.
	c.initNetworkFabric(cfg)

	c.FleetProvisionUC = usecase.NewFleetProvision(
		context.Background(),
		c.ImageWorkloadRepo,
		c.ImageMaterializer,
		c.ImageBooter,
		cfg.ImageBoot.WorkDir,
		cfg.ImageBoot.AdvertiseHost,
	)
	if cfg.Fleet.SecretSelfTest {
		c.FleetProvisionUC.SetSecretSelfTest(true)
		log.Println("Fleet secret vsock self-test ENABLED (APP_FLEET_SECRET_SELFTEST) — non-secret marker injected on provisions")
	}

	// #fleet-image-staging-dirs-no-gc — GC for the materialize staging root.
	// Deliberately NOT gated on the executor: both the replica path and the
	// test/job/fallback path stage under this same root, and a host that once ran
	// as a fleet host still carries their leftovers after a config change. It
	// needs BOTH repositories to tell an orphan from a live workload's rootfs.
	if cfg.ImageBoot.WorkDir != "" {
		c.FleetStagingSweeper = usecase.NewFleetStagingSweeper(c.ReplicaRepo, c.ImageWorkloadRepo, cfg.ImageBoot.WorkDir)
	}

	// CP4 §9#6 — resident replica runtime (firecracker host only; the fail-loud
	// booter would reject every boot off-host so it stays nil there).
	if cfg.App.ExecutorType == "firecracker" && c.FCProvider != nil {
		c.FleetReplicaRuntimeUC = usecase.NewFleetReplicaRuntime(
			c.ImageMaterializer,
			c.ImageBooter,
			c.ReplicaRepo,
			c.FleetAppRepo,
			cfg.ImageBoot.WorkDir,
			cfg.ImageBoot.AdvertiseHost,
		)
		// rt#9 — attach persistent data disks on resident boot.
		c.FleetReplicaRuntimeUC.SetVolumeManager(c.FleetVolumeManager)
		// D-124 — read each app's handed registry pull token at materialize.
		// Unconditional (not gated on Vault): registry pulls always happen.
		c.FleetReplicaRuntimeUC.SetRegistryTokenStore(c.fleetRegistryTokenStore)
		if cfg.Fleet.SecretSelfTest {
			c.FleetReplicaRuntimeUC.SetSecretSelfTest(true)
			log.Println("Fleet secret vsock self-test ENABLED on resident replica runtime (APP_FLEET_SECRET_SELFTEST) — non-secret marker injected on resident boots")
		}

		// runtime-fleet P3.4 — wire the per-tenant secret resolver (P14/I28/I29)
		// into the resident boot path. Degrade-not-crash: if Vault is not
		// configured or unreachable at boot, the resolver stays nil and only
		// secret-less apps boot here — a secret-bearing app then fails closed at
		// resolve time (ErrSecretResolverUnavailable), never silently secret-less.
		if resolver := c.buildSecretResolver(); resolver != nil {
			c.FleetReplicaRuntimeUC.SetSecretResolver(resolver)
			// D-125 — hand the in-memory token store to the boot path so
			// bootSecrets can stamp the deployment's handed token onto the resolver.
			c.FleetReplicaRuntimeUC.SetTokenStore(c.fleetTokenStore)
			log.Println("Fleet handed-token secret resolver wired on resident replica runtime (D-125)")

			// P7 RunJob seam — the one-shot job class resolves secret_refs through
			// this SAME resolver (a migrator needs its DSN). The job's per-deployment
			// Vault token arrives on each descriptor and is passed straight to the
			// resolve call, so no token store is needed here: unlike a resident app
			// (whose reconciler re-boots replicas later from a DB row that never
			// carries the token), a job resolves exactly once, in-process, at provision.
			c.FleetProvisionUC.SetSecretResolver(resolver)
			log.Println("Fleet handed-token secret resolver wired on the job path (P7 RunJob)")
		}
	}

	// CP4 §9#5 — placement scheduler.
	c.FleetScheduler = usecase.NewFleetScheduler(c.FleetHostRegistry, c.ReplicaRepo, c.FleetAppRepo, fleetHostStaleness(cfg))

	// CP4 §9#7 — reconciler-backed app→replicas model. Firecracker host only
	// (needs the replica runtime); off-host the resident class falls back to the
	// CP3 single-workload path in FleetProvision.
	if c.FleetReplicaRuntimeUC != nil {
		c.FleetOrchestratorUC = usecase.NewFleetOrchestrator(
			c.FleetAppRepo,
			c.ReplicaRepo,
			c.FleetScheduler,
			c.FleetReplicaRuntimeUC,
			// The P19 claim ledger: DecommissionApp refuses to tear down an app a
			// live durable resource backs (data loss with no recovery point).
			c.FleetResourceRepo,
		)
		// rt#9 — persistent-volume lifecycle (ensure/affinity/attach/degrade).
		c.FleetOrchestratorUC.SetVolumeManager(c.FleetVolumeManager)

		// CP4.5 §9#5 — P21 network membership gate on ProvisionApp + per-tick
		// re-resolution of each app's chain from its LIVE replica set.
		c.FleetOrchestratorUC.SetNetworkFabric(c.FleetNetworkFabricUC)

		// D-124 — stash each app's handed registry pull token at ProvisionApp and
		// drop it at DecommissionApp. Unconditional (not gated on Vault).
		c.FleetOrchestratorUC.SetRegistryTokenStore(c.fleetRegistryTokenStore)

		// D-125 — the orchestrator stashes each app's handed token at ProvisionApp
		// and revokes it at DecommissionApp (nil-safe when no Vault path is wired).
		c.FleetOrchestratorUC.SetTokenStore(c.fleetTokenStore)

		// rt#8 — fleet-owned ingress (D-079). The syncer drives a co-located Caddy
		// over its loopback admin API; construct it only on the firecracker host
		// (where Caddy runs), leaving it nil elsewhere. The route repo + base
		// domain are always wired so ProvisionApp records the host and returns the
		// stable https URL.
		var ingressSyncer usecase.IngressSyncer
		if cfg.App.ExecutorType == "firecracker" {
			ingressSyncer = caddy.NewSyncer(cfg.Fleet.Caddy.AdminEndpoint, cfg.Fleet.ActivatorEndpoint, cfg.Fleet.Caddy.AccessLogPath)
			log.Printf("Fleet ingress: Caddy syncer wired (admin=%s, domain=%s, activator=%s)", cfg.Fleet.Caddy.AdminEndpoint, cfg.Fleet.IngressDomain, cfg.Fleet.ActivatorEndpoint)

			// #fleet-scale-to-zero-activity-feed (D-122) — tail the Caddy access log
			// so SweepIdle skips draining an app served directly through Caddy. Only
			// when an access-log path is configured; its Run goroutine starts in Start.
			if cfg.Fleet.Caddy.AccessLogPath != "" {
				// Caddy runs co-located in this process (root on the KVM host) and
				// writes the access log here; ensure the parent dir exists so the
				// first Sync/tail does not fail on a missing directory. Non-fatal —
				// a missing dir only leaves the feed cold (SweepIdle fails safe).
				if err := os.MkdirAll(filepath.Dir(cfg.Fleet.Caddy.AccessLogPath), 0o755); err != nil {
					log.Printf("Warning: fleet activity feed: create access-log dir %q failed (%v) — feed will stay cold until Caddy can write it", filepath.Dir(cfg.Fleet.Caddy.AccessLogPath), err)
				}
				c.FleetActivityFeed = caddy.NewAccessLogFeed(cfg.Fleet.Caddy.AccessLogPath, 0)
				c.FleetOrchestratorUC.SetActivityFeed(c.FleetActivityFeed)
				log.Printf("Fleet activity feed: tailing Caddy access log (%s)", cfg.Fleet.Caddy.AccessLogPath)
			}
		}
		c.FleetOrchestratorUC.SetIngress(c.RouteRepo, cfg.Fleet.IngressDomain, ingressSyncer)

		c.FleetProvisionUC.SetOrchestrator(c.FleetOrchestratorUC)

		// rt#11 — scale-to-zero wake path. The activator resolves a woken request's
		// host to its app, scales it to one replica, and blocks until a resident
		// replica is health-passing (ActivateTimeout budget). Mounted on the HTTP
		// server in initHandlers.
		c.FleetActivatorUC = usecase.NewFleetActivator(
			c.RouteRepo,
			c.FleetAppRepo,
			c.ReplicaRepo,
			c.FleetOrchestratorUC,
			cfg.Fleet.ActivateTimeout,
		)
	}

	c.initResourceControlPlane(cfg)

	// D-184 — scope the boot-time restore sweep to the resources whose data lives
	// on THIS host, reusing the volume host-affinity the reconciler already uses
	// to decide whether a stateful app is ours. Wired only AFTER self-registration
	// (the host id does not exist before it); without it the sweep is a no-op, so
	// a restore live on another host is never stamped by this instance.
	if c.ResourceRestorer != nil && c.fleetSelf != nil && c.FleetVolumeManager != nil {
		c.ResourceRestorer.SetHostScope(c.fleetSelf.ID, c.FleetVolumeManager)
	}

	// Same scoping for the report-only ledger audit: it may only judge the volumes
	// pinned HERE. Off a fleet host it stays scope-less and reports nothing — a
	// file that is not on this filesystem proves nothing by being absent from it.
	if c.FleetLedgerReconciler != nil && c.fleetSelf != nil {
		c.FleetLedgerReconciler.SetHostScope(c.fleetSelf.ID)
	}
	return nil
}

// initResourceControlPlane wires the P19 durable resource control plane (CP4.5
// §9#3, D-183): the volume snapshotter (D-080), the dedicated-tier provisioner
// (R2), and the ResourceProvisioning gRPC handler.
//
// The dedicated path composes c.FleetProvisionUC, so it inherits the SAME
// fail-loud posture as the P7 workload seam: off the firecracker host
// FleetProvision boots through FailLoudImageBooter and every ProvisionDedicated
// is refused (ErrImageBootUnavailable) rather than silently faking a data-VM.
// The snapshotter needs a live guest control channel and so is
// firecracker-host-only; off-host it stays nil and the snapshot-first
// decommission of a durable resource fails closed (ErrResourceFinalSnapshotRequired).
// fleetHostStaleness bounds how old a host's last heartbeat may be for it to
// still count as LIVE: 3× the heartbeat interval, so a host that misses a couple
// of beats drops out of the candidate set.
//
// One function, two callers, on purpose. The scheduler decides where a replica
// goes and the standard-ha gate decides whether an `ha` claim is placeable at
// all; if those two ever disagreed about which hosts are live, the gate would
// admit claims the scheduler cannot satisfy — a promise accepted against a host
// that is already gone.
func fleetHostStaleness(cfg *config.Config) time.Duration {
	staleness := 3 * cfg.Fleet.HeartbeatInterval
	if staleness <= 0 {
		staleness = 3 * 30 * time.Second
	}
	return staleness
}

func (c *Container) initResourceControlPlane(cfg *config.Config) {
	// Volume snapshotter — firecracker host only (it quiesces the guest over the
	// real GuestControl client; off-host GuestControl is fail-loud, so every
	// attached-volume snapshot would be refused anyway) AND only with a configured
	// artifact store: the snapshotter calls store.Put unconditionally, so a nil
	// store here would panic on the first snapshot instead of failing closed. A nil
	// pointer is deliberate off-host; it is never wrapped in a non-nil interface
	// (see the explicit branches below).
	if cfg.App.ExecutorType == "firecracker" && c.FCProvider != nil && c.snapshotArtifactStore != nil {
		c.ResourceSnapshotter = usecase.NewFleetVolumeSnapshotter(
			c.GuestControl,
			c.snapshotArtifactStore,
			c.VolumeRepo,
			c.ReplicaRepo,
			c.FleetResourceRepo,
		)
	} else if cfg.App.ExecutorType == "firecracker" && c.FCProvider != nil {
		log.Println("Fleet resource snapshotter DISABLED: no snapshot artifact store configured (APP_SNAPSHOT_STORE_ENABLED) — snapshot/restore report unsupported")
	}

	// In-place restorer (D-184). Needs the orchestrator (stop/start the data VM)
	// and the artifact store (fetch the recovery point); nil without either, so
	// GetResourceCapabilities keeps reporting supports_restore honestly.
	if c.FleetOrchestratorUC != nil && c.snapshotArtifactStore != nil {
		c.ResourceRestorer = usecase.NewFleetVolumeRestorer(
			context.Background(),
			c.FleetResourceRepo,
			c.VolumeRepo,
			c.ReplicaRepo,
			c.FleetOrchestratorUC,
			c.FleetProvisionUC,
			c.snapshotArtifactStore,
		)
	}

	// Dedicated-tier provisioner (R2) — always wired. Pass the snapshotter as the
	// VolumeSnapshotter port ONLY when non-nil so DecommissionDedicated's nil-guard
	// stays honest (a typed-nil interface would defeat it).
	var snapPort usecase.VolumeSnapshotter
	if c.ResourceSnapshotter != nil {
		snapPort = c.ResourceSnapshotter
	}
	c.FleetResourceUC = usecase.NewFleetResourceProvisioner(
		c.FleetProvisionUC,
		c.FleetResourceRepo,
		c.ReplicaRepo,
		snapPort,
		// D-203 — the claim's ownership stamp over the backing app's volumes. The
		// manager is built earlier in initFleet, so it is non-nil here; a nil
		// binder refuses every dedicated provision by design.
		c.FleetVolumeManager,
		usecase.DedicatedEngineConfig{
			Registry:   cfg.Resource.EnginePGImageRegistry,
			Repository: cfg.Resource.EnginePGImageRepository,
			Digest:     cfg.Resource.EnginePGImageDigest,
			ConnBudget: cfg.Resource.ConnBudget,
		},
		// The permanent customer-facing naming context (D-190). Unset ⇒ every
		// dedicated provision is refused rather than minting a name under a guessed
		// zone or region.
		domain.EndpointNaming{
			Zone:   cfg.Resource.EndpointZone,
			Region: cfg.Resource.EndpointRegion,
		},
		// The live-host inventory the standard-ha placement gate refuses on
		// (slice 1). The SAME registry and the SAME staleness the scheduler uses, so
		// the gate can never admit a claim over a host set placement would reject.
		c.FleetHostRegistry,
		fleetHostStaleness(cfg),
	)

	// D-200 — the CONTROL-PLANE recovery-point mirror. Nil on every fleet host and
	// whenever no second domain is configured, which leaves recovery points recorded
	// primary_only: the truth, and visible as such on
	// sentiae_fleet_recovery_points_by_location.
	c.RecoveryPointMirror = c.newRecoveryPointMirrorWorker(cfg)

	// ResourceProvisioning handler. The shared-tier provisioner is intentionally
	// nil here: constructing its testdb.Provisioner backend requires shared-engine
	// admin credentials (user/password/admin-db/template) that ResourceConfig does
	// not carry — a NEEDS-DECISION escalated to the session lead. Until that lands,
	// the shared route answers Unavailable (an honest "not configured" rather than
	// a silent fake). Pass a true-nil snapshotter off the firecracker host so the
	// handler's Unavailable guard fires.
	//
	// Same typed-nil care for the snapshotter and the restorer: a nil *T wrapped
	// in a non-nil interface would defeat the handler's Unavailable guards.
	var snapHandler grpchandler.ResourceSnapshotPort
	if c.ResourceSnapshotter != nil {
		snapHandler = c.ResourceSnapshotter
	}
	var restoreHandler grpchandler.ResourceRestorePort
	if c.ResourceRestorer != nil {
		restoreHandler = c.ResourceRestorer
	}
	c.ResourceServer = grpchandler.NewResourceServer(c.FleetResourceUC, nil, snapHandler, restoreHandler, c.FleetResourceRepo)
}

// initNetworkFabric wires the P21 fleet network fabric (CP4.5 §9#5, D-164).
//
// The sequence is install → restore → PROVE, and every step is a gate: the host
// only keeps the real enforcer if it can install the complete FORWARD program,
// rebuild every active system chain from the DB, and then prove the resulting
// layout matches the intended program exactly. Anything else flips it to
// FailLoudNetworkEnforcer, which refuses every fabric call and every provision
// carrying a system_id.
//
// Failing here is safe by construction: apps with no system_id are untouched
// (they reach no peer either way), and system-scoped apps simply do not boot.
// Replicas already resident keep running with no system chain — their peers are
// unreachable. Broken-CLOSED. A restart degrades to isolation, never to reach.
func (c *Container) initNetworkFabric(cfg *config.Config) {
	failLoud := func(reason string, err error) {
		log.Printf("Fleet network enforcer: FAIL-LOUD (%s: %v) — system-scoped workloads will be REFUSED on this host", reason, err)
		c.NetworkEnforcer = usecase.FailLoudNetworkEnforcer{}
		c.FleetNetworkFabricUC = usecase.NewFleetNetworkFabric(
			c.FleetNetworkRepo, c.FleetNetworkPolicyRepo, c.FleetAppRepo, c.ReplicaRepo, c.NetworkEnforcer,
		)
	}

	if cfg.App.ExecutorType != "firecracker" || c.FCProvider == nil {
		failLoud("firecracker executor not selected", nil)
		return
	}

	ctx := context.Background()
	enforcer := netfabric.NewIPTablesEnforcer()
	c.NetworkEnforcer = enforcer
	c.FleetNetworkFabricUC = usecase.NewFleetNetworkFabric(
		c.FleetNetworkRepo, c.FleetNetworkPolicyRepo, c.FleetAppRepo, c.ReplicaRepo, c.NetworkEnforcer,
	)

	if err := enforcer.InstallSkeleton(ctx); err != nil {
		failLoud("install FORWARD program", err)
		return
	}
	if err := c.FleetNetworkFabricUC.RestoreAll(ctx); err != nil {
		failLoud("restore system chains from store", err)
		return
	}
	// Prove it; do not assume it. This is the step that catches a FORWARD program
	// some other writer reordered underneath us.
	if err := enforcer.AssertPosture(ctx); err != nil {
		failLoud("prove FORWARD program", err)
		return
	}
	log.Println("Fleet network enforcer: iptables program installed, restored, and PROVEN")
}

// buildSecretResolver constructs the per-tenant envelope resolver used to
// resolve a resident app's secret_refs at boot (P14). It authenticates to Vault
// via SPIFFE JWT-SVID (as svc/runtime) using the standard VAULT_* env vars, then
// wraps a KV getter + a decrypt-only per-tenant Transit KEK (transit-tenants,
// AutoCreate:false → decrypt fails closed, I29). It returns nil (never crashes)
// when Vault is unconfigured or unreachable at boot: a secret-less app still
// boots, and a secret-bearing app then fails closed at resolve time. The built
// VaultClient is retained on the container so Close stops its lease renewer, and
// the resolver rides that same primary client's transport (see below).
func (c *Container) buildSecretResolver() secret.Resolver {
	if os.Getenv("VAULT_ADDR") == "" || os.Getenv("VAULT_AUTH_MODE") == "" {
		log.Println("Fleet secret resolver: VAULT_ADDR/VAULT_AUTH_MODE unset — resident secret_refs will fail closed")
		return nil
	}
	vc, err := pkconfig.NewFromEnv(context.Background())
	if err != nil {
		log.Printf("Warning: fleet secret resolver: build Vault client failed (%v) — resident secret_refs will fail closed", err)
		return nil
	}
	c.vaultClient = vc
	// D-125 (executes D-089): the fleet host holds NO mint capability. It no
	// longer mints a per-org child token (the D-085 ScopedEnvelopeVaultResolver
	// path is dropped). Instead delivery mints the per-deployment secret-broker
	// token and hands it down the descriptor; the HandedTokenEnvelopeResolver
	// decrypts under that handed token (carried on secret.Principal.Token by
	// bootSecrets from the in-memory token store). A stolen svc/runtime credential
	// can no longer mint a child for any org.
	//
	// The token store renews each handed token via renew-self and revokes it on
	// Decommission, over the fleet's own Vault client (used only for address+TLS
	// to clone; the handed token governs the renew/revoke ACL). It is wired into
	// the replica runtime + orchestrator by the caller.
	c.fleetTokenStore = usecase.NewFleetSecretTokenStore(
		context.Background(),
		vaulttoken.New(vc.Raw()),
		0, // default renewal cadence (30m; self-adjusts to the token's granted TTL)
	)
	// The resolver rides the PRIMARY client (vc) for address + transport only: its
	// per-call Clone leaves CloneToken false, so no token comes with it and the
	// handed per-deployment token still governs every read (the host stays a
	// bearer, never a minter — D-089 above). What the transport buys is CA
	// rotation: in svid mode vc's TLS verifies against the live X509Source on every
	// handshake, whereas building a client from vault.DefaultConfig() snapshots
	// VAULT_CACERT into an *x509.CertPool once at process start — and this resolver
	// outlives the process's boot, so that snapshot went stale on every daily SPIRE
	// CA rotation and no data-VM could boot until a restart.
	return secret.NewHandedTokenEnvelopeResolverWithClient(vc.Raw(), "secret", "transit-tenants")
}

// newRecoveryPointMirrorWorker builds the CONTROL-PLANE recovery-point mirror
// (D-200) — the component that gives the fleet `failure_domains = 2`.
//
// ⚠ WHERE IT RUNS, AND WHY THAT IS THE WHOLE POINT. Every recovery point lands in
// the MinIO container on the SAME PHYSICAL CHASSIS as the fleet host whose data it
// protects, so `failure_domains = 1` and every durability promise was arithmetic
// over one machine. The obvious fix — copy to Cloudflare R2 from the host that took
// the snapshot — was REJECTED: it would give a tenant-adjacent machine a standing,
// whole-bucket, all-tenant off-chassis credential, converting D-125's deliberate
// non-exposure into a new class of exposure. So the copy runs HERE, on the control
// plane, which already holds the ledger and the primary object store and is already
// the all-tenant TCB. The fleet host ends up with NO off-chassis object-store
// credential at all.
//
// ⚠ THE GATE IS THE EXECUTOR TYPE, matching registerSelfHost exactly. A Firecracker
// instance is a fleet host and gets no worker; anything else is the single mesh
// instance. One seam, one predicate (isControlPlaneInstance), so there is no second
// name to typo into the wrong branch.
//
// ⚠ SINGLE WRITER BY CONSTRUCTION: exactly one control-plane instance exists, so no
// claiming protocol is built (see RecoveryPointMirrorWorker's note on why a second
// one would duplicate work but never corrupt the ledger).
//
// ⚠ NOTHING HERE MAY ENUMERATE THE BUCKET. The D-199 token grants object access and
// not bucket listing (LIST returns 403, verified live), which is why the store is
// built with SkipBucketProbe. The ledger is the source of truth for what exists off
// the chassis; the bucket cannot be asked.
//
// Returns nil on every miss, which leaves recovery points stamped primary_only —
// true, and counted as such by
// sentiae_fleet_recovery_points_by_location{locations="primary_only"} plus the
// oldest-single-domain age gauge. Refusing to boot instead would take the control
// plane away from customer VMs that are already running, and it is the LEDGER being
// truthful, not the mirror existing, that keeps the durability numbers honest.
func (c *Container) newRecoveryPointMirrorWorker(cfg *config.Config) *usecase.RecoveryPointMirrorWorker {
	if !isControlPlaneInstance(cfg.App.ExecutorType) {
		// Not a warning. A fleet host has no business copying off its own chassis, and
		// after D-200 it does not even hold a credential to try with.
		log.Println("[recovery-point-mirror] not this instance: the second-domain copy runs on the CONTROL PLANE (D-200), never on a Firecracker fleet host")
		return nil
	}
	sd := cfg.SecondDomain
	if !sd.Enabled {
		log.Println("[recovery-point-mirror] DISABLED (APP_SECOND_DOMAIN_ENABLED unset) — every recovery point will exist in ONE failure domain (the fleet chassis) and is recorded primary_only")
		return nil
	}
	if c.snapshotArtifactStore == nil {
		log.Println("[recovery-point-mirror] DISABLED: no PRIMARY artifact store on this instance to read the blobs out of — every recovery point stays recorded primary_only")
		return nil
	}
	// Every field is required. A partially-configured mirror is worse than none: it
	// would be wired, non-nil, and fail on every single recovery point.
	var missing []string
	for name, v := range map[string]string{
		"bucket": sd.Bucket, "endpoint": sd.Endpoint,
		"access_key": sd.AccessKey, "secret_key": sd.SecretKey, "region": sd.Region,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		log.Printf("[recovery-point-mirror] DISABLED: APP_SECOND_DOMAIN_ENABLED is set but %s missing — every recovery point will exist in ONE failure domain and is recorded primary_only",
			strings.Join(missing, ","))
		return nil
	}

	host, useSSL, err := s3EndpointHost(sd.Endpoint)
	if err != nil {
		log.Printf("[recovery-point-mirror] DISABLED: %v — every recovery point will exist in ONE failure domain and is recorded primary_only", err)
		return nil
	}
	secondary, err := objectstore.NewS3ArtifactStore(objectstore.S3Config{
		Endpoint:  host,
		Region:    sd.Region,
		Bucket:    sd.Bucket,
		AccessKey: sd.AccessKey,
		SecretKey: sd.SecretKey,
		UseSSL:    useSSL,
		// The token cannot HeadBucket or CreateBucket, and CreateBucket must never be
		// attempted: a bucket this code created would not carry the object lock that is
		// the whole durability control.
		SkipBucketProbe: true,
	})
	if err != nil {
		log.Printf("[recovery-point-mirror] DISABLED: build the second-domain store failed (%v) — every recovery point will exist in ONE failure domain and is recorded primary_only", err)
		return nil
	}
	// The domain label recorded on every row it protects. Host, not bucket alone: the
	// question during an incident is which PROVIDER holds the copy.
	label := "r2:" + sd.Bucket
	mirror, err := usecase.NewArtifactStoreMirror(c.snapshotArtifactStore, secondary, label)
	if err != nil {
		log.Printf("[recovery-point-mirror] DISABLED: %v — every recovery point will exist in ONE failure domain and is recorded primary_only", err)
		return nil
	}
	worker, err := usecase.NewRecoveryPointMirrorWorker(c.FleetResourceRepo, mirror)
	if err != nil {
		log.Printf("[recovery-point-mirror] DISABLED: %v — every recovery point will exist in ONE failure domain and is recorded primary_only", err)
		return nil
	}
	log.Printf("[recovery-point-mirror] ENABLED on the control plane: primary_only recovery points are copied to %s (%s), oldest first, and confirmed by checksum before the ledger claims two failure domains", label, host)
	return worker
}

// isControlPlaneInstance reports whether THIS process is the single mesh instance
// rather than a Firecracker fleet host.
//
// The same predicate registerSelfHost is gated on, deliberately: a fleet host runs
// executor_type=firecracker and self-registers, and the mesh instance does neither.
// A pure function so the D-200 gate is assertable without a live container — the
// failure this guards against is a wiring miss, and a wiring miss is exactly what
// unit tests of the surrounding code never see.
func isControlPlaneInstance(executorType string) bool { return executorType != "firecracker" }

// s3EndpointHost splits an S3 endpoint into the host:port minio-go wants and
// whether to use TLS. A bare host (no scheme) is assumed HTTPS — for an
// off-premises credential, guessing plaintext would put the secret key on the wire.
func s3EndpointHost(endpoint string) (host string, useSSL bool, err error) {
	if !strings.Contains(endpoint, "://") {
		return endpoint, true, nil
	}
	u, perr := url.Parse(endpoint)
	if perr != nil || u.Host == "" {
		return "", false, fmt.Errorf("second-domain endpoint %q is not a usable URL", endpoint)
	}
	switch u.Scheme {
	case "https":
		return u.Host, true, nil
	case "http":
		return u.Host, false, nil
	default:
		return "", false, fmt.Errorf("second-domain endpoint %q has unsupported scheme %q", endpoint, u.Scheme)
	}
}

// publishImageBooter decides what the fleet's booter seam becomes given the
// addressing plane's outcome. It is a pure function so the fail-closed rule is
// assertable without a live host.
//
// Every boot passes a PRECONDITION that is re-derived from the plane, never a
// stored verdict: `verify` is consulted per boot, so a host whose violation has
// been resolved starts serving again on its own. That is the whole difference
// between fail-closed and stuck — the boot-time reconcile's error used to be frozen
// into this seam, and a host then refused every boot for as long as the process
// lived, citing a row that no longer existed.
//
// A nil `verify` is the one case that latches, and it must: it means the plane
// could not be CONSTRUCTED (no host identity, no assigned ordinal), and the
// allocator was built with that same missing input, so nothing this process can
// observe would change the answer. planeErr then carries the reason.
//
// Teardown is DELEGATED on every path. Refusing to boot protects customer data,
// whereas refusing to tear down protects nothing, strands a running VM together
// with its /30, its lease and its rootfs — and is how an operator clears the very
// row a refusal names.
func publishImageBooter(real usecase.ImageBooter, verify usecase.NetPlaneVerifier, planeErr error) usecase.ImageBooter {
	if verify == nil {
		reason := planeErr
		if reason == nil {
			// Unreachable through initNetPlane, and refused rather than trusted: a
			// booter with no plane check is the fail-OPEN shape of this seam.
			reason = fmt.Errorf("%w: no addressing-plane verifier was wired for this host", domain.ErrNetPlaneUnreconciled)
		}
		return usecase.FailLoudImageBooter{
			Reason:   fmt.Errorf("%w: %v", domain.ErrNetPlaneUnreconciled, reason),
			Teardown: real,
		}
	}
	return usecase.NetPlaneGuardedImageBooter{Real: real, Verify: verify}
}

// initNetPlane wires the durable microVM addressing plane (SentiaeDB Phase 0) and
// reconciles it against this host's reality. It returns the real booter — ALWAYS
// non-nil, so the caller has something to delegate teardown to — plus the error
// that must make every boot fail closed.
//
// ⚠ WHAT THIS REPLACES, and why the fail-closed posture is not optional. Boots
// used to draw their /30, TAP name, jail id and per-VM uid from a process-local
// map inside the booter, SEEDED here from the net_index columns of live rows. Both
// seed calls logged their errors and continued — so a DB blip at startup produced
// an EMPTY used-set, and the next boot allocated index 1, which a live customer VM
// already held: same address, same uid/gid, same chroot. The seed also never
// covered `dead` replicas, whose VMM RefreshHealth leaves running. The plane is
// now a table with unique fences, and the only honest response to "I cannot tell
// which addresses are held" is to hand out none.
//
// The sequence is: resolve this host's ordinal → build the allocator → build the
// booter over it → reconcile the host's leases. A failure at any step is returned
// and logged loudly (the initNetworkFabric precedent); the caller publishes a seam
// that refuses boots. It also returns the plane VERIFIER, which is what re-asks the
// question on every boot so a refusal cannot outlive its cause — see
// publishImageBooter.
func (c *Container) initNetPlane(cfg *config.Config) (*firecracker.ImageBooter, usecase.NetPlaneVerifier, error) {
	ctx := context.Background()

	// A negative ordinal is the "unknown" sentinel the allocator refuses to
	// allocate from. It is carried rather than short-circuited so the booter (and
	// therefore teardown, which releases leases by owner and needs no ordinal) is
	// still built on every failure path.
	ordinal := -1
	var planeErr error

	selfHost := uuid.Nil
	if c.fleetSelf != nil {
		selfHost = c.fleetSelf.ID
	}
	switch {
	case selfHost == uuid.Nil:
		// Self-registration is fatal on missing identity and only swallows an
		// unmeasurable host, so reaching here means this host never joined the
		// registry — it has no block to allocate from.
		planeErr = fmt.Errorf("this fleet host is not registered, so it has no addressing block: %w", domain.ErrHostNetOrdinalUnset)
	case c.fleetSelf.NetOrdinal == nil:
		planeErr = fmt.Errorf("host %s has no assigned net ordinal: %w", selfHost, domain.ErrHostNetOrdinalUnset)
	default:
		ordinal = *c.fleetSelf.NetOrdinal
	}

	alloc := usecase.NewFleetNetAllocator(
		c.NetLeaseRepo,
		selfHost,
		ordinal,
		cfg.Firecracker.VMUIDBase,
		cfg.Firecracker.VMUIDSpan,
	)
	booter := firecracker.NewImageBooter(
		c.FCProvider,
		cfg.ImageBoot.AdvertiseHost,
		c.guestControlTokens,
		alloc,
	)
	if planeErr != nil {
		// No verifier: the missing input (identity / ordinal) is read once at boot and
		// the allocator above was built with it, so re-deriving would change nothing.
		return booter, nil, planeErr
	}

	reconciler := usecase.NewFleetNetLeaseReconciler(
		c.NetLeaseRepo,
		c.HostRepo,
		c.ReplicaRepo,
		c.ImageWorkloadRepo,
		// The reconcile tears a reclaimed lease's VM down through the REAL booter
		// (never the fail-loud one), because a lease is only released after its VM is
		// actually stopped.
		booter,
		booter,
		selfHost,
		cfg.Firecracker.VMUIDBase,
	)
	// The verifier is returned on EVERY path below, including the failing one: a
	// reconcile that refuses today must be re-asked tomorrow, or the host stays
	// refused after its cause is gone (the latch this returns instead of).
	verify := usecase.NetPlaneVerifier(reconciler.Verify)

	report, err := reconciler.Reconcile(ctx)
	if err != nil {
		return booter, verify, err
	}
	// Logged, not stashed: /posture reports DECLARED CONTROLS (opshttp.PostureHandler
	// over a posture.Set), and a control there is asserted by MustHold, which
	// REFUSES BOOT when it does not hold. Declaring the plane's state that way would
	// turn "refuse every boot, keep serving teardown/health/ingress" into "do not
	// come up at all", which is a different and worse posture than the one this
	// change is specified to have. The report therefore stays a boot-log fact until
	// the ops surface grows a non-fatal report channel.
	log.Printf("Fleet microVM addressing plane: host_ordinal=%d leases=%d adopted=%d torn_down=%d reclaimed=%d left=%d",
		report.HostOrdinal, report.Leases, report.Adopted, report.TornDown, report.Reclaimed, report.Left)
	return booter, verify, nil
}

// fleetVolumeDir resolves the host root under which per-volume backing files are
// materialized. Shared by the volume manager and the capacity measurement so the
// disk that is ADVERTISED is the disk the volumes are actually written to.
func fleetVolumeDir(cfg *config.Config) string {
	if cfg.Fleet.VolumeDir != "" {
		return cfg.Fleet.VolumeDir
	}
	return filepath.Join(cfg.Firecracker.SnapshotPath, "volumes")
}

// resolveFleetHostID resolves this host's fleet identity from configuration. It
// is REQUIRED and it is never derived.
//
// A derived id used to be the fallback (UUIDv5 over the advertise host). That is
// a routable-address-derived identity, and it is wrong in the one direction that
// costs data: a host that gets a new IP mints a SECOND identity, re-registers as
// a fresh empty host, and every resource whose volumes live on its disks is now
// pinned to a host id nothing answers for — while the scheduler happily places
// new customer databases on the "new" host. The reverse is just as bad: two hosts
// briefly sharing an advertise address collide onto ONE registry row.
//
// Host identity therefore has to be minted at host BIRTH and pinned in the host's
// env file (config-as-code, §34), which makes an empty or unparseable value a
// fatal MISCONFIGURATION rather than something to paper over. There is no
// fallback branch to guard, because a fallback is how the permissive branch
// survives.
func resolveFleetHostID(raw string) (uuid.UUID, error) {
	if strings.TrimSpace(raw) == "" {
		return uuid.Nil, fmt.Errorf("APP_FLEET_HOST_ID is not set: a fleet host's identity must be minted at host birth and pinned in its env file, never derived from a routable address (a re-IP'd host would mint a second identity and orphan every volume pinned to the first)")
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("APP_FLEET_HOST_ID %q is not a valid uuid: %w", raw, err)
	}
	if id == uuid.Nil {
		return uuid.Nil, fmt.Errorf("APP_FLEET_HOST_ID is the nil uuid: a fleet host's identity must be a real, minted uuid")
	}
	return id, nil
}

// registerSelfHost registers this runtime-service instance as a fleet host and
// records the self-host + heartbeat cadence for the background loop.
//
// It returns an error ONLY for a fatal misconfiguration (an absent/invalid
// APP_FLEET_HOST_ID): that refuses boot, because a fleet host with no minted
// identity must not run at all. Every other failure — a host that cannot measure
// itself, a registry that is unreachable — is logged and swallowed as before:
// self-registration is not on the boot critical path, and the host simply stays
// out of the placement candidate set.
//
// The advertised capacity is MEASURED off this machine, never asserted by config.
// It used to be neither: disk came from a fixed 51200MB default (on a host with
// 40GB, ~22GB of it free) and cpu/memory came from Firecracker.Max{VCPU,MemMB},
// which are the PER-MICROVM ceiling — the same two values the per-VM bin-packer
// reads as one VM's maximum. Advertising a per-VM ceiling as the host total is a
// coincidence when it is close and an over-advertisement when it is not, and the
// scheduler places customer databases on it. Config now only NARROWS the
// measurement (see domain.ResolveHostCapacity).
func (c *Container) registerSelfHost(cfg *config.Config) error {
	// Identity FIRST: it is the one fatal condition here, and it is a pure config
	// fact. Resolving it after the capacity measurement would let a host that
	// cannot measure itself return early and never notice it has no identity
	// either — the misconfiguration would surface only on some later boot.
	hostID, err := resolveFleetHostID(cfg.Fleet.HostID)
	if err != nil {
		return fmt.Errorf("fleet host identity: %w", err)
	}

	volumeDir := fleetVolumeDir(cfg)
	measured, merr := hostcapacity.Measure(volumeDir)
	if merr != nil {
		// Fail closed: a host that cannot measure itself advertises nothing. It stays
		// out of the placement candidate set instead of joining it with invented
		// numbers, and the process keeps running (self-registration never was on the
		// boot critical path).
		log.Printf("ERROR: fleet self-registration REFUSED — this host cannot measure its own capacity (volume dir %s), so it will not advertise any: %v", volumeDir, merr)
		return nil
	}
	capacity, cerr := domain.ResolveHostCapacity(measured, domain.HostCapacityOverride{
		VCPU:          cfg.Fleet.HostVCPU,
		MemMB:         cfg.Fleet.HostMemMB,
		DiskMB:        cfg.Fleet.HostDiskMB,
		DiskReserveMB: cfg.Fleet.HostDiskReserveMB,
	})
	if cerr != nil {
		log.Printf("ERROR: fleet self-registration REFUSED — configured capacity is not honest about this host (measured vcpu=%d mem=%dMB disk_total=%dMB disk_free=%dMB on %s; configured vcpu=%d mem=%dMB disk=%dMB reserve=%dMB): %v",
			measured.VCPU, measured.MemTotalMB, measured.DiskTotalMB, measured.DiskAvailableMB, volumeDir,
			cfg.Fleet.HostVCPU, cfg.Fleet.HostMemMB, cfg.Fleet.HostDiskMB, cfg.Fleet.HostDiskReserveMB, cerr)
		return nil
	}
	// Log the measurement NEXT TO the override and the result, once, so the
	// advertised number is observable rather than inferred from a config file that
	// may not even set the keys.
	log.Printf("Fleet host capacity: measured vcpu=%d mem=%dMB disk_total=%dMB disk_free=%dMB (fs of %s) | configured vcpu=%d mem=%dMB disk=%dMB reserve=%dMB | advertising vcpu=%d mem=%dMB disk=%dMB",
		measured.VCPU, measured.MemTotalMB, measured.DiskTotalMB, measured.DiskAvailableMB, volumeDir,
		cfg.Fleet.HostVCPU, cfg.Fleet.HostMemMB, cfg.Fleet.HostDiskMB, cfg.Fleet.HostDiskReserveMB,
		capacity.VCPU, capacity.MemMB, capacity.DiskMB)

	endpoint := fmt.Sprintf("%s:%s", cfg.ImageBoot.AdvertiseHost, cfg.Server.GRPC.Port)
	self := domain.Host{
		ID:     hostID,
		Region: cfg.Fleet.Region,
		// The structured placement fact (D-196). Unset ⇒ RegisterHost REFUSES below
		// and this host stays out of the live inventory rather than joining it with
		// an unknowable failure domain. That is the intended posture: an
		// un-attested host is not a candidate for a tier whose promise is that two
		// members do not die together.
		FailureDomain:  cfg.Fleet.FailureDomain,
		CapacityVCPU:   capacity.VCPU,
		CapacityMemMB:  capacity.MemMB,
		CapacityDiskMB: capacity.DiskMB,
		Endpoint:       endpoint,
	}

	registered, rerr := c.FleetHostRegistry.RegisterHost(context.Background(), self)
	if rerr != nil {
		// The placement facts get their own line: without a registration this host
		// never heartbeats, so it falls out of the live inventory and the scheduler
		// stops placing on it — a large, correct consequence that must not be
		// misdiagnosed as a transient DB error. Name the env key, because the fix is
		// one line of config and nothing else can supply the fact.
		if errors.Is(rerr, domain.ErrHostFailureDomainRequired) || errors.Is(rerr, domain.ErrHostFailureDomainInvalid) {
			log.Printf("ERROR: fleet self-registration REFUSED — this host has not stated its failure domain (APP_FLEET_FAILURE_DOMAIN, structured as site/power/network e.g. rgalileo-room/breaker-a/switch-1; got %q). It will NOT heartbeat and the scheduler will NOT place on it: %v",
				cfg.Fleet.FailureDomain, rerr)
			return nil
		}
		if errors.Is(rerr, domain.ErrHostRegionRequired) {
			log.Printf("ERROR: fleet self-registration REFUSED — this host has no region (APP_FLEET_REGION). It will NOT heartbeat and the scheduler will NOT place on it: %v", rerr)
			return nil
		}
		log.Printf("Warning: fleet self-registration failed (continuing without it): %v", rerr)
		return nil
	}
	c.fleetSelf = &registered
	c.fleetHeartbeatEvery = cfg.Fleet.HeartbeatInterval
	if c.fleetHeartbeatEvery <= 0 {
		c.fleetHeartbeatEvery = 10 * time.Second
	}
	log.Printf("Fleet self-registered: host_id=%s region=%s capacity=%dvcpu/%dMB/%dMB endpoint=%s",
		registered.ID, registered.Region, capacity.VCPU, capacity.MemMB, capacity.DiskMB, endpoint)
	return nil
}

// initDatabase initializes the database connection
func (c *Container) initDatabase(cfg *config.Config) error {
	logLevel := logger.Silent
	if cfg.App.Environment == "development" {
		logLevel = logger.Info
	}

	port := 5432
	if p, err := strconv.Atoi(cfg.Database.Postgres.Port); err == nil {
		port = p
	}

	dbConfig := postgres.Config{
		Host:            cfg.Database.Postgres.Host,
		Port:            port,
		User:            cfg.Database.Postgres.User,
		Password:        cfg.Database.Postgres.Password,
		Database:        cfg.Database.Postgres.Database,
		SSLMode:         cfg.Database.Postgres.SSLMode,
		MaxOpenConns:    cfg.Database.Postgres.Pool.MaxOpenConns,
		MaxIdleConns:    cfg.Database.Postgres.Pool.MaxIdleConns,
		ConnMaxLifetime: cfg.Database.Postgres.Pool.MaxLifetime,
		ConnMaxIdleTime: 5 * time.Minute,
		LogLevel:        logLevel,
	}

	db, err := postgres.NewDB(dbConfig)
	if err != nil {
		return err
	}

	c.DB = db
	log.Println("Database connection initialized successfully")

	// Run golang-migrate migrations (durable path, idempotent) — the SOLE
	// schema authority (D-178). Every runtime table, core + fleet control-plane,
	// is owned by migrations/ now; GORM AutoMigrate was retired.
	version, applied, err := postgres.RunMigrations(db)
	if err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}
	log.Printf("Migrations complete (version=%d applied=%t)", version, applied)

	return nil
}

// initInfrastructure initializes infrastructure providers
func (c *Container) initInfrastructure(cfg *config.Config) {
	switch cfg.App.ExecutorType {
	case "firecracker":
		c.FCProvider = firecracker.NewProvider(cfg.Firecracker)
		c.VMProvider = c.FCProvider

		// Choose execution mode
		if cfg.App.UseVsock {
			// Vsock agent with warm pool — VMs are reused across tasks
			fcListener := vmcomm.NewFirecrackerListener()
			c.VsockPool = vmcomm.NewPool(c.FCProvider, fcListener, 1)
			c.ExecutionRunner = vmcomm.NewPoolRunner(c.VsockPool)
			log.Println("Firecracker with vsock agent + warm pool execution")
		} else {
			// Rootfs-injection — each task gets a fresh disposable VM
			c.ExecutionRunner = c.FCProvider
			log.Println("Firecracker with rootfs-injection execution")
		}
		log.Println("Firecracker provider initialized")

		// §9.1 — wire warm pools per language profile if configured.
		c.initFirecrackerPools(cfg)
		// §9.3 — wire per-VM auto-snapshot scheduler.
		c.initFirecrackerCheckpointScheduler(cfg)
	case "simulated":
		// Simulated provider — no Docker or Firecracker needed. This is an
		// EXPLICIT operator choice, but it still FAKES execution (echoes code
		// back, exit 0) — code nodes do not really run and are not sandboxed.
		simProv := simulated.NewProvider()
		c.VMProvider = simProv
		c.ExecutionRunner = simProv
		log.Println("WARNING: SIMULATED executor selected — code nodes are FAKED (not executed, not sandboxed).")
	default:
		// Default to container provider (works on macOS and Linux)
		if dockerAvailable() {
			c.ContainerProvider = container.NewProvider(cfg.Container)
			c.VMProvider = c.ContainerProvider
			c.ExecutionRunner = c.ContainerProvider
			log.Println("Docker container provider initialized (hardened untrusted-code sandbox)")
		} else {
			// FAIL LOUD: no Docker → no real sandbox. We deliberately do NOT
			// fall back to in-process execution (running untrusted bodies in
			// this process is forbidden). The simulated provider FAKES runs,
			// so unsandboxed/unexecuted runs must never be mistaken for real
			// ones — make that unmissable in the logs.
			log.Println("================================================================")
			log.Println("WARNING: Docker is NOT available — falling back to SIMULATED executor.")
			log.Println("WARNING: code nodes are NOT really executing and NOT sandboxed.")
			log.Println("WARNING: execution results are FAKE (code echoed back, exit code 0).")
			log.Println("WARNING: install Docker or set APP_EXECUTOR_TYPE=firecracker for")
			log.Println("WARNING: real, isolated code execution before serving users.")
			log.Println("================================================================")
			simProv := simulated.NewProvider()
			c.VMProvider = simProv
			c.ExecutionRunner = simProv
		}
	}

	// Initialize Kafka event publisher
	if cfg.Kafka.Enabled && len(cfg.Kafka.Brokers) > 0 {
		publisher, err := messaging.NewKafkaPublisher(kafka.PublisherConfig{
			Brokers: cfg.Kafka.Brokers,
			Source:  "runtime-service",
		})
		if err != nil {
			log.Printf("Warning: failed to initialize Kafka publisher: %v (using noop)", err)
			c.EventPublisher = &messaging.NoopPublisher{}
		} else {
			c.EventPublisher = publisher
			log.Println("Kafka event publisher initialized")
			ensureCtx, ensureCancel := context.WithTimeout(context.Background(), 15*time.Second)
			if err := publisher.EnsureTopics(ensureCtx); err != nil {
				log.Printf("Warning: Kafka EnsureTopics failed: %v (continuing)", err)
			}
			ensureCancel()
		}

		// Initialize inbound event consumer (canvas → runtime).
		// Platform-kit derives the Kafka topic from event type as
		// "{prefix}.{domain}.{resource}", so the event
		// "sentiae.canvas.code.execute_requested" is published to topic
		// "sentiae.canvas.code". Subscribe to the topic; event-type
		// matching happens in EventConsumer.Handle().
		topics := cfg.Kafka.InboundTopics
		if len(topics) == 0 {
			// sentiae.canvas.code covers canvas.code.execute_requested;
			// sentiae.canvas.node covers canvas.node.executed (A1 — Tier
			// 1A Canvas → Runtime event-driven path).
			topics = []string{"sentiae.canvas.code", "sentiae.canvas.node"}
		}
		// §8.6 — add git topics so the affected-test trigger and
		// session lifecycle handler see commit.created, push, and
		// session.merged events. Publishers across the platform use
		// several conventions for git topics; subscribing to the
		// union keeps us tolerant to whichever ends up landing.
		gitTopics := []string{
			"sentiae.git.events",
			"sentiae.git.commit",
			"sentiae.git.push",
			"sentiae.git.session",
		}
		for _, gt := range gitTopics {
			already := false
			for _, t := range topics {
				if t == gt {
					already = true
					break
				}
			}
			if !already {
				topics = append(topics, gt)
			}
		}
		// A1 — ensure canvas.node.executed flows into runtime even when
		// operators override InboundTopics. Platform-kit maps
		// "sentiae.canvas.node.executed" → topic "sentiae.canvas.node".
		canvasNodeTopic := "sentiae.canvas.node"
		hasCanvasNode := false
		for _, t := range topics {
			if t == canvasNodeTopic {
				hasCanvasNode = true
				break
			}
		}
		if !hasCanvasNode {
			topics = append(topics, canvasNodeTopic)
		}
		groupID := cfg.Kafka.GroupID
		if groupID == "" {
			groupID = "runtime-service"
		}
		consumer, err := messaging.NewEventConsumer(cfg.Kafka.Brokers, groupID, topics)
		if err != nil {
			log.Printf("Warning: failed to initialize Kafka consumer: %v (inbound events disabled)", err)
		} else {
			c.EventConsumer = consumer
			log.Printf("Kafka event consumer initialized (group=%s, topics=%v)", groupID, topics)
		}
	} else {
		c.EventPublisher = &messaging.NoopPublisher{}
		log.Println("Event publisher initialized (noop - Kafka disabled)")
	}
}

// initRepositories initializes all repositories
func (c *Container) initRepositories() {
	// CS11 — shared time-travel recorder wired into write-path repos
	// so TestRun / HermeticBuild / HermeticBuildStep / VMSnapshot
	// writes land in the platform entity_snapshots table.
	recorder := timetravel.NewGORMRecorder(c.DB, "runtime-service", nil)
	// Ensure the schema exists. Failures are logged but never fatal;
	// the platform-wide cross-service recorder is a best-effort path.
	if err := timetravel.AutoMigrate(c.DB); err != nil {
		log.Printf("[timetravel] AutoMigrate failed: %v", err)
	}

	c.ExecutionRepo = postgres.NewExecutionRepository(c.DB)
	c.VMRepo = postgres.NewMicroVMRepository(c.DB)
	c.SnapshotRepo = postgres.NewSnapshotRepository(c.DB).WithRecorder(recorder)
	c.MetricsRepo = postgres.NewExecutionMetricsRepository(c.DB)
	c.VMInstanceRepo = postgres.NewVMInstanceRepository(c.DB)
	c.TerminalSessionRepo = postgres.NewTerminalSessionRepository(c.DB)

	// Graph repositories
	c.GraphDefRepo = postgres.NewGraphDefinitionRepository(c.DB)
	c.GraphNodeRepo = postgres.NewGraphNodeRepository(c.DB)
	c.GraphEdgeRepo = postgres.NewGraphEdgeRepository(c.DB)
	c.GraphExecRepo = postgres.NewGraphExecutionRepository(c.DB)
	c.NodeExecRepo = postgres.NewNodeExecutionRepository(c.DB)
	c.DebugSessionRepo = postgres.NewGraphDebugSessionRepository(c.DB)
	c.GraphTraceRepo = postgres.NewGraphTraceRepository(c.DB)
	c.TraceSnapshotRepo = postgres.NewGraphTraceSnapshotRepository(c.DB)

	// Test tracking
	testRunRepo := postgres.NewTestRunRepository(c.DB).WithRecorder(recorder)
	c.TestRunRepo = testRunRepo

	// §9.2 — HermeticBuild repo + usecase + per-step hash chain.
	c.HermeticBuildRepo = postgres.NewHermeticBuildRepository(c.DB).WithRecorder(recorder)
	c.StepArtifactHashRepo = postgres.NewStepArtifactHashRepository(c.DB).WithRecorder(recorder)
	c.HermeticBuildUC = usecase.NewHermeticBuildUseCase(c.HermeticBuildRepo).
		WithHashRepo(c.StepArtifactHashRepo).
		WithEnforceBaseImageDigest(c.Config.Hermetic.EnforceBaseImageDigest).
		WithEnforceReproducibility(c.Config.Hermetic.EnforceReproducibility)

	// 9.4 — remote agent registry
	c.RuntimeAgentRepo = postgres.NewRuntimeAgentRepository(c.DB)

	// runtime-fleet CP3 — image-boot workloads
	c.ImageWorkloadRepo = postgres.NewImageWorkloadRepository(c.DB)

	// runtime-fleet CP4 — durable fleet control-plane store
	c.HostRepo = postgres.NewHostRepository(c.DB)
	c.FleetAppRepo = postgres.NewFleetAppRepository(c.DB)
	c.ReplicaRepo = postgres.NewReplicaRepository(c.DB)
	c.PlacementRepo = postgres.NewPlacementRepository(c.DB)
	c.RouteRepo = postgres.NewRouteRepository(c.DB)
	c.VolumeRepo = postgres.NewVolumeRepository(c.DB)

	// SentiaeDB Phase 0 — the durable microVM addressing plane (migrations/0020).
	// Wired on EVERY executor: the host registry assigns net ordinals through it,
	// and teardown releases leases through it, neither of which is
	// firecracker-only.
	c.NetLeaseRepo = postgres.NewNetLeaseRepository(c.DB)

	// CP4.5 §9#3 (D-183) — P19 resource control-plane store.
	c.FleetResourceRepo = postgres.NewFleetResourceRepository(c.DB)

	// CP4.5 §9#5 — P21 fleet network fabric store.
	c.FleetNetworkRepo = postgres.NewFleetNetworkRepository(c.DB)
	c.FleetNetworkPolicyRepo = postgres.NewFleetNetworkPolicyRepository(c.DB)

	log.Println("Repositories initialized (PostgreSQL)")
}

// initUseCases initializes all use cases
func (c *Container) initUseCases(cfg *config.Config) {
	c.VMUC = usecase.NewVMService(c.VMRepo, c.VMProvider)

	// Durable snapshot object-store backing. When enabled, build a
	// CachingStore (S3/MinIO source of truth + local FilesystemStore warm
	// cache) and inject it so snapshots survive host loss and restore on
	// any host. When disabled (or on construction failure) the snapshot
	// service stays on the local-only flow — nil store is the safe default.
	snapSvc := usecase.NewSnapshotService(c.SnapshotRepo, c.VMRepo, c.VMProvider)
	// Build the durable snapshot store once and reuse it for both the snapshot
	// service and the warm-pool template persistence (don't double-build).
	snapshotStore := c.buildSnapshotStore(cfg)
	// Stash for initFleet's P19 volume snapshotter (D-183), which streams
	// recovery points to the same durable artifact store.
	c.snapshotArtifactStore = snapshotStore
	if snapshotStore != nil {
		if injectable, ok := snapSvc.(usecase.SnapshotStoreInjectable); ok {
			injectable.SetArtifactStore(snapshotStore)
			log.Println("Snapshot durable object store wired (S3/MinIO + local cache)")
		}
	}
	c.SnapshotUC = snapSvc
	c.ExecutionUC = usecase.NewExecutionService(c.ExecutionRepo, c.MetricsRepo, c.VMUC, c.ExecutionRunner, c.VMProvider)

	// Multi-file project compile. Backed by an ephemeral `docker run --rm`
	// build container — independent of the Firecracker execution pool.
	// When docker is unavailable the compiler self-reports
	// ErrCompileToolchainUnavailable at call time (Unavailable to callers).
	c.ProjectCompiler = compiler.NewDockerCompiler()
	c.CompileUC = usecase.NewCompileProject(c.ProjectCompiler)

	// Wire event publisher for execution events (cross-service integration)
	type eventPublishable interface {
		SetEventPublisher(pub usecase.EventPublisher)
	}
	if svc, ok := c.ExecutionUC.(eventPublishable); ok {
		svc.SetEventPublisher(c.EventPublisher)
	}

	// Initialize scheduler with localhost resources
	var localVCPU, localMemMB int
	if cfg.App.ExecutorType == "firecracker" {
		localVCPU = cfg.Firecracker.MaxVCPU
		localMemMB = cfg.Firecracker.MaxMemMB
	} else {
		localVCPU = cfg.Container.MaxVCPU
		localMemMB = cfg.Container.MaxMemMB
	}
	if localVCPU <= 0 {
		localVCPU = 8
	}
	if localMemMB <= 0 {
		localMemMB = 8192
	}
	c.SchedulerUC = usecase.NewScheduler(localVCPU, localMemMB)

	// Initialize VM instance service with reconciliation support
	c.VMInstanceUC = usecase.NewVMInstanceService(c.VMInstanceRepo, c.VMProvider, c.SchedulerUC)

	// Initialize reconciliation controller (5-second interval)
	c.ReconciliationController = usecase.NewReconciliationController(c.VMInstanceUC, 5*time.Second)

	// Initialize graph use cases
	c.GraphUC = usecase.NewGraphService(c.GraphDefRepo, c.GraphNodeRepo, c.GraphEdgeRepo, c.EventPublisher)

	// Initialize trace recorder
	c.TraceRecorder = usecase.NewGraphTraceRecorder(c.GraphTraceRepo, c.TraceSnapshotRepo)

	// Initialize graph execution engine
	c.GraphEngine = usecase.NewGraphExecutionEngine(
		c.GraphDefRepo, c.GraphNodeRepo, c.GraphEdgeRepo,
		c.GraphExecRepo, c.NodeExecRepo,
		c.ExecutionUC, c.EventPublisher,
	)
	c.GraphEngine.SetTraceRecorder(c.TraceRecorder)

	// Wire the warm-clone code runner when enabled. Gated on executor
	// firecracker + a live FCProvider so the WarmManager has the FC config
	// (binary/kernel/rootfs/socket/snapshot paths) it needs. Disabled →
	// engine.warm stays nil → code nodes run the cold single-shot path.
	if cfg.App.ExecutorType == "firecracker" && cfg.Firecracker.WarmPoolEnabled && c.FCProvider != nil {
		warmMgr := firecracker.NewWarmManager(c.FCProvider)
		warmAgent := firecracker.NewAgentClient(10 * time.Second)
		// Reuse the same durable store the snapshot service uses (nil ⇒ the
		// pool stays local-only). Pulled/persisted template files land under a
		// stable per-language path inside the FC snapshot dir.
		templateDir := filepath.Join(cfg.Firecracker.SnapshotPath, "templates")
		// readyN > 0 keeps a pre-warmed buffer of restored clones per language so
		// an execution grabs one instantly; 0 ⇒ on-demand only (no goroutines).
		readyN := cfg.Firecracker.WarmPoolReady
		c.WarmPool = firecracker.NewWarmPool(warmMgr, warmAgent, snapshotStore, templateDir, readyN)
		c.GraphEngine.SetWarmRunner(c.WarmPool)
		if snapshotStore != nil {
			log.Printf("[WARM-POOL] enabled — code nodes run on fast warm clones (durable template persistence on, prewarm_ready=%d)", readyN)
		} else {
			log.Printf("[WARM-POOL] enabled — code nodes run on fast warm clones (prewarm_ready=%d)", readyN)
		}
	}

	// Initialize graph debug service
	c.GraphDebugUC = usecase.NewGraphDebugService(
		c.DebugSessionRepo, c.GraphDefRepo, c.GraphNodeRepo, c.GraphEdgeRepo,
		c.GraphExecRepo, c.NodeExecRepo, c.GraphEngine,
		c.EventPublisher, c.TraceRecorder,
	)

	// Initialize graph replay service
	c.GraphReplayUC = usecase.NewGraphReplayService(c.GraphTraceRepo, c.TraceSnapshotRepo)

	// Initialize terminal service
	c.TerminalUC = usecase.NewTerminalService(c.TerminalSessionRepo, c.VMUC, c.VMProvider)

	// 9.4 — agent registry + remote dispatcher. Remote executor is
	// wired with an empty token provider by default; production
	// injects a real secret-store-backed provider in main.go so the
	// bootstrap order (secrets → registry → dispatcher) stays clear.
	c.AgentRegistry = usecase.NewAgentRegistry(c.RuntimeAgentRepo)
	c.RemoteExecutor = agent.NewRemoteExecutor(agent.NewMapTokenProvider(map[string]string{}))
	c.AgentDispatcher = usecase.NewDispatcher(c.AgentRegistry, c.RemoteExecutor, nil)

	// Initialize pending processor (2-second interval, batch size 10)
	c.PendingProcessor = usecase.NewPendingProcessor(c.ExecutionUC, c.GraphEngine, 2*time.Second, 10)

	// Register inbound Kafka handlers if the consumer is available.
	c.InboundEventHandlerSvc = usecase.NewInboundEventHandlerService(c.ExecutionUC)
	if c.EventConsumer != nil {
		c.InboundEventHandlerSvc.RegisterHandlers(c.EventConsumer)
	}

	// A1 (GAP_MAP_2026_04_17) — subscribe to sentiae.canvas.node.executed
	// so "Run on canvas" is event-driven end-to-end. The executionService
	// already publishes sentiae.runtime.execution.completed on finish via
	// the injected EventPublisher, which canvas consumes for the result
	// badge — no republish needed here.
	c.CanvasExecutedConsumer = messaging.NewCanvasExecutedConsumer(c.ExecutionUC)
	if c.EventConsumer != nil {
		c.CanvasExecutedConsumer.Register(c.EventConsumer)
	}

	// §8.6 — git → runtime handlers. AffectedTestTrigger requires the
	// resolver and test-run repo; SessionLifecycleHandler needs only
	// the test-run repo. Both subscribe to multiple event types on the
	// same consumer; the platform-kit consumer keys handlers by event
	// type so each registration is independent.

	// Test generation + affected test resolution (Items 51 / 52).
	// Foundry client is optional; noop fallback yields deterministic scaffolds.
	c.TestGenUC = usecase.NewTestGenerationService(foundry.NewNoopClient())
	// Symbol graph from git-service. An empty services.git.url leaves
	// the resolver in heuristic mode (filename-stem matching); a
	// configured URL promotes it to symbol-graph-driven resolution via
	// git-service's /impact endpoint.
	var symbolGraph usecase.SymbolGraphClient
	if gitURL := cfg.Services.Git.URL; gitURL != "" {
		timeout := cfg.Services.Git.Timeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		httpClient := &http.Client{Timeout: timeout}
		if sgc := gitservice.NewSymbolGraphClient(gitURL, httpClient); sgc != nil {
			symbolGraph = sgc
		}
	}
	c.AffectedUC = usecase.NewAffectedTestResolver(symbolGraph)

	// §8.6 — affected-test trigger + session lifecycle handler. These
	// translate git-service events into TestRun rows + test.queued
	// CloudEvents so Pulse reflects the cascade in real time.
	c.AffectedTestTrigger = usecase.NewAffectedTestTrigger(c.AffectedUC, c.TestRunRepo, c.EventPublisher)
	c.SessionLifecycleHandler = usecase.NewSessionLifecycleHandler(c.TestRunRepo, c.EventPublisher)

	// §8.3 pool scheduler wrapping the Firecracker dispatcher. The
	// fallback dispatcher defined below in initHandlers is the same
	// instance kind; constructing a second one here is safe because
	// dispatch is stateless and each run still acquires its own VM.
	poolDispatcher := usecase.NewFirecrackerTestRunDispatcher(c.ExecutionUC, c.TestRunRepo)
	c.TestPool = usecase.NewPoolScheduler(poolDispatcher, usecase.DefaultPoolWorkers)
	c.SessionCommitAddedConsumer = messaging.NewSessionCommitAddedConsumer(
		c.TestRunRepo, c.TestPool, c.EventPublisher,
	)

	// CS-2 G2.7 — continuous-testing trigger. Subscribes to
	// git.push.received (affected dispatch) and git.session.created
	// (smoke run on critical tests). Idempotency by (sha, test_id) +
	// (session_id, test_id) lives inside the trigger.
	c.ContinuousTestTrigger = eventhandler.NewContinuousTestTrigger(
		c.AffectedUC,
		c.TestPool,
		&smokeTestListerAdapter{repo: c.TestRunRepo},
		c.TestRunRepo,
		&continuousEventPublisherAdapter{publisher: c.EventPublisher},
	)

	if c.EventConsumer != nil {
		// AffectedTestTrigger self-filters on event.Type, so registering
		// the same handler on every git lifecycle type we care about is
		// the safest way to stay tolerant of publisher convention drift.
		for _, t := range []string{
			"sentiae.git.commit.created",
			"git.commit.created",
			"sentiae.git.push",
			"git.push",
			"sentiae.git.change.created",
			"git.change.created",
		} {
			c.EventConsumer.Handle(t, c.AffectedTestTrigger.HandleGitEvent)
		}
		for _, t := range []string{
			"sentiae.git.session.merged",
			"git.session.merged",
		} {
			c.EventConsumer.Handle(t, c.SessionLifecycleHandler.HandleGitEvent)
		}
		// §8.6 session.commit_added → PoolScheduler dispatch.
		c.SessionCommitAddedConsumer.Register(c.EventConsumer)
		// CS-2 G2.7 — continuous-testing trigger.
		c.ContinuousTestTrigger.Register(c.EventConsumer)
		if c.PostMergeFullRunTrigger != nil {
			// §B36 subscribe both merge.completed (internal sessions) and
			// pr.merged (external PR merges) so the full-suite sweep
			// covers every merge path.
			for _, t := range usecase.FullRunEventTypes {
				c.EventConsumer.Handle(t, c.PostMergeFullRunTrigger.HandleGitEvent)
			}
		}
	}

	// B6 gap #1 — automatic VM snapshot scheduler.
	c.CheckpointScheduler = usecase.NewCheckpointScheduler(
		c.VMInstanceRepo,
		c.SnapshotRepo,
		c.VMProvider,
		c.EventPublisher,
		0, // use DefaultCheckpointInterval
	)

	// §8.3 — auto-quarantine scheduler. Flips TestRun.Quarantined when a
	// test's FlakinessScore stays above the threshold over a sustained
	// window. Zero args fall back to the package defaults.
	c.QuarantineUC = usecase.NewQuarantineUseCase(
		c.TestRunRepo,
		c.EventPublisher,
		0, 0, 0,
	)

	// §8.6 — post-merge full-suite trigger. Listens for
	// sentiae.git.merge.completed and enqueues a full canvas test run.
	c.PostMergeFullRunTrigger = usecase.NewPostMergeFullRunTrigger(c.TestRunRepo, c.EventPublisher)

	// B6 gap #5 — regression test generator. TraceFetcher uses ops-service
	// when configured; nil fetcher means the endpoint fast-fails with a
	// descriptive error which is the desired behaviour for unconfigured
	// deployments. foundry.NewNoopClient yields deterministic scaffolds.
	c.RegressionGenerator = usecase.NewRegressionTestGenerator(
		nil,
		foundry.NewNoopClient(),
		c.RegressionTestRepo,
		c.EventPublisher,
	)

	log.Println("Use cases initialized")
}

// initHandlers initializes HTTP and gRPC handlers
func (c *Container) initHandlers() {
	c.HTTPServer = httphandler.NewServer(
		c.ExecutionUC,
		c.VMUC,
		c.SnapshotUC,
		c.VMInstanceUC,
		c.SchedulerUC,
		c.GraphUC,
		c.GraphEngine,
		c.GraphDebugUC,
		c.GraphReplayUC,
		c.TerminalUC,
		c.TestRunRepo,
		c.TestGenUC,
		c.AffectedUC,
	)
	// Emit sentiae.runtime.test.completed on test-run terminal
	// transitions so foundry-service's spec-driven saga can advance.
	c.HTTPServer.SetTestRunEventPublisher(c.EventPublisher)

	// §19.1 flow 1E — push test results directly to canvas-service
	// alongside the Kafka emit so badges update at the latency floor.
	// CANVAS_SERVICE_URL empty disables the push (Kafka still delivers).
	if canvasURL := os.Getenv("CANVAS_SERVICE_URL"); canvasURL != "" {
		// Phase 2 mTLS: the shared client-side SPIFFE source is built in
		// initMTLSSource via grpcclient.NewMeshSource (fail-closed under strict —
		// the #pulse-outbound-mtls-fail-open fix). c.canvasClientSource is nil when
		// mode is off, or under permissive with the workload API down — the dial
		// then degrades to insecure via grpcclient.Dial.
		canvasClient := canvasservice.NewClient(context.Background(), canvasURL, pkconfig.MTLSMode(), c.canvasClientSource, 10*time.Second)
		// x-api-key value: prefer the shared service API key; fall back to the
		// legacy CANVAS_SERVICE_TOKEN only when the API key is unset.
		serviceToken := c.Config.Server.GRPC.ServiceAPIKey
		if serviceToken == "" {
			serviceToken = os.Getenv("CANVAS_SERVICE_TOKEN")
		}
		canvasClient.ServiceToken = serviceToken
		canvasClient.ServiceUserID = os.Getenv("SERVICE_USER_ID")
		c.HTTPServer.SetTestRunCanvasClient(canvasClient)
		log.Printf("canvas-service HTTP push enabled (url: %s)", canvasURL)
	}

	// 9.4 — agent registry + dispatcher HTTP surface.
	c.HTTPServer.SetRuntimeAgentHandler(httphandler.NewRuntimeAgentHandler(c.AgentRegistry, c.RuntimeAgentRepo))

	// Warm-VM fleet visibility + control. c.WarmPool may be nil (warm pool
	// disabled); NewFleetHandler accepts nil and reports the fleet as disabled,
	// so we register the routes unconditionally.
	//
	// Fail-closed at BOOT, like the customer-agent enrolment endpoint above and
	// the §B1 permission checker below: /fleet is mounted outside the JWT group
	// on an HTTP port that binds all interfaces on the fleet host, and it
	// includes DELETE /fleet/clones/{id}. Without a service token the surface
	// would either be open (the old behaviour) or silently 401 everything while
	// looking healthy — both are worse than refusing to start.
	fleetHandler, fleetErr := httphandler.NewFleetHandler(c.WarmPool, c.Config.Server.GRPC.ServiceAPIKey)
	if fleetErr != nil {
		log.Fatalf("runtime-service: %v", fleetErr)
	}
	c.HTTPServer.SetFleetHandler(fleetHandler)

	// rt#11 — scale-to-zero wake endpoint (/_activate). Mounted only when the
	// activator is wired (firecracker host); the setter no-ops on a nil handler.
	if c.FleetActivatorUC != nil {
		c.HTTPServer.SetActivatorHandler(httphandler.NewActivatorHandler(c.FleetActivatorUC))
	}

	// §9.4 — customer-agent enrolment endpoint. Signs CSRs submitted
	// by freshly-installed customer-hosted agents. Unwired when the CA
	// paths aren't set (dev deployments / tenants not using the agent).
	caCert := os.Getenv("AGENT_CA_CERT_PATH")
	caKey := os.Getenv("AGENT_CA_KEY_PATH")
	if caCert != "" && caKey != "" {
		tokenSource := func() string { return os.Getenv("AGENT_ENROLMENT_TOKEN") }
		// Fail-closed at BOOT, like the §B1 permission checker below: the
		// route is mounted outside the JWT group and signs CSRs with the
		// agent CA, so enabling it without AGENT_ENROLMENT_TOKEN would
		// publish an anonymous identity-issuing endpoint. Refusing at
		// request time only is not enough — the endpoint would look
		// healthy while being a hole (or, once fixed, silently unusable).
		agentCertHandler, agentCertErr := httphandler.NewCustomerAgentCertHandler(caCert, caKey, tokenSource)
		if agentCertErr != nil {
			log.Fatalf("runtime-service: %v", agentCertErr)
		}
		c.HTTPServer.SetCustomerAgentCertHandler(agentCertHandler)
		log.Printf("[agent-enrolment] endpoint enabled (ca_cert=%s)", caCert)
	}

	// B6 gap #5 — regression tests from production traces.
	c.HTTPServer.SetRegressionTestHandler(
		httphandler.NewRegressionTestHandler(c.RegressionGenerator, c.RegressionTestRepo),
	)

	// §19 follow-up (I1 + I3): DSL /dsl/execute handler with run_tests
	// wired to the real TestRun repo.
	c.HTTPServer.SetDSLHandler(httphandler.NewDSLHandler(c.TestRunRepo, c.HermeticBuildUC))

	// §9.2 full hermetic build: content-addressed artifact store +
	// resolve/verify/upload endpoints. The artifact store root comes
	// from ARTIFACT_STORE_ROOT; missing or unwritable disables upload
	// but leaves resolve/verify working.
	if root := os.Getenv("ARTIFACT_STORE_ROOT"); root != "" {
		if store, err := usecase.NewFilesystemStore(root); err != nil {
			log.Printf("[hermetic-build] artifact store disabled: %v", err)
		} else {
			c.HermeticBuildUC.WithStore(store)
			log.Printf("[hermetic-build] artifact store enabled at %s", root)
		}
	}
	c.HTTPServer.SetHermeticBuildHandler(httphandler.NewHermeticBuildHandler(c.HermeticBuildUC))

	// §8.3 manual quarantine override routes.
	if c.QuarantineUC != nil {
		c.HTTPServer.SetTestQuarantineHandler(httphandler.NewTestQuarantineHandler(c.QuarantineUC))
	}

	// §B1 fail-closed: never default to AllowAllPermissionChecker. When
	// no real checker is wired, MustPermissionChecker returns deny-all
	// (dev/staging) or the caller is expected to have already errored
	// on startup (production). Operators can opt-in to dev fail-open via
	// APP_PERMISSION_ALLOW_ALL=true; every allowed check logs a warning.
	checker, real, permErr := httphandler.MustPermissionChecker(nil)
	if permErr != nil {
		log.Fatalf("runtime-service: %v", permErr)
	}
	if !real {
		log.Printf("runtime-service: PermissionChecker running in fallback mode (deny-all or APP_PERMISSION_ALLOW_ALL=true)")
	}
	c.HTTPServer.SetPermissionChecker(checker)

	// §8.4 multi-type dispatcher: route perf / security / contract test
	// runs to their dedicated executors; everything else falls through
	// to the generic Firecracker dispatcher. DB-provisioning middleware
	// wraps the router so ephemeral_pg runs transparently acquire a
	// DATABASE_URL — no-op when no provisioner is wired.
	fallbackDispatcher := usecase.NewFirecrackerTestRunDispatcher(c.ExecutionUC, c.TestRunRepo)
	perfExec := usecase.NewPerfTestExecutor(c.ExecutionUC, c.TestRunRepo)
	secExec := usecase.NewSecurityTestExecutor(c.ExecutionUC, c.TestRunRepo, c.EventPublisher)
	contractExec := usecase.NewContractTestExecutor(c.ExecutionUC, c.TestRunRepo)

	// §8.4 — visual + accessibility executors. They consume the same
	// VMExecRunner seam (shared with perf/sec/contract) so we reuse the
	// execExecutorRunnerAdapter below to wrap ExecutionUseCase. The
	// updater is the test-run repo (same as above).
	vmRunner := &execExecutorRunnerAdapter{execUC: c.ExecutionUC}
	visualExec := visual.NewPlaywrightExecutor(vmRunner, c.TestRunRepo)
	a11yExec := a11y.NewAxeExecutor(vmRunner, c.TestRunRepo)

	multi := usecase.NewMultiTypeDispatcher(perfExec, secExec, contractExec, fallbackDispatcher).
		WithVisualExecutor(&profileDroppingDispatcher{inner: visualExec}).
		WithA11yExecutor(&profileDroppingDispatcher{inner: a11yExec})
	// Provisioner left nil; operators inject via a platform-kit wrapper
	// when ephemeral-pg is available.
	wrapped := usecase.NewDBProvisioningMiddleware(multi, nil)
	c.HTTPServer.SetTestRunDispatcher(wrapped)

	if c.Config.Server.GRPC.Enabled {
		c.GRPCServer = grpchandler.NewServer(
			grpchandler.ServerConfig{
				EnableLogging:  c.Config.App.Environment == "development",
				EnableRecovery: true,
				ServiceAPIKey:  c.Config.Server.GRPC.ServiceAPIKey,
				JWKSURL:        c.Config.Server.GRPC.JWKSURL,
				JWTIssuer:      c.Config.Server.GRPC.JWTIssuer,
			},
			c.ExecutionUC,
			c.GraphUC,
			c.GraphEngine,
		)
		// Wire the extended test-run / VM-usage surface. `wrapped` already
		// threads DB-provisioning → multi-type executor → Firecracker
		// dispatcher, matching the HTTP /test-runs/{id}/dispatch path.
		c.GRPCServer.ExecutionServer().WithTestRunDeps(grpchandler.TestRunServerDeps{
			TestRunRepo:      c.TestRunRepo,
			TestRunDispatch:  wrapped,
			VMUC:             c.VMUC,
			ExecutionsLister: c.ExecutionUC,
		})
		// Wire the multi-file project compile RPC (ephemeral build container).
		c.GRPCServer.ExecutionServer().WithCompiler(c.CompileUC)
	}

	// runtime-fleet CP3 — FleetOrchestration gRPC registration. Relocated here
	// from initFleet (which now runs first) because it is the fleet's only
	// dependency on the GRPCServer that this method builds. FleetProvisionUC +
	// FleetHostRegistry are already wired by initFleet.
	if c.GRPCServer != nil {
		c.GRPCServer.RegisterFleet(grpchandler.NewFleetServer(c.FleetProvisionUC, c.FleetHostRegistry))
		log.Println("FleetOrchestration gRPC service registered")

		// CP4.5 §9#5 — P21 FleetNetworkFabric. Registered on every host: off the
		// firecracker host the fail-loud enforcer makes each RPC refuse, which is a
		// truthful answer rather than an unimplemented one.
		c.GRPCServer.RegisterNetworkFabric(grpchandler.NewNetworkFabricServer(c.FleetNetworkFabricUC))
		log.Println("FleetNetworkFabric gRPC service registered")

		// CP4.5 §9#3 (D-183) — P19 ResourceProvisioning. Registered on every host:
		// off the firecracker host the dedicated path fails loud through the composed
		// FleetProvision, a truthful answer rather than an unimplemented one.
		if c.ResourceServer != nil {
			c.GRPCServer.RegisterResourceProvisioning(c.ResourceServer)
			log.Println("ResourceProvisioning gRPC service registered")
		}
	}

	// Wave-8 uniform ops surface (D-179): /posture reports the declared boot
	// posture; /healthz/consumers surfaces runtime's inbound Kafka consumer lag
	// + DLQ. Wired before SetupRoutes so the mounts see the live handles.
	c.HTTPServer.SetOpsSurface(c.Posture, c.kafkaConsumers()...)

	// Register routes now that every Set*Handler has fired. NewServer
	// deliberately skips this step so setupRoutes can see the full set
	// of handlers. Mirrors foundry-service's SetupRoutes pattern.
	c.HTTPServer.SetupRoutes()

	log.Println("Handlers initialized")
}

// StartBackgroundControllers starts background controllers (reconciliation loop, etc.)
func (c *Container) StartBackgroundControllers(ctx context.Context) {
	if c.ReconciliationController != nil {
		c.ReconciliationController.Start(ctx)
		log.Println("Reconciliation controller started")
	}
	if c.PendingProcessor != nil {
		c.PendingProcessor.Start(ctx)
		log.Println("Pending processor started")
	}
	if c.CheckpointScheduler != nil {
		// Attempt to restore any VMs that were running before the last
		// restart. Failures are logged; we still start the periodic
		// snapshotter so newly-launched VMs are protected.
		if n, err := c.CheckpointScheduler.RestoreLatest(ctx); err != nil {
			log.Printf("[CHECKPOINT] RestoreLatest on startup failed: %v", err)
		} else if n > 0 {
			log.Printf("[CHECKPOINT] restored %d VM(s) from prior checkpoints", n)
		}
		c.CheckpointScheduler.Start(ctx)
		log.Println("Checkpoint scheduler started")
	}
	if c.QuarantineUC != nil {
		c.QuarantineUC.Start(ctx)
		log.Println("Quarantine scheduler started")
	}
	if c.TestPool != nil {
		c.TestPool.Start(ctx)
		log.Println("Test pool scheduler started (§8.3, default workers)")
	}
	// §9.1 — start warm-up refill loop for every Firecracker VM pool.
	for _, pool := range c.FCPools {
		if pool != nil {
			pool.Start(ctx)
		}
	}
	if len(c.FCPools) > 0 {
		log.Printf("[FC-POOL] %d warm pool(s) started", len(c.FCPools))
	}
	c.startFleetHeartbeat(ctx)
	c.startFleetActivityFeed(ctx)
	// D-184 — a restore in flight when this process died left its resource in
	// phase `restoring` (and its volume too, so every boot is refused). Release
	// THIS host's stuck rows to `failed` before the reconciler starts ticking;
	// recovery is a re-issued RestoreResource, never an auto-resume.
	c.sweepInterruptedRestores(ctx)
	c.startStagingSweeper(ctx)
	c.startLedgerReconciler(ctx)
	// The durability gauges' timer. Started BEFORE the fleet reconciler so the first
	// values land within a second of boot: an instance that serves traffic while its
	// durability gauges are still unset is an instance whose alerts have nothing to
	// evaluate.
	if c.FleetDurabilityMetrics != nil {
		c.FleetDurabilityMetrics.Start(ctx)
		log.Printf("Fleet durability metric collector started (interval=%s)", usecase.DurabilityCollectEvery)
	}
	// D-200 — drain the primary_only backlog into the second failure domain. Nil on
	// every fleet host; the single control-plane instance is the only writer.
	if c.RecoveryPointMirror != nil {
		c.RecoveryPointMirror.Start(ctx)
		log.Printf("Recovery-point second-domain mirror started (interval=%s, batch=%d, domain=%s)",
			usecase.RecoveryPointMirrorEvery, usecase.RecoveryPointMirrorBatch, c.RecoveryPointMirror.Domain())
	}
	c.startFleetReconciler(ctx)
	// CP4.5 §9#3 (D-183) — shared-tier TTL reaper. Nil until the shared engine is
	// wired (NEEDS-DECISION on shared-engine admin credentials); the loop is
	// ctx-aware + panic-recovering and exits on ctx cancel or Stop().
	if c.FleetResourceSharedUC != nil {
		c.FleetResourceSharedUC.Start(ctx)
		log.Println("Fleet shared-resource TTL reaper started")
	}
	c.StartConsumers(ctx)
}

// sweepInterruptedRestores releases the resources of THIS host left mid-restore
// by a restart (D-184): phase restoring → failed + last_error. Synchronous and
// log-and-continue — it is a bookkeeping pass over a handful of rows, and a
// failure must never block startup.
func (c *Container) sweepInterruptedRestores(ctx context.Context) {
	if c.ResourceRestorer == nil {
		return
	}
	n, err := c.ResourceRestorer.SweepInterruptedRestores(ctx)
	if err != nil {
		log.Printf("[FLEET-RESTORE] interrupted-restore sweep failed: %v", err)
		return
	}
	if n > 0 {
		log.Printf("[FLEET-RESTORE] released %d resource(s) left mid-restore by a restart to failed — boots stay refused until RestoreResource is re-issued", n)
	}
}

// startStagingSweeper runs the orphaned-staging-directory GC
// (#fleet-image-staging-dirs-no-gc): once at startup, then every
// stagingSweepEvery. A fleet host that fills up takes down EVERY customer VM on
// it, so this is an availability control, not housekeeping.
//
// It is periodic and not startup-only because a process killed between
// materialize and boot leaves a directory no boot-path cleanup can reach. In its
// own goroutine (ctx-aware + panic-recovering) because a full work root can hold
// hundreds of directories and startup must not block on walking them.
func (c *Container) startStagingSweeper(ctx context.Context) {
	if c.FleetStagingSweeper == nil {
		return
	}
	sweeper := c.FleetStagingSweeper
	sweep := func() {
		out, err := sweeper.Sweep(ctx)
		if err != nil {
			log.Printf("[FLEET-STAGING-GC] sweep failed: %v", err)
			return
		}
		if out.Reclaimed > 0 {
			log.Printf("[FLEET-STAGING-GC] reclaimed %d orphaned staging dir(s), %d byte(s)", out.Reclaimed, out.Bytes)
		}
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[FLEET-STAGING-GC] sweep loop panicked: %v", r)
			}
		}()
		sweep()
		ticker := time.NewTicker(stagingSweepEvery)
		defer ticker.Stop()
		log.Printf("[FLEET-STAGING-GC] staging sweep started (interval=%s)", stagingSweepEvery)
		for {
			select {
			case <-ctx.Done():
				log.Println("[FLEET-STAGING-GC] staging sweep stopped (context cancelled)")
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()
}

// startLedgerReconciler runs the report-only ledger↔reality audit: once at
// startup, then every ledgerReconcileEvery. It is the only thing that would ever
// notice that a fleet_volumes row advertises customer data with no bytes behind
// it, or that a counted recovery point has no object in the store.
//
// REPORT-ONLY — it deletes, moves and repairs nothing (see the use case doc), so
// running it on a ticker cannot itself destroy anything. Divergences land as
// Error-level log lines carrying the identity needed to act; acting is a human
// decision.
//
// In its own goroutine (ctx-aware + panic-recovering) because the pass stats
// every backing file and does an object-store HEAD per recovery point, and
// startup must not block on either.
func (c *Container) startLedgerReconciler(ctx context.Context) {
	if c.FleetLedgerReconciler == nil {
		return
	}
	rec := c.FleetLedgerReconciler
	pass := func() {
		rep, err := rec.Reconcile(ctx)
		if err != nil {
			log.Printf("[FLEET-LEDGER] reconcile pass failed (nothing reported): %v", err)
			return
		}
		// Only a COMPLETED pass publishes. A failed pass proved nothing, and
		// republishing zeros for it would turn an unreadable oracle into a clean bill
		// of health — the ledger gauges then keep their previous values, and the
		// last-success timestamp stops advancing, which is the honest signal.
		usecase.PublishLedgerReport(rep, time.Now().UTC())
		if rep.Divergences() > 0 {
			log.Printf("[FLEET-LEDGER] %d LEDGER DIVERGENCE(S): %d row-without-file, %d file-without-row, %d recovery-point-without-object (checked %d volume(s), %d file(s), %d recovery point(s); %d undetermined) — report-only, nothing was changed",
				rep.Divergences(), rep.RowsWithoutFile, rep.FilesWithoutRow, rep.RecoveryPointsWithoutObject,
				rep.VolumesChecked, rep.FilesChecked, rep.RecoveryPointsChecked, rep.Undetermined)
			return
		}
		if rep.Undetermined > 0 {
			log.Printf("[FLEET-LEDGER] no divergence found, but %d entr(ies) were undetermined (checked %d volume(s), %d file(s), %d recovery point(s))",
				rep.Undetermined, rep.VolumesChecked, rep.FilesChecked, rep.RecoveryPointsChecked)
		}
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[FLEET-LEDGER] reconcile loop panicked: %v", r)
			}
		}()
		pass()
		ticker := time.NewTicker(ledgerReconcileEvery)
		defer ticker.Stop()
		log.Printf("[FLEET-LEDGER] report-only ledger reconcile started (interval=%s)", ledgerReconcileEvery)
		for {
			select {
			case <-ctx.Done():
				log.Println("[FLEET-LEDGER] ledger reconcile stopped (context cancelled)")
				return
			case <-ticker.C:
				pass()
			}
		}
	}()
}

// ledgerReconcileEvery is the report-only ledger↔reality audit interval. Slow on
// purpose: a divergence here is permanent (it never heals on its own), while the
// pass costs an object-store HEAD per recovery point, so there is nothing to buy
// by running it often.
const ledgerReconcileEvery = 6 * time.Hour

// stagingSweepEvery is the orphaned-staging-directory GC interval. Hourly: the
// boot path reclaims its own failures immediately, so this loop only has to
// catch what a killed process left behind.
const stagingSweepEvery = time.Hour

// startFleetActivityFeed starts the Caddy access-log tailer that backs the
// SweepIdle direct-serve guard (#fleet-scale-to-zero-activity-feed, D-122). No-op
// when the feed is not wired (non-firecracker executor or no access-log path).
// The feed's Run is ctx-aware + panic-recovering and exits on ctx cancel; it is
// started BEFORE the reconciler so it can warm before the first sweep.
func (c *Container) startFleetActivityFeed(ctx context.Context) {
	if c.FleetActivityFeed == nil {
		return
	}
	go c.FleetActivityFeed.Run(ctx)
	log.Println("[FLEET-ACTIVITY] access-log activity feed started")
}

// startFleetReconciler runs the CP4 §9#7 fleet reconcile loop, driving every
// app's replica set toward its desired count every 10s. Firecracker host only
// (the orchestrator is nil otherwise). ctx-aware + panic-recovering.
func (c *Container) startFleetReconciler(ctx context.Context) {
	if c.FleetOrchestratorUC == nil {
		return
	}
	orch := c.FleetOrchestratorUC
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[FLEET-RECONCILE] reconcile loop panicked: %v", r)
			}
		}()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		log.Println("[FLEET-RECONCILE] reconcile loop started (interval=10s)")
		for {
			select {
			case <-ctx.Done():
				log.Println("[FLEET-RECONCILE] reconcile loop stopped (context cancelled)")
				return
			case <-ticker.C:
				if err := orch.ReconcileAll(ctx); err != nil {
					log.Printf("[FLEET-RECONCILE] reconcile error: %v", err)
				}
				// rt#11 — scale idle scale-to-zero apps down to zero replicas.
				// Log-and-continue: a sweep error must never stop reconcile.
				if err := orch.SweepIdle(ctx); err != nil {
					log.Printf("[FLEET-SWEEP] idle sweep error: %v", err)
				}
				// rt#8 — push the current route set to the ingress gateway.
				// Log-and-continue: an unreachable Caddy must never stop reconcile.
				if err := orch.SyncIngress(ctx); err != nil {
					log.Printf("[FLEET-INGRESS] sync error: %v", err)
				}
			}
		}
	}()
}

// startFleetHeartbeat runs the self-host heartbeat loop (runtime-fleet CP4
// §9#4). Only active when this instance self-registered (firecracker executor).
// Allocatable == full capacity until the §9#5 scheduler tracks precise usage.
func (c *Container) startFleetHeartbeat(ctx context.Context) {
	if c.fleetSelf == nil || c.FleetHostRegistry == nil {
		return
	}
	self := c.fleetSelf
	every := c.fleetHeartbeatEvery
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[FLEET-HB] heartbeat loop panicked: %v", r)
			}
		}()
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		log.Printf("[FLEET-HB] heartbeat loop started (host_id=%s interval=%s)", self.ID, every)
		for {
			select {
			case <-ctx.Done():
				log.Println("[FLEET-HB] heartbeat loop stopped (context cancelled)")
				return
			case <-ticker.C:
				if err := c.FleetHostRegistry.Heartbeat(ctx, self.ID,
					self.CapacityVCPU, self.CapacityMemMB, self.CapacityDiskMB,
					string(domain.HostHealthHealthy),
				); err != nil {
					log.Printf("[FLEET-HB] heartbeat failed: %v", err)
				}
			}
		}
	}()
}

// kafkaConsumers returns runtime's underlying platform-kit Kafka consumers for
// the /healthz/consumers ops surface (D-179 Wave-8). runtime runs a single
// shared inbound consumer (EventConsumer) onto which the session-commit and
// canvas-executed handlers register; the wrapper is skipped when Kafka is
// disabled (nil consumer).
func (c *Container) kafkaConsumers() []*kafka.KafkaConsumer {
	var out []*kafka.KafkaConsumer
	if c.EventConsumer != nil {
		if kc := c.EventConsumer.KafkaConsumer(); kc != nil {
			out = append(out, kc)
		}
	}
	return out
}

// StartConsumers launches any inbound Kafka consumers in background
// goroutines. Safe to call even when Kafka is disabled.
func (c *Container) StartConsumers(ctx context.Context) {
	if c.EventConsumer == nil {
		return
	}
	go func() {
		if err := c.EventConsumer.Start(ctx); err != nil {
			log.Printf("[KAFKA] Consumer stopped with error: %v", err)
		}
	}()
	log.Println("Kafka inbound consumer started")
}

// Close gracefully shuts down all resources
func (c *Container) Close() error {
	log.Println("Closing container resources...")

	// Stop background processors
	if c.PendingProcessor != nil {
		c.PendingProcessor.Stop()
	}

	// Stop reconciliation controller
	if c.ReconciliationController != nil {
		c.ReconciliationController.Stop()
	}

	// Drain the test pool scheduler before closing Kafka so in-flight
	// jobs complete their dispatch path.
	if c.TestPool != nil {
		c.TestPool.Stop()
	}

	// runtime-fleet CP3 — wait for detached image-boot test runs to finish so
	// their results are persisted before the DB pool closes.
	if c.FleetProvisionUC != nil {
		c.FleetProvisionUC.Wait()
	}

	// D-184 — wait for an in-flight restore to reach its terminal phase before the
	// DB pool closes, so it never dies between the swap and the phase write.
	if c.ResourceRestorer != nil {
		c.ResourceRestorer.Wait()
	}

	// Stop the durability metric collector (waits for the loop) BEFORE the DB pool
	// closes, so a final pass never queries a closed pool and reports a fabricated
	// collection error on the way out.
	if c.FleetDurabilityMetrics != nil {
		c.FleetDurabilityMetrics.Stop()
	}

	// D-200 — stop the second-domain mirror (waits for the in-flight pass) BEFORE
	// the DB pool closes, so a confirmed copy is never left unrecorded because the
	// ledger write raced shutdown.
	if c.RecoveryPointMirror != nil {
		c.RecoveryPointMirror.Stop()
	}

	// CP4.5 §9#3 (D-183) — stop the shared-tier TTL reaper (waits for the loop).
	if c.FleetResourceSharedUC != nil {
		c.FleetResourceSharedUC.Stop()
	}

	// runtime-fleet P3.4 — stop the Vault client's background lease renewer.
	// ⚠ Must stay AFTER the provision/restore waits above: the secret resolver now
	// shares this client's transport, so closing it also kills the X509Source that
	// verifies in-flight secret resolves — not only the lease renewer.
	if c.vaultClient != nil {
		_ = c.vaultClient.Close()
	}

	// vsock warm pool — cancel the idle-cleanup loop and shut down each pooled
	// agent. ⚠ Must stay BEFORE the c.FCPools drain below: its VMs were booted
	// through c.FCProvider, which hands out clones from c.FCPools[0] (SetPool),
	// so closing the FCPools first would pull the substrate out from under the
	// VMs this Close() is trying to terminate gracefully.
	if c.VsockPool != nil {
		c.VsockPool.Close()
	}

	// §9.1 — drain every warm pool before releasing upstream resources
	// so booted VMs get a chance to terminate gracefully.
	for _, pool := range c.FCPools {
		if pool != nil {
			pool.Close(context.Background())
		}
	}
	// Pre-warm clone buffer: cancel the replenisher goroutines and destroy
	// every buffered ready clone (freeing its index) so no warm VMs / netns
	// leak past shutdown. No-op when readyN==0 (nothing was started).
	if c.WarmPool != nil {
		if err := c.WarmPool.Close(); err != nil {
			log.Printf("Warning: failed to close warm pool: %v", err)
		}
	}
	// §9.3 — stop the per-VM snapshot scheduler.
	if c.FCCheckpointScheduler != nil {
		c.FCCheckpointScheduler.Close()
	}

	// Close event consumer
	if c.EventConsumer != nil {
		if err := c.EventConsumer.Close(); err != nil {
			log.Printf("Warning: failed to close event consumer: %v", err)
		}
	}

	// Close event publisher
	if c.EventPublisher != nil {
		if err := c.EventPublisher.Close(); err != nil {
			log.Printf("Warning: failed to close event publisher: %v", err)
		}
	}

	// Phase 2 mTLS — release the outbound canvas client's SPIFFE source.
	if c.canvasClientSource != nil {
		if err := c.canvasClientSource.Close(); err != nil {
			log.Printf("Warning: failed to close canvas client SPIFFE source: %v", err)
		}
	}

	if c.DB != nil {
		if err := postgres.Close(c.DB); err != nil {
			return fmt.Errorf("failed to close database: %w", err)
		}
	}

	log.Println("Container resources closed successfully")
	return nil
}

// initFirecrackerPools constructs one VMPool per configured language
// profile and wires the first pool into the provider via SetPool so
// Provider.Boot hits the warm path when available. The remaining
// pools stay on c.FCPools for future per-language Acquire plumbing
// (kept alive + Started so refill keeps them warm). §9.1.
func (c *Container) initFirecrackerPools(cfg *config.Config) {
	if c.FCProvider == nil {
		return
	}
	languages := cfg.Firecracker.PoolLanguages
	if len(languages) == 0 {
		log.Println("[FC-POOL] pool_languages empty — warm pool disabled (cold boots only)")
		return
	}
	size := cfg.Firecracker.PoolSizePerProfile
	if size <= 0 {
		size = cfg.Firecracker.PoolSize
	}
	if size <= 0 {
		size = 2
	}
	vcpu := cfg.Firecracker.DefaultVCPU
	memMB := cfg.Firecracker.DefaultMemMB
	for _, langStr := range languages {
		lang := domain.Language(langStr)
		pool, err := firecracker.NewVMPool(firecracker.VMPoolOptions{
			Size:      size,
			Language:  lang,
			VCPU:      vcpu,
			MemoryMB:  memMB,
			Boot:      c.FCProvider.Boot,
			Terminate: c.FCProvider.Terminate,
		})
		if err != nil {
			log.Printf("[FC-POOL] skipping language %s: %v", lang, err)
			continue
		}
		c.FCPools = append(c.FCPools, pool)
	}
	if len(c.FCPools) > 0 {
		// Provider holds a single optional pool — wire the first one so
		// Boot hits it fast-path. Remaining pools are kept as-is; they
		// can be leveraged by a future per-language router but for now
		// simply pre-warm capacity for their rootfs profile.
		c.FCProvider.SetPool(c.FCPools[0])
		log.Printf("[FC-POOL] %d pool(s) wired (size=%d, first_capacity=%d)",
			len(c.FCPools), size, c.FCPools[0].Capacity())
	}
}

// initFirecrackerCheckpointScheduler wires the per-VM auto-snapshot
// scheduler. Registration happens inside Provider.Boot, which already
// calls SetCheckpointScheduler-registered VMs. Start/Close are driven
// by the container lifecycle hooks. §9.3.
func (c *Container) initFirecrackerCheckpointScheduler(cfg *config.Config) {
	if c.FCProvider == nil || !cfg.Firecracker.EnableCheckpointScheduler {
		return
	}
	backend := &firecracker.ProviderSnapshotBackend{P: c.FCProvider}
	c.FCCheckpointScheduler = firecracker.NewCheckpointScheduler(backend)
	c.FCProvider.SetCheckpointScheduler(c.FCCheckpointScheduler)
	interval := cfg.Firecracker.CheckpointIntervalMinutes
	if interval <= 0 {
		interval = firecracker.DefaultCheckpointIntervalMinutes
	}
	log.Printf("[FC-CHECKPOINT] scheduler wired (interval=%dm)", interval)
}

// buildSnapshotStore constructs the durable snapshot ArtifactStore when
// snapshot_store.enabled is set: an S3/MinIO backend (source of truth)
// fronted by a local FilesystemStore cache (fast restore path). Any
// construction failure is logged and yields nil so the snapshot service
// degrades to the local-only flow rather than failing service start.
func (c *Container) buildSnapshotStore(cfg *config.Config) usecase.ArtifactStore {
	sc := cfg.SnapshotStore
	if !sc.Enabled {
		return nil
	}
	// D-200: name the miss. The credential has no default any more, so an
	// unconfigured host lands here — and the failure it would otherwise produce
	// is a bare 403 from the bucket probe, which reads like a MinIO problem
	// rather than a missing env var. This is a diagnostic, NOT a fail-closed
	// gate: the store still degrades to nil below, exactly as before. Whether a
	// host that cannot protect its data should refuse to serve at all is the
	// open durability-gate question and an owner call, not a config edit.
	if sc.AccessKey == "" || sc.SecretKey == "" {
		log.Printf("[snapshot-store] MISCONFIGURED: snapshot_store.enabled=true but no credential " +
			"(set APP_SNAPSHOT_STORE_ACCESS_KEY and APP_SNAPSHOT_STORE_SECRET_KEY); " +
			"this host will fall back to LOCAL-ONLY snapshots and holds no durable recovery points")
	}
	remote, err := objectstore.NewS3ArtifactStore(objectstore.S3Config{
		Endpoint:  sc.Endpoint,
		Region:    sc.Region,
		Bucket:    sc.Bucket,
		AccessKey: sc.AccessKey,
		SecretKey: sc.SecretKey,
		UseSSL:    sc.UseSSL,
		PathStyle: sc.PathStyle,
	})
	if err != nil {
		log.Printf("[snapshot-store] disabled: failed to init S3 backend: %v", err)
		return nil
	}
	// NO local cache in front of this store, deliberately.
	//
	// The only local implementation is FilesystemStore, which is CONTENT-ADDRESSED:
	// its Put streams the body to a temp file, hashes it, and refuses the write
	// unless sha256(content) == key (see its ErrArtifactIntegrity path). That is
	// correct for its real user, the hermetic-build artifact store, whose keys ARE
	// digests.
	//
	// Every key on THIS store is a path, not a digest -- "snapshots/<id>/mem",
	// "volumes/<vol>/<snap>.ext4", warm templates. So the check could never pass:
	// each Put wrote a full-size temp file, hashed it, mismatched, deleted it, and
	// returned an error CachingStore records but never surfaces. The cache
	// therefore never held a single object and never served a read, while costing
	// a second full-size write of every snapshot -- on the fleet host's own disk,
	// where free space is what keeps customer VMs alive.
	//
	// Caching multi-GB volume images locally would be the wrong goal anyway: they
	// are written once and read only on restore, so the cache would trade the disk
	// live volumes need for a hit that almost never comes. If a warm-path cache is
	// ever wanted, it needs a path-keyed local store, not this one.
	log.Printf("[snapshot-store] enabled (bucket=%s, endpoint=%s, no local cache)", sc.Bucket, sc.Endpoint)
	return remote
}

// dockerAvailable returns true if the Docker CLI is installed and the daemon
// is reachable (i.e. "docker info" exits 0).
func dockerAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "info")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

// HealthCheck performs health checks on all critical dependencies
func (c *Container) HealthCheck(ctx context.Context) error {
	if err := postgres.HealthCheck(ctx, c.DB); err != nil {
		return fmt.Errorf("database health check failed: %w", err)
	}

	return nil
}

// --- CS-2 G2.7 adapters --------------------------------------------

// smokeTestListerAdapter wraps *postgres.TestRunRepo and exposes the
// narrow SmokeTestLister surface the continuous-testing trigger needs.
// Kept here rather than in handler/event so the adapter doesn't pull
// runtime-service/repository into that package.
type smokeTestListerAdapter struct {
	repo *postgres.TestRunRepo
}

func (a *smokeTestListerAdapter) ListSmokeTests(ctx context.Context, canvasID uuid.UUID, baseBranch string) ([]domain.TestRun, error) {
	if a == nil || a.repo == nil {
		return nil, nil
	}
	return a.repo.FindSmokeTestsForCanvas(ctx, canvasID, baseBranch)
}

// continuousEventPublisherAdapter wraps the runtime-service kafka
// publisher (which expects a payload) into the event-handler-facing
// surface (which passes any).
type continuousEventPublisherAdapter struct {
	publisher usecase.EventPublisher
}

func (a *continuousEventPublisherAdapter) Publish(ctx context.Context, eventType, key string, data any) error {
	if a == nil || a.publisher == nil {
		return nil
	}
	return a.publisher.Publish(ctx, eventType, key, data)
}

// --- §8.4 executor adapters -----------------------------------------

// execExecutorRunnerAdapter wraps runtime-service's ExecutionUseCase
// into the narrow executors.VMExecRunner surface the visual + a11y
// executors consume. Kept in DI so the adapter doesn't pull the full
// execution wiring into the executor package.
//
// The adapter submits a synchronous Execution and flattens it to a
// stdout / stderr / exit-code triple — the shape every per-type
// executor expects when parsing tool-specific JSON output.
type execExecutorRunnerAdapter struct {
	execUC usecase.ExecutionUseCase
}

func (a *execExecutorRunnerAdapter) ExecuteInVM(ctx context.Context, req executors.VMExecRequest) (*executors.VMExecResult, error) {
	if a == nil || a.execUC == nil {
		return nil, fmt.Errorf("execExecutorRunnerAdapter: execUC unset")
	}
	envVars := domain.JSONMap{}
	for k, v := range req.EnvVars {
		envVars[k] = v
	}
	input := usecase.CreateExecutionInput{
		OrganizationID: req.OrganizationID,
		Language:       req.Language,
		Code:           req.Command,
		Resources: &domain.ResourceLimit{
			TimeoutSec: req.TimeoutSec,
		},
		EnvVars: envVars,
	}
	exec, err := a.execUC.ExecuteSync(ctx, input)
	if err != nil {
		return nil, err
	}
	exitCode := 0
	if exec.ExitCode != nil {
		exitCode = *exec.ExitCode
	}
	return &executors.VMExecResult{
		Stdout:   exec.Stdout,
		Stderr:   exec.Stderr,
		ExitCode: exitCode,
	}, nil
}

// profileDroppingDispatcher bridges the visual / a11y executor's
// `DispatchInVM(ctx, run)` surface into MultiTypeDispatcher's
// `DispatchInVM(ctx, run, profile)` surface. The profile is discarded
// because both executors derive their command from TestRun.ResultJSON
// (script / url / image / etc) rather than from the profile row —
// visual+a11y test types don't carry language-keyed command templates.
type profileDroppingDispatcher struct {
	inner interface {
		DispatchInVM(ctx context.Context, run *domain.TestRun) error
	}
}

func (d *profileDroppingDispatcher) DispatchInVM(ctx context.Context, run *domain.TestRun, _ usecase.TestRunnerProfile) error {
	if d == nil || d.inner == nil {
		return nil
	}
	return d.inner.DispatchInVM(ctx, run)
}
