package grpc

import (
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/sentiae/platform-kit/interceptor"
	"github.com/sentiae/platform-kit/tenant"
	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// Server represents the gRPC server with all service implementations.
type Server struct {
	grpcServer      *grpc.Server
	executionServer *ExecutionServer
	graphServer     *GraphServer
	healthServer    *health.Server
}

// ServerConfig holds configuration for the gRPC server.
type ServerConfig struct {
	// EnableLogging enables request/response logging.
	EnableLogging bool

	// EnableRecovery enables panic recovery interceptor.
	EnableRecovery bool

	// ServiceAPIKey is the shared service-to-service token validated as the
	// x-api-key header; a match marks the caller as a trusted service principal.
	ServiceAPIKey string

	// JWKSURL + JWTIssuer configure the JWKS-backed user-token validator so
	// handlers derive the trusted actor + org from the verified principal.
	JWKSURL   string
	JWTIssuer string
}

// NewServer creates a new gRPC server with interceptors and service registrations.
func NewServer(
	config ServerConfig,
	executionUC usecase.ExecutionUseCase,
	graphUC usecase.GraphUseCase,
	execEngine *usecase.GraphExecutionEngine,
) *Server {
	// Mandatory server interceptor chain (CLAUDE.md §23): Recovery → Logging →
	// Auth, built by interceptor.NewChain. Auth layers a service-token
	// (x-api-key) validator with a JWKS-backed user-token validator so handlers
	// derive the trusted actor/org from the verified principal (tenant.FromContext)
	// instead of the spoofable x-user-id/x-organization-id metadata. Health +
	// reflection are skipped so k8s probes and grpcurl keep working unauthenticated.
	svcToken := tenant.ServiceTokenValidator{Expected: config.ServiceAPIKey}
	jwks, err := tenant.NewJWKSValidator(tenant.JWKSConfig{JWKSURL: config.JWKSURL, Issuer: config.JWTIssuer})
	if err != nil {
		// api-key-only fallback: user JWTs can't be verified, but service callers
		// still authenticate. Never fail boot on a JWKS build error.
		slog.Default().Warn("runtime gRPC: JWKS user-token validator unavailable, falling back to api-key-only auth", "err", err)
		jwks = nil
	}
	unary, stream := interceptor.NewChain(interceptor.Config{
		Logger: slog.Default(),
		Auth: &interceptor.AuthConfig{
			APIKeyValidator: svcToken,
			TokenValidator:  jwks,
			SkipMethods: []string{
				"/grpc.health.v1.Health/Check",
				"/grpc.health.v1.Health/Watch",
				"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
				"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
			},
		},
	})

	// Create gRPC server with interceptors
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),
	)

	// Register health check service
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	healthServer.SetServingStatus("runtime.v1.RuntimeService", grpc_health_v1.HealthCheckResponse_SERVING)

	// Create service implementations
	executionServer := NewExecutionServer(executionUC)
	graphServer := NewGraphServer(graphUC, execEngine)

	// Register services
	runtimev1.RegisterRuntimeServiceServer(grpcServer, executionServer)
	runtimev1.RegisterGraphServiceServer(grpcServer, graphServer)
	healthServer.SetServingStatus("runtime.v1.GraphService", grpc_health_v1.HealthCheckResponse_SERVING)

	// Enable server reflection so grpcurl / grpcui can introspect the
	// service in development + staging. Production can strip via build tag
	// later if needed.
	reflection.Register(grpcServer)

	return &Server{
		grpcServer:      grpcServer,
		executionServer: executionServer,
		graphServer:     graphServer,
		healthServer:    healthServer,
	}
}

// RegisterFleet registers the FleetOrchestration service (runtime-fleet CP3).
// Safe to call after NewServer but before Serve; the shared auth interceptor
// applies (service principals present the x-api-key like every other RPC).
func (s *Server) RegisterFleet(fleet *FleetServer) {
	runtimev1.RegisterFleetOrchestrationServer(s.grpcServer, fleet)
	if s.healthServer != nil {
		s.healthServer.SetServingStatus("runtime.v1.FleetOrchestration", grpc_health_v1.HealthCheckResponse_SERVING)
	}
}

// GetGRPCServer returns the underlying gRPC server.
func (s *Server) GetGRPCServer() *grpc.Server {
	return s.grpcServer
}

// ExecutionServer returns the underlying ExecutionServer so callers (DI)
// can attach additional dependencies after construction.
func (s *Server) ExecutionServer() *ExecutionServer {
	return s.executionServer
}

// Shutdown gracefully shuts down the gRPC server.
func (s *Server) Shutdown() {
	s.grpcServer.GracefulStop()
}
