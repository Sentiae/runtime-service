package grpc

import (
	"context"
	"log/slog"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	pkconfig "github.com/sentiae/platform-kit/config"
	"github.com/sentiae/platform-kit/grpcserver"
	"github.com/sentiae/platform-kit/interceptor"
	"github.com/sentiae/platform-kit/spiffe"
	"github.com/sentiae/platform-kit/tenant"
	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// Server represents the gRPC server with all service implementations.
type Server struct {
	builder         *grpcserver.Builder
	executionServer *ExecutionServer
	graphServer     *GraphServer
	healthServer    *health.Server
	source          *workloadapi.X509Source
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
	// SVID-authz mesh policy (T-SEC-FND Wave 4): governs Principal.CanActInOrg for
	// SVID/api-key service callers consulted by the tenant.* org checks. Neutral at
	// default env (strict=false).
	tenant.SetServiceGrants(tenant.LoadMeshPolicy())
	tenant.SetMeshSVIDAuthzStrict(pkconfig.MeshSVIDAuthzStrict())
	unary, stream := interceptor.NewChain(interceptor.Config{
		Logger: slog.Default(),
		Auth: &interceptor.AuthConfig{
			APIKeyValidator: svcToken,
			TokenValidator:  jwks,
			AcceptAPIKey:    pkconfig.AcceptAPIKey(),
			RequirePeerSVID: pkconfig.RequirePeerSVID(),
			SkipMethods: []string{
				"/grpc.health.v1.Health/Check",
				"/grpc.health.v1.Health/Watch",
				"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
				"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
			},
		},
	})

	// Phase 2 mTLS mesh: build a dual-mode server. Mode "off" (default) yields
	// one plaintext server identical to before. When a mode is requested but
	// SPIRE is unreachable, the builder degrades to plaintext-only rather than
	// crash. Reflection is registered by the builder at Serve time (grpcurl /
	// grpcui can still introspect in dev + staging).
	var src *workloadapi.X509Source
	if pkconfig.MTLSMode() != pkconfig.MTLSModeOff {
		s, srcErr := spiffe.NewSource(context.Background())
		if srcErr != nil {
			slog.Default().Warn("runtime gRPC: SPIFFE source unavailable, degrading to plaintext", "err", srcErr)
		} else {
			src = s
		}
	}

	b := grpcserver.New(grpcserver.Config{
		Mode:        pkconfig.MTLSMode(),
		Source:      src,
		ServiceName: "runtime",
	},
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),
	)

	// Register health check service
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(b.Registrar(), healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	healthServer.SetServingStatus("runtime.v1.RuntimeService", grpc_health_v1.HealthCheckResponse_SERVING)

	// Create service implementations
	executionServer := NewExecutionServer(executionUC)
	graphServer := NewGraphServer(graphUC, execEngine)

	// Register services
	runtimev1.RegisterRuntimeServiceServer(b.Registrar(), executionServer)
	runtimev1.RegisterGraphServiceServer(b.Registrar(), graphServer)
	healthServer.SetServingStatus("runtime.v1.GraphService", grpc_health_v1.HealthCheckResponse_SERVING)

	return &Server{
		builder:         b,
		executionServer: executionServer,
		graphServer:     graphServer,
		healthServer:    healthServer,
		source:          src,
	}
}

// RegisterFleet registers the FleetOrchestration service (runtime-fleet CP3).
// Safe to call after NewServer but before Serve; the shared auth interceptor
// applies (service principals present the x-api-key like every other RPC).
func (s *Server) RegisterFleet(fleet *FleetServer) {
	runtimev1.RegisterFleetOrchestrationServer(s.builder.Registrar(), fleet)
	if s.healthServer != nil {
		s.healthServer.SetServingStatus("runtime.v1.FleetOrchestration", grpc_health_v1.HealthCheckResponse_SERVING)
	}
}

// GetGRPCServer returns a primary underlying gRPC server for introspection.
// Use Serve for the real listen path so every configured transport is served.
func (s *Server) GetGRPCServer() *grpc.Server {
	return s.builder.Server()
}

// Serve serves every configured transport on lis (plaintext and, in
// permissive/strict modes, mTLS via cmux). Blocks until the listener closes.
func (s *Server) Serve(lis net.Listener) error {
	return s.builder.Serve(lis)
}

// ExecutionServer returns the underlying ExecutionServer so callers (DI)
// can attach additional dependencies after construction.
func (s *Server) ExecutionServer() *ExecutionServer {
	return s.executionServer
}

// Shutdown gracefully shuts down the gRPC server.
func (s *Server) Shutdown() {
	s.builder.GracefulStop()
	if s.source != nil {
		_ = s.source.Close()
	}
}
