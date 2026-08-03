package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	kafka "github.com/sentiae/platform-kit/kafka"
	"github.com/sentiae/platform-kit/opshttp"
	"github.com/sentiae/platform-kit/posture"
	"github.com/sentiae/runtime-service/internal/infrastructure/canvasservice"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
	"github.com/sentiae/runtime-service/internal/usecase"
	"github.com/sentiae/runtime-service/internal/version"
)

// Server represents the HTTP server with all handlers
type Server struct {
	router               chi.Router
	executionHandler     *ExecutionHandler
	vmHandler            *VMHandler
	snapshotHandler      *SnapshotHandler
	vmInstanceHandler    *VMInstanceHandler
	graphHandler         *GraphHandler
	graphExecHandler     *GraphExecutionHandler
	graphDebugHandler    *GraphDebugHandler
	graphReplayHandler   *GraphReplayHandler
	terminalHandler      *TerminalHandler
	testRunHandler       *TestRunHandler
	testGenHandler       *TestGenHandler
	runtimeAgentHandler  *RuntimeAgentHandler      // 9.4
	regressionHandler    *RegressionTestHandler    // B6 gap #5
	dslHandler           *DSLHandler               // §19 DSL execute
	hermeticBuildHandler *HermeticBuildHandler     // §9.2 hermetic builds
	quarantineHandler    *TestQuarantineHandler    // §8.3 quarantine toggles
	customerAgentHandler *CustomerAgentCertHandler // §9.4 agent enrolment
	fleetHandler         *FleetHandler             // warm-VM fleet visibility + control
	activatorHandler     *ActivatorHandler         // rt#11 scale-to-zero wake endpoint
	permissionChecker    PermissionChecker
	postureSet           *posture.Set             // D-179 Wave-8 — backs /posture
	consumers            []*kafka.KafkaConsumer    // D-179 Wave-8 — backs /healthz/consumers
}

// SetOpsSurface wires the Wave-8 uniform ops endpoints (/posture,
// /healthz/consumers). Called by the DI container BEFORE SetupRoutes, alongside
// the other Set*Handler setters.
func (s *Server) SetOpsSurface(postureSet *posture.Set, consumers ...*kafka.KafkaConsumer) {
	s.postureSet = postureSet
	s.consumers = consumers
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
		vmHandler:          NewVMHandler(vmUC, nil),
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
	// setupRoutes runs via SetupRoutes() called by the DI container
	// AFTER every Set*Handler setter has fired. Without that order,
	// deferred handlers would all be nil at route-registration time.

	return server
}

// SetupRoutes registers every route on the router. MUST be called
// exactly once by the DI container AFTER all Set*Handler setters have
// fired. Mirrors foundry-service's SetupRoutes pattern.
func (s *Server) SetupRoutes() { s.setupRoutes() }

// setupRoutes registers all routes
func (s *Server) setupRoutes() {
	// Health check
	s.router.Get("/health", s.healthCheck)
	s.router.Get("/api/v1/health", s.healthCheck)

	// Wave-8 uniform ops surface (D-179). /posture reports the declared
	// security controls + their boot result; /healthz/consumers surfaces
	// per-topic Kafka lag + DLQ activity for runtime's inbound consumer.
	// /health above is runtime's own liveness and stays untouched.
	s.router.Handle("/posture", opshttp.PostureHandler("runtime", s.postureSet))
	s.router.Handle("/healthz/consumers", kafka.ConsumersHealthzHandler("runtime", s.consumers...))

	// Prometheus scrape surface (§22), matching identity/git/conversation/vigil:
	// promhttp over the DEFAULT registry, which is the same registry otelkit.Init
	// bridges into the OTLP push path. Both paths matter here — the bare Firecracker
	// fleet host runs with APP_TELEMETRY_ENABLED=false (no reachable collector), so
	// without this route the durability gauges on the host that actually holds
	// customer data would be unreadable.
	s.router.Handle("/metrics", promhttp.Handler())

	// §9.4 — customer-agent enrolment endpoint. Lives OUTSIDE the
	// /api/v1 auth-protected group because the customer-agent has
	// no JWT yet at enrolment time; its sole protection is the
	// enrolment bearer token validated inside the handler.
	if s.customerAgentHandler != nil {
		s.customerAgentHandler.RegisterRoutes(s.router)
	}

	// Warm-VM fleet surface. Registered OUTSIDE the /api/v1 JWT group: it is the
	// per-host data source the control plane (deployment-service Fleet RPC) polls
	// over the internal network, which presents the shared service token, not a
	// runtime JWT + X-User-ID. The handler guards a nil pool (warm pool disabled
	// ⇒ GET /fleet reports {enabled:false}).
	if s.fleetHandler != nil {
		s.fleetHandler.RegisterRoutes(s.router)
	}

	// rt#11 scale-to-zero wake endpoint. Registered OUTSIDE the /api/v1 JWT group:
	// its sole caller is the co-located Caddy gateway on loopback (zero-upstream
	// route → reverse_proxy to :8090/_activate). Only mounted on the firecracker
	// host where the orchestrator is wired.
	if s.activatorHandler != nil {
		s.activatorHandler.RegisterRoutes(s.router)
	}

	// API routes with authentication
	s.router.Route("/api/v1", func(r chi.Router) {
		r.Use(AuthMiddleware)

		// §B1 fail-closed: if permissionChecker was never wired (dev
		// misconfig), default to deny-all so routes that opt in to the
		// permission gate still refuse traffic. Operators can opt-in
		// explicitly to dev allow-all via APP_PERMISSION_ALLOW_ALL=true
		// (wired in container.go — DI must populate s.permissionChecker
		// via MustPermissionChecker).
		checker := s.permissionChecker
		if checker == nil {
			checker = denyAllPermissionChecker{}
		}

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
		if s.dslHandler != nil {
			s.dslHandler.RegisterRoutes(r)
		}
		if s.hermeticBuildHandler != nil {
			// Hermetic-build MUTATIONS require write permission on the build. The
			// gates are handed to the handler so they wrap the routes themselves —
			// this used to be a middleware on an empty chi.Group next to routes
			// registered outside it, i.e. no check at all.
			//
			// ⚠ With no PermissionChecker wired (the default — see
			// MustPermissionChecker) `checker` is deny-all, so these two routes
			// REFUSE every request. That is the intended fail-closed posture and it
			// breaks nothing: no caller of /hermetic-builds exists in the fleet.
			s.hermeticBuildHandler.RegisterRoutes(r,
				RequireRuntimePermission(checker, "hermetic_build", "write", "id"),
				RequireRuntimePermissionFromBody(checker, "hermetic_build", "write", "build_id"),
			)
		}
		if s.quarantineHandler != nil {
			s.quarantineHandler.RegisterRoutes(r)
		}
	})
}

// SetPermissionChecker wires the permission client used by the runtime
// resource permission middleware. Defaults to AllowAllPermissionChecker
// when left unset — production callers MUST supply a real checker.
func (s *Server) SetPermissionChecker(c PermissionChecker) {
	if s != nil {
		s.permissionChecker = c
	}
}

// SetTestRunDispatcher installs the §8.4 multi-type dispatcher on the
// test-run handler. The handler already owns its dispatcher slot; this
// setter forwards the typed pointer so the container keeps a single
// source of truth for test-run routing.
func (s *Server) SetTestRunDispatcher(d TestRunDispatcher) {
	if s == nil || s.testRunHandler == nil {
		return
	}
	s.testRunHandler.WithDispatcher(d)
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

// SetDSLHandler installs the §19 DSL execute handler.
func (s *Server) SetDSLHandler(h *DSLHandler) {
	if s != nil && h != nil {
		s.dslHandler = h
	}
}

// SetHermeticBuildHandler installs the §9.2 hermetic-build handler.
func (s *Server) SetHermeticBuildHandler(h *HermeticBuildHandler) {
	if s != nil && h != nil {
		s.hermeticBuildHandler = h
	}
}

// SetTestQuarantineHandler installs the §8.3 manual quarantine toggle.
func (s *Server) SetTestQuarantineHandler(h *TestQuarantineHandler) {
	if s != nil && h != nil {
		s.quarantineHandler = h
	}
}

// SetCustomerAgentCertHandler installs the §9.4 customer-agent
// enrolment endpoint. Left unset in dev deployments that don't run
// customer-hosted Firecracker agents.
func (s *Server) SetCustomerAgentCertHandler(h *CustomerAgentCertHandler) {
	if s != nil && h != nil {
		s.customerAgentHandler = h
	}
}

// SetFleetHandler installs the warm-VM fleet handler. Kept as a setter so the
// DI container can pass the live *WarmPool (which may be nil when the warm pool
// is disabled — the handler then reports the fleet as disabled rather than
// 500ing). Always call it; the handler owns the nil-pool guard.
func (s *Server) SetFleetHandler(h *FleetHandler) {
	if s != nil && h != nil {
		s.fleetHandler = h
	}
}

// SetActivatorHandler installs the rt#11 scale-to-zero wake handler. Kept as a
// setter so the DI container mounts it only on the firecracker host (where the
// orchestrator + activator are wired); left nil elsewhere.
func (s *Server) SetActivatorHandler(h *ActivatorHandler) {
	if s != nil && h != nil {
		s.activatorHandler = h
	}
}

// SetCheckpointScheduler wires the checkpoint scheduler into the VM
// handler so POST /vms/{id}/pause can snapshot on demand (§9.3).
func (s *Server) SetCheckpointScheduler(cp *usecase.CheckpointScheduler) {
	if s == nil || s.vmHandler == nil || cp == nil {
		return
	}
	s.vmHandler.checkpoints = cp
}

// SetTestRunEventPublisher wires the Kafka publisher into the test-run
// handler so terminal status transitions emit
// `sentiae.runtime.test.completed` CloudEvents.
func (s *Server) SetTestRunEventPublisher(pub usecase.EventPublisher) {
	if s == nil || s.testRunHandler == nil {
		return
	}
	s.testRunHandler.SetEventPublisher(pub)
}

// SetTestRunCanvasClient wires the canvas-service HTTP client so
// terminal test transitions push inline badges to canvas-service in
// addition to the durable Kafka event (§19.1 flow 1E).
func (s *Server) SetTestRunCanvasClient(c *canvasservice.Client) {
	if s == nil || s.testRunHandler == nil {
		return
	}
	s.testRunHandler.SetCanvasClient(c)
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
		// Deploy provenance: the commit this image was built from, identical to
		// the image's org.opencontainers.image.revision label (same build arg).
		"vcs.revision": version.Revision,
		"vcs.modified": version.Modified,
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
