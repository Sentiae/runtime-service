package di

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	kafka "github.com/sentiae/platform-kit/kafka"
	timetravel "github.com/sentiae/platform-kit/timetravel"

	"github.com/sentiae/runtime-service/internal/domain"

	eventhandler "github.com/sentiae/runtime-service/internal/handler/event"
	grpchandler "github.com/sentiae/runtime-service/internal/handler/grpc"
	httphandler "github.com/sentiae/runtime-service/internal/handler/http"
	"github.com/sentiae/runtime-service/internal/infrastructure/agent"
	"github.com/sentiae/runtime-service/internal/infrastructure/canvasservice"
	"github.com/sentiae/runtime-service/internal/infrastructure/container"
	"github.com/sentiae/runtime-service/internal/infrastructure/executors"
	"github.com/sentiae/runtime-service/internal/infrastructure/executors/a11y"
	"github.com/sentiae/runtime-service/internal/infrastructure/executors/visual"
	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker"
	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker/vmcomm"
	"github.com/sentiae/runtime-service/internal/infrastructure/foundry"
	"github.com/sentiae/runtime-service/internal/infrastructure/gitservice"
	"github.com/sentiae/runtime-service/internal/infrastructure/messaging"
	"github.com/sentiae/runtime-service/internal/infrastructure/simulated"
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
	VsockListener     *vmcomm.Listener
	VsockRunner       *vmcomm.Runner

	// §9.1 — warm Firecracker VM pools, one per language profile. Kept
	// on the container so the background controller hook can Start()
	// them at service start and Close() them at shutdown.
	FCPools []*firecracker.VMPool

	// §9.3 — per-VM auto-snapshot scheduler. Distinct from the
	// usecase-layer CheckpointScheduler (DB poller); this one drives
	// Pause→CreateSnapshot→Resume from Boot-time registration.
	FCCheckpointScheduler *firecracker.CheckpointScheduler

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

	// Initialize handlers
	c.initHandlers()

	return c, nil
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

	// Run auto-migrations if enabled
	if cfg.Database.Postgres.Migrations.AutoMigrate {
		if err := postgres.AutoMigrate(db); err != nil {
			return fmt.Errorf("failed to run auto-migrations: %w", err)
		}
		log.Println("Auto-migrations completed successfully")
	}

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
			pool := vmcomm.NewPool(c.FCProvider, fcListener, 1)
			c.ExecutionRunner = vmcomm.NewPoolRunner(pool)
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
		// Simulated provider — no Docker or Firecracker needed
		simProv := simulated.NewProvider()
		c.VMProvider = simProv
		c.ExecutionRunner = simProv
		log.Println("Simulated provider initialized (no Docker/Firecracker required)")
	default:
		// Default to container provider (works on macOS and Linux)
		if dockerAvailable() {
			c.ContainerProvider = container.NewProvider(cfg.Container)
			c.VMProvider = c.ContainerProvider
			c.ExecutionRunner = c.ContainerProvider
			log.Println("Docker container provider initialized (dev mode)")
		} else {
			log.Println("Warning: Docker is not available, falling back to simulated executor")
			simProv := simulated.NewProvider()
			c.VMProvider = simProv
			c.ExecutionRunner = simProv
			log.Println("Simulated provider initialized (Docker fallback)")
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

	log.Println("Repositories initialized (PostgreSQL)")
}

// initUseCases initializes all use cases
func (c *Container) initUseCases(cfg *config.Config) {
	c.VMUC = usecase.NewVMService(c.VMRepo, c.VMProvider)
	c.SnapshotUC = usecase.NewSnapshotService(c.SnapshotRepo, c.VMRepo, c.VMProvider)
	c.ExecutionUC = usecase.NewExecutionService(c.ExecutionRepo, c.MetricsRepo, c.VMUC, c.ExecutionRunner, c.VMProvider)

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
		canvasClient := canvasservice.NewClient(canvasURL, 10*time.Second)
		canvasClient.ServiceToken = os.Getenv("CANVAS_SERVICE_TOKEN")
		canvasClient.ServiceUserID = os.Getenv("SERVICE_USER_ID")
		c.HTTPServer.SetTestRunCanvasClient(canvasClient)
		log.Printf("canvas-service HTTP push enabled (url: %s)", canvasURL)
	}

	// 9.4 — agent registry + dispatcher HTTP surface.
	c.HTTPServer.SetRuntimeAgentHandler(httphandler.NewRuntimeAgentHandler(c.AgentRegistry, c.RuntimeAgentRepo))

	// §9.4 — customer-agent enrolment endpoint. Signs CSRs submitted
	// by freshly-installed customer-hosted agents. Unwired when the CA
	// paths aren't set (dev deployments / tenants not using the agent).
	caCert := os.Getenv("AGENT_CA_CERT_PATH")
	caKey := os.Getenv("AGENT_CA_KEY_PATH")
	if caCert != "" && caKey != "" {
		tokenSource := func() string { return os.Getenv("AGENT_ENROLMENT_TOKEN") }
		c.HTTPServer.SetCustomerAgentCertHandler(
			httphandler.NewCustomerAgentCertHandler(caCert, caKey, tokenSource),
		)
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
	}

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
	c.StartConsumers(ctx)
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

	// §9.1 — drain every warm pool before releasing upstream resources
	// so booted VMs get a chance to terminate gracefully.
	for _, pool := range c.FCPools {
		if pool != nil {
			pool.Close(context.Background())
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
	c.FCCheckpointScheduler = firecracker.NewCheckpointScheduler(backend, nil)
	c.FCProvider.SetCheckpointScheduler(c.FCCheckpointScheduler)
	interval := cfg.Firecracker.CheckpointIntervalMinutes
	if interval <= 0 {
		interval = firecracker.DefaultCheckpointIntervalMinutes
	}
	log.Printf("[FC-CHECKPOINT] scheduler wired (interval=%dm)", interval)
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
