package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
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
	testRunHandler      *TestRunHandler
	testGenHandler      *TestGenHandler
	runtimeAgentHandler *RuntimeAgentHandler   // 9.4
	regressionHandler   *RegressionTestHandler // B6 gap #5
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
	testRunRepo *postgres.TestRunRepo,
	testGenUC usecase.TestGenerationUseCase,
	affectedUC usecase.AffectedTestResolver,
) *Server {
	router := chi.NewRouter()

	server := &Server{
		router:             router,
		executionHandler:   NewExecutionHandler(executionUC),
		vmHandler:          NewVMHandler(vmUC),
		snapshotHandler:    NewSnapshotHandler(snapshotUC),
		vmInstanceHandler:  NewVMInstanceHandler(vmInstanceUC, schedulerUC),
		graphHandler:       NewGraphHandler(graphUC),
		graphExecHandler:   NewGraphExecutionHandler(graphEngine),
		graphDebugHandler:  NewGraphDebugHandler(graphDebugUC),
		graphReplayHandler: NewGraphReplayHandler(graphReplayUC),
		terminalHandler:    NewTerminalHandler(terminalUC),
		testRunHandler:     NewTestRunHandler(testRunRepo),
		testGenHandler:     NewTestGenHandler(testGenUC, affectedUC),
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
		s.testRunHandler.RegisterRoutes(r)
		s.testGenHandler.RegisterRoutes(r)
		if s.runtimeAgentHandler != nil {
			s.runtimeAgentHandler.RegisterRoutes(r)
		}
		if s.regressionHandler != nil {
			s.regressionHandler.RegisterRoutes(r)
		}
	})
}

// SetRuntimeAgentHandler installs the 9.4 agent-registry HTTP handler.
// Kept as a setter (not a constructor arg) so the DI container can
// decide whether to enable the feature based on config.
func (s *Server) SetRuntimeAgentHandler(h *RuntimeAgentHandler) {
	if s != nil {
		s.runtimeAgentHandler = h
	}
}

// SetRegressionTestHandler installs the B6 regression-test generator HTTP
// handler. Kept as a setter so the container can omit the feature when
// trace ingestion isn't available.
func (s *Server) SetRegressionTestHandler(h *RegressionTestHandler) {
	if s != nil {
		s.regressionHandler = h
	}
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
	RespondSuccess(w, map[string]any{
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
