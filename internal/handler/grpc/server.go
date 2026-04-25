package grpc

import (
	"context"
	"log"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/sentiae/runtime-service/gen/proto/runtime/v1"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// Server represents the gRPC server with all service implementations.
type Server struct {
	grpcServer      *grpc.Server
	executionServer *ExecutionServer
	graphServer     *GraphServer
}

// ServerConfig holds configuration for the gRPC server.
type ServerConfig struct {
	// EnableLogging enables request/response logging.
	EnableLogging bool

	// EnableRecovery enables panic recovery interceptor.
	EnableRecovery bool
}

// NewServer creates a new gRPC server with interceptors and service registrations.
func NewServer(
	config ServerConfig,
	executionUC usecase.ExecutionUseCase,
	graphUC usecase.GraphUseCase,
	execEngine *usecase.GraphExecutionEngine,
) *Server {
	// Build interceptor chain
	var unaryInterceptors []grpc.UnaryServerInterceptor
	var streamInterceptors []grpc.StreamServerInterceptor

	// Add recovery interceptor first (outermost)
	if config.EnableRecovery {
		unaryInterceptors = append(unaryInterceptors, recoveryUnaryInterceptor())
		streamInterceptors = append(streamInterceptors, recoveryStreamInterceptor())
	}

	// Add logging interceptor
	if config.EnableLogging {
		unaryInterceptors = append(unaryInterceptors, loggingUnaryInterceptor())
		streamInterceptors = append(streamInterceptors, loggingStreamInterceptor())
	}

	// Add dev auth interceptor to extract user/org info from metadata
	unaryInterceptors = append(unaryInterceptors, devAuthUnaryInterceptor())
	streamInterceptors = append(streamInterceptors, devAuthStreamInterceptor())

	// Create gRPC server with interceptors
	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(unaryInterceptors...),
		grpc.ChainStreamInterceptor(streamInterceptors...),
	}

	grpcServer := grpc.NewServer(opts...)

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

// loggingUnaryInterceptor logs all unary RPC calls.
func loggingUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		start := time.Now()

		userID := getUserIDFromContext(ctx)

		log.Printf("[gRPC] --> %s | user: %s", info.FullMethod, userID)

		resp, err := handler(ctx, req)

		duration := time.Since(start)
		statusCode := codes.OK
		if err != nil {
			statusCode = status.Code(err)
		}

		log.Printf("[gRPC] <-- %s | status: %s | duration: %v | user: %s",
			info.FullMethod, statusCode, duration, userID)

		return resp, err
	}
}

// loggingStreamInterceptor logs all streaming RPC calls.
func loggingStreamInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv any,
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		start := time.Now()

		userID := getUserIDFromContext(ss.Context())

		log.Printf("[gRPC Stream] --> %s | user: %s", info.FullMethod, userID)

		err := handler(srv, ss)

		duration := time.Since(start)
		statusCode := codes.OK
		if err != nil {
			statusCode = status.Code(err)
		}

		log.Printf("[gRPC Stream] <-- %s | status: %s | duration: %v | user: %s",
			info.FullMethod, statusCode, duration, userID)

		return err
	}
}

// recoveryUnaryInterceptor recovers from panics in unary RPCs.
func recoveryUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[gRPC Recovery] panic in %s: %v\n%s", info.FullMethod, r, debug.Stack())
				err = status.Errorf(codes.Internal, "internal server error: %v", r)
			}
		}()

		return handler(ctx, req)
	}
}

// recoveryStreamInterceptor recovers from panics in streaming RPCs.
func recoveryStreamInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv any,
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) (err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[gRPC Recovery] panic in %s: %v\n%s", info.FullMethod, r, debug.Stack())
				err = status.Errorf(codes.Internal, "internal server error: %v", r)
			}
		}()

		return handler(srv, ss)
	}
}

// devAuthUnaryInterceptor extracts user ID from x-user-id metadata in development mode.
func devAuthUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if ok {
			if userIDs := md.Get("x-user-id"); len(userIDs) > 0 {
				ctx = context.WithValue(ctx, userIDKey{}, userIDs[0])
			}
		}
		return handler(ctx, req)
	}
}

// devAuthStreamInterceptor extracts user ID from x-user-id metadata in development mode.
func devAuthStreamInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv any,
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		md, ok := metadata.FromIncomingContext(ss.Context())
		if ok {
			if userIDs := md.Get("x-user-id"); len(userIDs) > 0 {
				ctx := context.WithValue(ss.Context(), userIDKey{}, userIDs[0])
				wrapped := &wrappedServerStream{ServerStream: ss, ctx: ctx}
				return handler(srv, wrapped)
			}
		}
		return handler(srv, ss)
	}
}

// userIDKey is used as a key for storing user ID in context.
type userIDKey struct{}

// getUserIDFromContext extracts user ID from context.
func getUserIDFromContext(ctx context.Context) string {
	userID, ok := ctx.Value(userIDKey{}).(string)
	if !ok {
		return "anonymous"
	}
	return userID
}

// wrappedServerStream wraps grpc.ServerStream with a custom context.
type wrappedServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

// Context returns the wrapped context.
func (w *wrappedServerStream) Context() context.Context {
	return w.ctx
}
