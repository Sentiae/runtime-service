package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	pkdebug "github.com/sentiae/platform-kit/debug"
	pkkafka "github.com/sentiae/platform-kit/kafka"
	"github.com/sentiae/runtime-service/internal/di"
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

	// Initialize logger
	logger.Init()

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
			if err := container.GRPCServer.GetGRPCServer().Serve(lis); err != nil {
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
