package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// Server represents the HTTP server with all handlers
type Server struct {
	router              chi.Router
	executionHandler    *ExecutionHandler
	vmHandler           *VMHandler
	snapshotHandler     *SnapshotHandler
	vmInstanceHandler   *VMInstanceHandler
	graphHandler        *GraphHandler
	graphExecHandler    *GraphExecutionHandler
	graphDebugHandler   *GraphDebugHandler
	graphReplayHandler  *GraphReplayHandler
	terminalHandler     *TerminalHandler
}

// NewServer creates a new HTTP server with all handlers
func NewServer(
	executionUC usecase.ExecutionUseCase,
	vmUC usecase.VMUseCase,
	snapshotUC usecase.SnapshotUseCase,
	vmInstanceUC usecase.VMInstanceUseCase,
	schedulerUC usecase.SchedulerUseCase,
	graphUC usecase.GraphUseCase,
	graphEngine *usecase.GraphExecutionEngine,
	graphDebugUC usecase.GraphDebugUseCase,
	graphReplayUC usecase.GraphReplayUseCase,
	terminalUC usecase.TerminalUseCase,
) *Server {
	router := chi.NewRouter()

	server := &Server{
		router:            router,
		executionHandler:  NewExecutionHandler(executionUC),
		vmHandler:         NewVMHandler(vmUC),
		snapshotHandler:   NewSnapshotHandler(snapshotUC),
		vmInstanceHandler: NewVMInstanceHandler(vmInstanceUC, schedulerUC),
		graphHandler:      NewGraphHandler(graphUC),
		graphExecHandler:  NewGraphExecutionHandler(graphEngine),
		graphDebugHandler:  NewGraphDebugHandler(graphDebugUC),
		graphReplayHandler: NewGraphReplayHandler(graphReplayUC),
		terminalHandler:    NewTerminalHandler(terminalUC),
	}

	server.setupMiddleware()
	server.setupRoutes()

	return server
}

// setupRoutes registers all routes
func (s *Server) setupRoutes() {
	// Health check
	s.router.Get("/health", s.healthCheck)
	s.router.Get("/api/v1/health", s.healthCheck)

	// API routes with authentication
	s.router.Route("/api/v1", func(r chi.Router) {
		r.Use(AuthMiddleware)

		s.executionHandler.RegisterRoutes(r)
		s.vmHandler.RegisterRoutes(r)
		s.snapshotHandler.RegisterRoutes(r)
		s.vmInstanceHandler.RegisterRoutes(r)
		s.graphHandler.RegisterRoutes(r)
		s.graphExecHandler.RegisterRoutes(r)
		s.graphDebugHandler.RegisterRoutes(r)
		s.graphReplayHandler.RegisterRoutes(r)
		s.terminalHandler.RegisterRoutes(r)
	})
}

// setupMiddleware sets up global middleware
func (s *Server) setupMiddleware() {
	s.router.Use(RecoveryMiddleware)
	s.router.Use(RequestIDMiddleware)
	s.router.Use(LoggingMiddleware)
	s.router.Use(CORSMiddleware)
	s.router.Use(ContentTypeMiddleware)
}

// healthCheck handles health check requests
func (s *Server) healthCheck(w http.ResponseWriter, r *http.Request) {
	RespondSuccess(w, map[string]interface{}{
		"status":  "healthy",
		"service": "runtime-service",
		"version": "1.0.0",
	})
}

// GetRouter returns the chi router
func (s *Server) GetRouter() chi.Router {
	return s.router
}

// ServeHTTP implements http.Handler
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}
