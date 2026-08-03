package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	pkdebug "github.com/sentiae/platform-kit/debug"
	pkkafka "github.com/sentiae/platform-kit/kafka"
	pklogger "github.com/sentiae/platform-kit/logger"
	otelkit "github.com/sentiae/platform-kit/otel"
	"github.com/sentiae/runtime-service/internal/app"
	"github.com/sentiae/runtime-service/internal/di"
	"github.com/sentiae/runtime-service/internal/version"
	"github.com/sentiae/runtime-service/pkg/config"
	"github.com/sentiae/runtime-service/pkg/logger"
)

var (
	// Build information (set by build flags)
	Version   = "dev"
	BuildTime = "unknown"
)

// maybeRegisterKafkaSchemas runs the G17 schema-registry bootstrap.
// Gated by APP_KAFKA_REGISTER_SCHEMAS_ON_BOOT=true.
func maybeRegisterKafkaSchemas() {
	if os.Getenv("APP_KAFKA_REGISTER_SCHEMAS_ON_BOOT") != "true" {
		return
	}
	url := os.Getenv("APP_KAFKA_SCHEMA_REGISTRY_URL")
	if url == "" {
		return
	}
	prefix := os.Getenv("APP_KAFKA_TOPIC_PREFIX")
	if prefix == "" {
		prefix = "sentiae"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	registry := pkkafka.NewSchemaRegistry(url)
	result := pkkafka.RegisterAllSchemas(ctx, registry, prefix)
	if len(result.Errors) > 0 {
		log.Printf("schema-registry bootstrap: registered=%d skipped=%d errors=%d (first: %s)",
			result.Registered, result.Skipped, len(result.Errors), result.Errors[0])
		return
	}
	log.Printf("schema-registry bootstrap: registered %d schemas", result.Registered)
}

func main() {
	stopPprof := pkdebug.StartPprofServer(context.Background(), "RUNTIME_DEBUG_PPROF")
	defer func() { _ = stopPprof() }()
	go maybeRegisterKafkaSchemas()
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize telemetry (traces, metrics & logs → OTLP collector) early so
	// the promauto→OTLP metric bridge is active before any component records
	// (D-179 Wave-8). Non-fatal: a collector outage must not block boot.
	//
	// APP_TELEMETRY_ENABLED=false skips the bootstrap entirely: no exporter, no
	// periodic reader, no background upload goroutine. Hosts with no reachable
	// collector (the bare Firecracker fleet host) set it so the OTel globals
	// stay no-ops instead of retrying an unresolvable endpoint every 60s.
	otelCtx, otelCancel := context.WithCancel(context.Background())
	defer otelCancel()
	var shutdownTelemetry otelkit.Shutdown
	if cfg.Telemetry.Enabled {
		log.Printf("Telemetry: enabled (OTLP endpoint: %s)", cfg.Telemetry.OTLPEndpoint)
		// service.instance.id is the DURABLE fleet host id when this process is a
		// fleet host: its series must be joinable to the placement records that key
		// on that uuid, and a hostname is not (a re-IP'd or renamed host would fork a
		// second series that no volume or resource row points at). Empty on a mesh
		// container, where platform-kit's hostname default is correct.
		shutdownTelemetry, err = otelkit.Init(otelCtx, otelkit.Config{
			ServiceName:    cfg.Telemetry.ServiceName,
			ServiceVersion: Version,
			Environment:    cfg.App.Environment,
			Endpoint:       cfg.Telemetry.OTLPEndpoint,
			Insecure:       true,
			InstanceID:     strings.TrimSpace(cfg.Fleet.HostID),
		})
		if err != nil {
			log.Printf("Failed to init telemetry (continuing without it): %v", err)
		}
	} else {
		log.Printf("Telemetry: disabled (APP_TELEMETRY_ENABLED=false) — no OTLP exporter started")
	}
	defer func() {
		if shutdownTelemetry != nil {
			_ = shutdownTelemetry(context.Background())
		}
	}()

	// Wire the shared structured logger as the slog default so every
	// logger.FromContext(ctx) call in the fleet warm-pool/provision paths
	// honors APP_LOGGING_LEVEL/FORMAT instead of silently falling back to
	// slog's uncontrolled default handler. Without this the fleet
	// provisioner's Info/Warn/Error lines never reach journald (only GORM
	// SQL was visible), leaving fleet failures invisible for RCA.
	slog.SetDefault(pklogger.New(pklogger.Config{
		Level:  cfg.Logging.Level,
		Format: cfg.Logging.Format,
	}))

	// Initialize logger
	logger.Init()

	// Bind the domain/library error sentinels to their HTTP/gRPC codes before
	// anything can serve (§16.3). Must precede the gRPC server: an unregistered
	// sentinel silently degrades to codes.Internal at the boundary.
	app.RegisterErrors()

	// Deploy provenance, structured so it is queryable in the log plane. The
	// same VCS_REVISION build arg labels the image
	// (org.opencontainers.image.revision), so this line and `docker inspect`
	// cannot disagree about what source is running.
	pklogger.FromContext(context.Background()).Info("runtime-service build provenance",
		"vcs.revision", version.Revision,
		"vcs.modified", version.Modified,
		"version", Version,
		"build_time", BuildTime,
	)

	log.Printf("Starting Runtime Service v%s (built: %s)", Version, BuildTime)
	log.Printf("Environment: %s", cfg.App.Environment)

	// Initialize DI container
	log.Println("Initializing dependency injection container...")
	container, err := di.NewContainer(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize DI container: %v", err)
	}
	defer func() {
		if err := container.Close(); err != nil {
			log.Printf("Error closing container: %v", err)
		}
	}()

	log.Println("DI container initialized successfully")

	// Start background controllers (reconciliation loop)
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	container.StartBackgroundControllers(bgCtx)

	// Perform initial health check
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := container.HealthCheck(ctx); err != nil {
		log.Printf("Warning: Initial health check failed: %v", err)
		log.Println("Service will start in degraded mode")
	} else {
		log.Println("All health checks passed")
	}
	cancel()

	// Setup HTTP server
	httpPort := cfg.Server.HTTP.Port
	httpServer := &http.Server{
		Addr:         ":" + httpPort,
		Handler:      container.HTTPServer,
		ReadTimeout:  cfg.Server.HTTP.Timeouts.Read,
		WriteTimeout: cfg.Server.HTTP.Timeouts.Write,
		IdleTimeout:  cfg.Server.HTTP.Timeouts.Idle,
	}

	// Start HTTP server in goroutine
	go func() {
		log.Printf("HTTP server starting on port %s", httpPort)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	// Start gRPC server if enabled
	if cfg.Server.GRPC.Enabled && container.GRPCServer != nil {
		grpcAddr := cfg.Server.GRPC.Host + ":" + cfg.Server.GRPC.Port
		go func() {
			lis, err := net.Listen("tcp", grpcAddr)
			if err != nil {
				log.Fatalf("Failed to listen for gRPC: %v", err)
			}
			log.Printf("gRPC server starting on %s", grpcAddr)
			if err := container.GRPCServer.Serve(lis); err != nil {
				log.Fatalf("gRPC server failed: %v", err)
			}
		}()
	}

	// Wait for interrupt signal to gracefully shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit

	log.Printf("Received signal: %v", sig)
	log.Println("Shutting down servers gracefully...")

	// Create shutdown context with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Shutdown gRPC server
	if container.GRPCServer != nil {
		container.GRPCServer.Shutdown()
		log.Println("gRPC server stopped gracefully")
	}

	// Shutdown HTTP server
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server forced to shutdown: %v", err)
	} else {
		log.Println("HTTP server stopped gracefully")
	}

	log.Println("Runtime Service shut down successfully")
}

// checkDatabaseConnection is a helper function to check database connection with retry
func checkDatabaseConnection(container *di.Container, maxRetries int) error {
	for i := 0; i < maxRetries; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := container.HealthCheck(ctx)
		cancel()

		if err == nil {
			return nil
		}

		if i < maxRetries-1 {
			log.Printf("Database connection attempt %d failed: %v, retrying...", i+1, err)
			time.Sleep(time.Duration(i+1) * time.Second)
		}
	}

	return fmt.Errorf("failed to connect to database after %d attempts", maxRetries)
}
