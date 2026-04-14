package di

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	kafka "github.com/sentiae/platform-kit/kafka"

	grpchandler "github.com/sentiae/runtime-service/internal/handler/grpc"
	httphandler "github.com/sentiae/runtime-service/internal/handler/http"
	"github.com/sentiae/runtime-service/internal/infrastructure/agent"
	"github.com/sentiae/runtime-service/internal/infrastructure/container"
	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker"
	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker/vmcomm"
	"github.com/sentiae/runtime-service/internal/infrastructure/foundry"
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
	TestRunRepo *postgres.TestRunRepo

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

	// Inbound event handler service (canvas → runtime execution requests).
	InboundEventHandlerSvc *usecase.InboundEventHandlerService

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
		}

		// Initialize inbound event consumer (canvas → runtime).
		// Platform-kit derives the Kafka topic from event type as
		// "{prefix}.{domain}.{resource}", so the event
		// "sentiae.canvas.code.execute_requested" is published to topic
		// "sentiae.canvas.code". Subscribe to the topic; event-type
		// matching happens in EventConsumer.Handle().
		topics := cfg.Kafka.InboundTopics
		if len(topics) == 0 {
			topics = []string{"sentiae.canvas.code"}
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
	c.ExecutionRepo = postgres.NewExecutionRepository(c.DB)
	c.VMRepo = postgres.NewMicroVMRepository(c.DB)
	c.SnapshotRepo = postgres.NewSnapshotRepository(c.DB)
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
	testRunRepo := postgres.NewTestRunRepository(c.DB)
	c.TestRunRepo = testRunRepo

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

	// Test generation + affected test resolution (Items 51 / 52).
	// Foundry client is optional; noop fallback yields deterministic scaffolds.
	c.TestGenUC = usecase.NewTestGenerationService(foundry.NewNoopClient())
	// Symbol graph from git-service is not wired yet; heuristic mode is used
	// until that integration lands.
	c.AffectedUC = usecase.NewAffectedTestResolver(nil)

	// B6 gap #1 — automatic VM snapshot scheduler.
	c.CheckpointScheduler = usecase.NewCheckpointScheduler(
		c.VMInstanceRepo,
		c.SnapshotRepo,
		c.VMProvider,
		c.EventPublisher,
		0, // use DefaultCheckpointInterval
	)

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
	// 9.4 — agent registry + dispatcher HTTP surface.
	c.HTTPServer.SetRuntimeAgentHandler(httphandler.NewRuntimeAgentHandler(c.AgentRegistry, c.RuntimeAgentRepo))

	// B6 gap #5 — regression tests from production traces.
	c.HTTPServer.SetRegressionTestHandler(
		httphandler.NewRegressionTestHandler(c.RegressionGenerator, c.RegressionTestRepo),
	)

	if c.Config.Server.GRPC.Enabled {
		c.GRPCServer = grpchandler.NewServer(
			grpchandler.ServerConfig{
				EnableLogging:  c.Config.App.Environment == "development",
				EnableRecovery: true,
			},
			c.ExecutionUC,
		)
	}

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
