package di

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	grpchandler "github.com/sentiae/runtime-service/internal/handler/grpc"
	httphandler "github.com/sentiae/runtime-service/internal/handler/http"
	"github.com/sentiae/runtime-service/internal/infrastructure/firecracker"
	"github.com/sentiae/runtime-service/internal/infrastructure/messaging"
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

	// Infrastructure
	FCProvider     *firecracker.Provider
	EventPublisher messaging.EventPublisher

	// Use Cases
	ExecutionUC  usecase.ExecutionUseCase
	VMUC         usecase.VMUseCase
	SnapshotUC   usecase.SnapshotUseCase
	VMInstanceUC usecase.VMInstanceUseCase
	SchedulerUC  usecase.SchedulerUseCase
	TerminalUC   usecase.TerminalUseCase

	// Graph Use Cases
	GraphUC       usecase.GraphUseCase
	GraphEngine   *usecase.GraphExecutionEngine
	GraphDebugUC  usecase.GraphDebugUseCase
	GraphReplayUC usecase.GraphReplayUseCase
	TraceRecorder *usecase.GraphTraceRecorder

	// Controllers
	ReconciliationController *usecase.ReconciliationController
	PendingProcessor         *usecase.PendingProcessor

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
	c.FCProvider = firecracker.NewProvider(cfg.Firecracker)
	log.Println("Firecracker provider initialized")

	// Initialize Kafka event publisher
	if cfg.Kafka.Enabled && len(cfg.Kafka.Brokers) > 0 {
		publisher, err := messaging.NewKafkaPublisher(cfg.Kafka.Brokers, cfg.Kafka.Topic)
		if err != nil {
			log.Printf("Warning: failed to initialize Kafka publisher: %v (using noop)", err)
			c.EventPublisher = &messaging.NoopPublisher{}
		} else {
			c.EventPublisher = publisher
			log.Println("Kafka event publisher initialized")
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

	log.Println("Repositories initialized (PostgreSQL)")
}

// initUseCases initializes all use cases
func (c *Container) initUseCases(cfg *config.Config) {
	c.VMUC = usecase.NewVMService(c.VMRepo, c.FCProvider)
	c.SnapshotUC = usecase.NewSnapshotService(c.SnapshotRepo, c.VMRepo, c.FCProvider)
	c.ExecutionUC = usecase.NewExecutionService(c.ExecutionRepo, c.MetricsRepo, c.VMUC, c.FCProvider, c.FCProvider)

	// Initialize scheduler with localhost resources
	localVCPU := cfg.Firecracker.MaxVCPU
	if localVCPU <= 0 {
		localVCPU = 8
	}
	localMemMB := cfg.Firecracker.MaxMemMB
	if localMemMB <= 0 {
		localMemMB = 8192
	}
	c.SchedulerUC = usecase.NewScheduler(localVCPU, localMemMB)

	// Initialize VM instance service with reconciliation support
	c.VMInstanceUC = usecase.NewVMInstanceService(c.VMInstanceRepo, c.FCProvider, c.SchedulerUC)

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
	c.TerminalUC = usecase.NewTerminalService(c.TerminalSessionRepo, c.VMUC, c.FCProvider)

	// Initialize pending processor (2-second interval, batch size 10)
	c.PendingProcessor = usecase.NewPendingProcessor(c.ExecutionUC, c.GraphEngine, 2*time.Second, 10)

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

// HealthCheck performs health checks on all critical dependencies
func (c *Container) HealthCheck(ctx context.Context) error {
	if err := postgres.HealthCheck(ctx, c.DB); err != nil {
		return fmt.Errorf("database health check failed: %w", err)
	}

	return nil
}
