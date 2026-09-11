package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sentiae/platform-kit/flowlang"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/platform-kit/nodeabi"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// DefaultMaxParallelism caps how many nodes execute concurrently within one
// wave. Every node is a container, so an unbounded wave on a wide graph would
// exhaust the host. The DI container can override this via SetMaxParallelism.
const DefaultMaxParallelism = 4

// flowEnvironments is the closed set a run may resolve secrets from. It is
// closed on purpose: a typo'd environment must refuse, never silently read a
// path that does not exist and hand the node an absent secret.
var flowEnvironments = []string{"dev", "staging", "prod"}

// nodeTimings records the wall-clock window each node ran in. Stored
// per-execution so the parallelism report can compute critical path,
// lane utilisation, and idle gaps. Lives in memory only — durable
// trace data goes via the GraphTraceRecorder.
type nodeTimings struct {
	NodeID         uuid.UUID `json:"node_id"`
	NodeName       string    `json:"node_name"`
	Lane           int       `json:"lane"`
	StartedAt      time.Time `json:"started_at"`
	CompletedAt    time.Time `json:"completed_at"`
	DurationMS     int64     `json:"duration_ms"`
	OnCriticalPath bool      `json:"on_critical_path"`
}

// runCredential is the per-run secret material the engine holds in memory only:
// the handed token and the flow environment its refs are built under. It is
// copied out of the request metadata BEFORE the run goroutine starts and
// dropped at terminal cleanup, so it never outlives the run and never reaches
// a row, a log line or an event.
type runCredential struct {
	token       string
	environment string
}

// GraphExecutionEngine runs a compiled flow: it rebuilds the lowered plan from
// the graph's rows, walks it with flowlang's ONE wave rule, and hands each node
// to the invoker. It interprets nothing itself — a node is a built bundle, and
// the only thing the engine reads out of a node's output is a respond node's
// `response`.
type GraphExecutionEngine struct {
	graphRepo      repository.GraphDefinitionRepository
	nodeRepo       repository.GraphNodeRepository
	edgeRepo       repository.GraphEdgeRepository
	graphExecRepo  repository.GraphExecutionRepository
	nodeExecRepo   repository.NodeExecutionRepository
	eventPublisher EventPublisher
	invoker        *NodeInvoker
	sidecars       SidecarManager
	traceRecorder  *GraphTraceRecorder

	maxParallelism int

	mu             sync.Mutex
	cancellations  map[uuid.UUID]context.CancelFunc
	runCredentials map[uuid.UUID]runCredential
	timings        map[uuid.UUID][]nodeTimings
}

// NewGraphExecutionEngine creates a new graph execution engine.
func NewGraphExecutionEngine(
	graphRepo repository.GraphDefinitionRepository,
	nodeRepo repository.GraphNodeRepository,
	edgeRepo repository.GraphEdgeRepository,
	graphExecRepo repository.GraphExecutionRepository,
	nodeExecRepo repository.NodeExecutionRepository,
	eventPublisher EventPublisher,
	invoker *NodeInvoker,
	sidecars SidecarManager,
) *GraphExecutionEngine {
	// Publish both handed-token revocation series at 0 before the first run can
	// end: an unobserved counter exports no series at all, and "no series" must
	// never be readable as "no failures" (D-7).
	precreateSecretTokenRevocationSeries()
	return &GraphExecutionEngine{
		graphRepo:      graphRepo,
		nodeRepo:       nodeRepo,
		edgeRepo:       edgeRepo,
		graphExecRepo:  graphExecRepo,
		nodeExecRepo:   nodeExecRepo,
		eventPublisher: eventPublisher,
		invoker:        invoker,
		sidecars:       sidecars,
		maxParallelism: DefaultMaxParallelism,
		cancellations:  make(map[uuid.UUID]context.CancelFunc),
		runCredentials: make(map[uuid.UUID]runCredential),
		timings:        make(map[uuid.UUID][]nodeTimings),
	}
}

// SetTraceRecorder sets the trace recorder for execution tracing.
func (e *GraphExecutionEngine) SetTraceRecorder(recorder *GraphTraceRecorder) {
	e.traceRecorder = recorder
}

// SetMaxParallelism overrides the per-wave concurrency cap. Values <= 0
// reset to DefaultMaxParallelism.
func (e *GraphExecutionEngine) SetMaxParallelism(n int) {
	if n <= 0 {
		n = DefaultMaxParallelism
	}
	e.mu.Lock()
	e.maxParallelism = n
	e.mu.Unlock()
}

// MaxParallelism returns the current concurrency cap.
func (e *GraphExecutionEngine) MaxParallelism() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.maxParallelism
}

// ExecuteGraph starts a run and returns its record; the run itself proceeds in
// a background goroutine.
//
// secretToken is the org-scoped token delivery minted for THIS run, and
// environment is the flow environment its secret refs are built under. Both
// arrive as request metadata, are copied into memory here, and are dropped at
// terminal cleanup — the pair is refused up front rather than half-honoured,
// because a run that resolves secrets from the wrong environment reads another
// environment's values instead of failing.
func (e *GraphExecutionEngine) ExecuteGraph(
	ctx context.Context,
	graphID, orgID, requestedBy uuid.UUID,
	input domain.JSONMap,
	debugMode bool,
	secretToken, environment string,
) (*domain.GraphExecution, error) {
	graph, err := e.graphRepo.FindByID(ctx, graphID)
	if err != nil {
		return nil, err
	}
	if graph.Status != domain.GraphStatusActive {
		return nil, domain.ErrGraphNotActive
	}

	nodes, err := e.nodeRepo.FindByGraph(ctx, graphID)
	if err != nil {
		return nil, fmt.Errorf("failed to load graph nodes: %w", err)
	}
	edges, err := e.edgeRepo.FindByGraph(ctx, graphID)
	if err != nil {
		return nil, fmt.Errorf("failed to load graph edges: %w", err)
	}

	plan, byName, err := buildRunPlan(nodes, edges)
	if err != nil {
		usecaseExecutions.WithLabelValues("execute_graph", outcomeInvalid).Inc()
		return nil, err
	}
	if err := checkRunCredentials(nodes, secretToken, environment); err != nil {
		usecaseExecutions.WithLabelValues("execute_graph", outcomeInvalid).Inc()
		return nil, err
	}

	now := time.Now().UTC()
	graphExec := &domain.GraphExecution{
		ID:             uuid.New(),
		GraphID:        graphID,
		OrganizationID: orgID,
		RequestedBy:    requestedBy,
		Status:         domain.GraphExecPending,
		Input:          input,
		TotalNodes:     len(nodes),
		DebugMode:      debugMode,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := e.graphExecRepo.Create(ctx, graphExec); err != nil {
		return nil, fmt.Errorf("failed to create graph execution: %w", err)
	}
	_ = e.eventPublisher.Publish(ctx, EventGraphExecCreated, graphExec.ID.String(), graphExec)

	// The run outlives the RPC that started it, so it detaches from the
	// caller's context.
	e.start(context.Background(), graphExec, graph, plan, byName, input,
		runCredential{token: secretToken, environment: environment})

	usecaseExecutions.WithLabelValues("execute_graph", outcomeOK).Inc()
	return graphExec, nil
}

// ProcessPendingGraphs picks up runs left pending by a previous process. They
// carry no credentials: a handed token is per-request memory and is gone with
// the process that received it, so a pending run of a secret-declaring graph
// fails closed on its first secret rather than resolving one with someone
// else's token.
func (e *GraphExecutionEngine) ProcessPendingGraphs(ctx context.Context, limit int) (int, error) {
	pending, err := e.graphExecRepo.FindPending(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("failed to find pending graph executions: %w", err)
	}

	processed := 0
	for i := range pending {
		exec := &pending[i]
		graph, err := e.graphRepo.FindByID(ctx, exec.GraphID)
		if err != nil {
			logger.FromContext(ctx).Warn("pending run: load graph", "run", exec.ID.String(), "err", err)
			continue
		}
		nodes, err := e.nodeRepo.FindByGraph(ctx, exec.GraphID)
		if err != nil {
			logger.FromContext(ctx).Warn("pending run: load nodes", "run", exec.ID.String(), "err", err)
			continue
		}
		edges, err := e.edgeRepo.FindByGraph(ctx, exec.GraphID)
		if err != nil {
			logger.FromContext(ctx).Warn("pending run: load edges", "run", exec.ID.String(), "err", err)
			continue
		}
		plan, byName, err := buildRunPlan(nodes, edges)
		if err != nil {
			logger.FromContext(ctx).Warn("pending run: rebuild plan", "run", exec.ID.String(), "err", err)
			continue
		}

		e.start(ctx, exec, graph, plan, byName, exec.Input, runCredential{})
		processed++
	}
	return processed, nil
}

// start puts one run on its own goroutine under its own cancellable context,
// with its credentials in memory before the first node can ask for them. It is
// the ONE place a run is launched, so cancellation, panic recovery and
// credential lifetime cannot differ between the two entry points.
func (e *GraphExecutionEngine) start(
	parent context.Context,
	graphExec *domain.GraphExecution,
	graph *domain.GraphDefinition,
	plan *flowlang.Plan,
	byName map[string]*domain.GraphNode,
	input domain.JSONMap,
	cred runCredential,
) {
	execCtx, cancel := context.WithCancel(parent)
	e.mu.Lock()
	e.cancellations[graphExec.ID] = cancel
	e.runCredentials[graphExec.ID] = cred
	e.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.FromContext(execCtx).Error("graph run panicked",
					"run", graphExec.ID.String(), "panic", fmt.Sprint(r))
			}
			cancel()
			e.mu.Lock()
			delete(e.cancellations, graphExec.ID)
			e.mu.Unlock()
		}()
		e.runPlan(execCtx, graphExec, graph, plan, byName, input)
	}()
}

// CancelGraphExecution cancels a running graph execution.
func (e *GraphExecutionEngine) CancelGraphExecution(ctx context.Context, execID uuid.UUID) error {
	e.mu.Lock()
	cancel, ok := e.cancellations[execID]
	e.mu.Unlock()
	if ok {
		cancel()
	}

	exec, err := e.graphExecRepo.FindByID(ctx, execID)
	if err != nil {
		return err
	}
	if exec.IsTerminal() {
		return nil
	}

	exec.MarkCancelled(exec.CompletedNodes)
	if err := e.graphExecRepo.Update(ctx, exec); err != nil {
		return fmt.Errorf("failed to cancel graph execution: %w", err)
	}
	_ = e.eventPublisher.Publish(ctx, EventGraphExecCancelled, execID.String(), exec)
	return nil
}

// GetGraphExecution returns a graph execution by ID.
func (e *GraphExecutionEngine) GetGraphExecution(ctx context.Context, id uuid.UUID) (*domain.GraphExecution, error) {
	return e.graphExecRepo.FindByID(ctx, id)
}

// ListGraphExecutions returns graph executions for a graph.
func (e *GraphExecutionEngine) ListGraphExecutions(ctx context.Context, graphID uuid.UUID, limit, offset int) ([]domain.GraphExecution, int64, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	return e.graphExecRepo.FindByGraph(ctx, graphID, limit, offset)
}

// GetNodeExecution returns a node execution by ID.
func (e *GraphExecutionEngine) GetNodeExecution(ctx context.Context, id uuid.UUID) (*domain.NodeExecution, error) {
	return e.nodeExecRepo.FindByID(ctx, id)
}

// ListNodeExecutions returns node executions for a graph execution.
func (e *GraphExecutionEngine) ListNodeExecutions(ctx context.Context, graphExecID uuid.UUID) ([]domain.NodeExecution, error) {
	return e.nodeExecRepo.FindByGraphExecution(ctx, graphExecID)
}

// buildRunPlan rebuilds the lowered plan from the graph's own rows. The runtime
// never re-schedules a document: sort_order is the carried order the compiler
// lowered, and PlanFromWire refuses if the order it recomputes differs.
func buildRunPlan(nodes []domain.GraphNode, edges []domain.GraphEdge) (*flowlang.Plan, map[string]*domain.GraphNode, error) {
	ordered := make([]*domain.GraphNode, 0, len(nodes))
	for i := range nodes {
		if nodes[i].NodeRef == nil {
			return nil, nil, domain.ErrLegacyGraph
		}
		ordered = append(ordered, &nodes[i])
	}
	slices.SortStableFunc(ordered, func(a, b *domain.GraphNode) int { return a.SortOrder - b.SortOrder })

	byID := make(map[uuid.UUID]*domain.GraphNode, len(nodes))
	byName := make(map[string]*domain.GraphNode, len(nodes))
	wire := make([]flowlang.WireNode, 0, len(ordered))
	for _, n := range ordered {
		byID[n.ID] = n
		byName[n.Name] = n
		wire = append(wire, flowlang.WireNode{
			Slug:     n.Name,
			Pin:      n.NodeRef.Pin(),
			Role:     string(n.Role),
			Required: n.Ports.RequiredInputs(),
			Outputs:  n.Ports.OutputNames(),
		})
	}

	wireEdges := make([]flowlang.Edge, 0, len(edges))
	for _, ed := range edges {
		from, okFrom := byID[ed.SourceNodeID]
		to, okTo := byID[ed.TargetNodeID]
		if !okFrom || !okTo {
			return nil, nil, fmt.Errorf("%w: edge names a node outside this graph", domain.ErrPlanInvalid)
		}
		wireEdges = append(wireEdges, flowlang.Edge{
			From: from.Name, FromPort: ed.SourcePort, To: to.Name, ToPort: ed.TargetPort,
		})
	}

	plan, err := flowlang.PlanFromWire(wire, wireEdges)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", domain.ErrPlanInvalid, err)
	}
	return plan, byName, nil
}

// checkRunCredentials refuses a token/environment pair that does not match what
// the graph declares, BEFORE any node runs.
func checkRunCredentials(nodes []domain.GraphNode, secretToken, environment string) error {
	declares := false
	for i := range nodes {
		if len(nodes[i].Secrets) > 0 {
			declares = true
			break
		}
	}
	switch {
	case declares && secretToken == "":
		return domain.ErrSecretTokenRequired
	case !declares && secretToken != "":
		return domain.ErrSecretTokenUnexpected
	case !declares && environment != "":
		return domain.ErrEnvironmentUnexpected
	case secretToken != "" && environment == "":
		return domain.ErrEnvironmentRequired
	case environment != "" && !slices.Contains(flowEnvironments, environment):
		return domain.ErrEnvironmentInvalid
	}
	return nil
}

// runPlan is the ONE loop. Every wave is flowlang's own rule — the nodes that
// are ready and runnable run, the ready-but-unfed ones are recorded skipped,
// and nothing else decides. The runtime never re-derives an order.
func (e *GraphExecutionEngine) runPlan(
	ctx context.Context,
	graphExec *domain.GraphExecution,
	graph *domain.GraphDefinition,
	plan *flowlang.Plan,
	byName map[string]*domain.GraphNode,
	input domain.JSONMap,
) {
	defer e.cleanupRun(ctx, graphExec.ID)

	graphExec.MarkRunning()
	if err := e.graphExecRepo.Update(ctx, graphExec); err != nil {
		logger.FromContext(ctx).Error("mark run running", "run", graphExec.ID.String(), "err", err)
		return
	}
	_ = e.eventPublisher.Publish(ctx, EventGraphExecStarted, graphExec.ID.String(), graphExec)

	var trace *domain.GraphExecutionTrace
	if e.traceRecorder != nil {
		t, err := e.traceRecorder.StartTrace(ctx, graphExec.ID, graph.ID, graphExec.OrganizationID, input)
		if err != nil {
			logger.FromContext(ctx).Warn("start trace", "run", graphExec.ID.String(), "err", err)
		} else {
			trace = t
		}
	}

	state := &runState{
		status:  map[string]flowlang.Status{},
		fired:   map[string]map[string]bool{},
		outputs: map[string]map[string]json.RawMessage{},
	}

	var runErr error
	for {
		if ctx.Err() != nil {
			runErr = ctx.Err()
			break
		}
		run, skip := plan.Next(state.status, state.fired)
		if len(run) == 0 && len(skip) == 0 {
			break
		}
		for _, slug := range skip {
			e.recordSkip(ctx, graphExec, byName[slug], state.nextSeq())
			state.status[slug] = flowlang.StatusSkipped
		}
		if len(run) == 0 {
			continue
		}
		if err := e.runWave(ctx, graphExec, plan, byName, input, run, state, trace); err != nil {
			runErr = err
			break
		}
	}

	e.finalize(ctx, graphExec, plan, state, runErr, trace)
}

// runState is one run's in-flight bookkeeping, guarded by its own mutex because
// a wave writes it from several goroutines at once.
type runState struct {
	mu      sync.Mutex
	seq     int
	done    int
	status  map[string]flowlang.Status
	fired   map[string]map[string]bool
	outputs map[string]map[string]json.RawMessage
}

func (s *runState) nextSeq() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return s.seq
}

// runWave executes one wave. The FIRST failure cancels the wave: the remaining
// nodes of a broken run are work nobody wants paid for, and their outputs could
// never be used anyway.
func (e *GraphExecutionEngine) runWave(
	ctx context.Context,
	graphExec *domain.GraphExecution,
	plan *flowlang.Plan,
	byName map[string]*domain.GraphNode,
	input domain.JSONMap,
	run []string,
	state *runState,
	trace *domain.GraphExecutionTrace,
) error {
	waveCtx, cancelWave := context.WithCancel(ctx)
	defer cancelWave()

	maxPar := e.MaxParallelism()
	sem := make(chan struct{}, maxPar)
	errCh := make(chan error, len(run))
	var wg sync.WaitGroup

	for _, slug := range run {
		wg.Add(1)
		go func(slug string) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errCh <- fmt.Errorf("node %q failed: crash: %v", slug, r)
					cancelWave()
				}
			}()
			select {
			case sem <- struct{}{}:
			case <-waveCtx.Done():
				return
			}
			defer func() { <-sem }()

			if err := e.runNode(waveCtx, graphExec, plan, byName[slug], slug, input, state, trace); err != nil {
				errCh <- err
				cancelWave()
			}
		}(slug)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		return err
	}
	return nil
}

// runNode resolves one node's inputs, records the attempt, invokes the bundle
// and records the outcome.
func (e *GraphExecutionEngine) runNode(
	ctx context.Context,
	graphExec *domain.GraphExecution,
	plan *flowlang.Plan,
	node *domain.GraphNode,
	slug string,
	input domain.JSONMap,
	state *runState,
	trace *domain.GraphExecutionTrace,
) error {
	if node == nil {
		return fmt.Errorf("node %q failed: %w", slug, domain.ErrGraphNodeNotFound)
	}

	state.mu.Lock()
	inputs, config, err := nodeInvocation(plan, node, slug, state.outputs, state.fired, input)
	state.mu.Unlock()
	if err != nil {
		e.recordFailure(ctx, graphExec, node, state, nil, err, trace, time.Now().UTC())
		return fmt.Errorf("node %q failed: %w", slug, err)
	}

	seq := state.nextSeq()
	startedAt := time.Now().UTC()
	nodeExec := newNodeExecutionRow(graphExec, node, seq, rawToJSONMap(inputs), startedAt)
	nodeExec.MarkRunning()
	if err := e.nodeExecRepo.Create(ctx, nodeExec); err != nil {
		logger.FromContext(ctx).Warn("create node execution", "run", graphExec.ID.String(), "node", slug, "err", err)
	}
	_ = e.eventPublisher.Publish(ctx, EventNodeExecStarted, nodeExec.ID.String(), nodeExec)

	cred := e.credentialFor(graphExec.ID)
	out, invErr := e.invoker.Invoke(ctx, InvokeNodeInput{
		RunID:       graphExec.ID,
		OrgID:       graphExec.OrganizationID,
		Environment: cred.environment,
		Node:        node,
		Inputs:      inputs,
		Config:      config,
		SecretToken: cred.token,
	})
	completedAt := time.Now().UTC()
	e.logNodeLogs(ctx, graphExec.ID, slug, out.Logs)
	e.recordTiming(graphExec.ID, nodeTimings{
		NodeID:      node.ID,
		NodeName:    node.Name,
		StartedAt:   startedAt,
		CompletedAt: completedAt,
		DurationMS:  completedAt.Sub(startedAt).Milliseconds(),
	})

	if invErr != nil {
		nodeExec.MarkFailed(invErr.Error())
		_ = e.nodeExecRepo.Update(ctx, nodeExec)
		_ = e.eventPublisher.Publish(ctx, EventNodeExecFailed, nodeExec.ID.String(), nodeExec)
		state.mu.Lock()
		state.status[slug] = flowlang.StatusFailed
		state.mu.Unlock()
		e.recordTrace(ctx, trace, node, seq, nodeExec.Input, nil, "failed", invErr.Error(), startedAt, completedAt)
		return fmt.Errorf("node %q failed: %w", slug, invErr)
	}

	outputMap := rawToJSONMap(out.Outputs)
	nodeExec.MarkCompleted(outputMap)
	_ = e.nodeExecRepo.Update(ctx, nodeExec)
	_ = e.eventPublisher.Publish(ctx, EventNodeExecCompleted, nodeExec.ID.String(), nodeExec)

	state.mu.Lock()
	state.outputs[slug] = out.Outputs
	state.fired[slug] = out.Fired
	state.status[slug] = flowlang.StatusDone
	state.done++
	state.mu.Unlock()

	e.recordTrace(ctx, trace, node, seq, nodeExec.Input, outputMap, "completed", "", startedAt, completedAt)
	return nil
}

// recordSkip writes the row for a node the plan did not fire. A skip is an
// outcome, not an absence: a run whose rows do not account for every node in
// the plan cannot be reasoned about afterwards.
func (e *GraphExecutionEngine) recordSkip(ctx context.Context, graphExec *domain.GraphExecution, node *domain.GraphNode, seq int) {
	if node == nil {
		return
	}
	now := time.Now().UTC()
	nodeExec := newNodeExecutionRow(graphExec, node, seq, nil, now)
	nodeExec.Status = domain.GraphExecSkipped
	nodeExec.StartedAt, nodeExec.CompletedAt = &now, &now
	if err := e.nodeExecRepo.Create(ctx, nodeExec); err != nil {
		logger.FromContext(ctx).Warn("create skipped node execution",
			"run", graphExec.ID.String(), "node", node.Name, "err", err)
	}
	_ = e.eventPublisher.Publish(ctx, EventNodeExecSkipped, nodeExec.ID.String(), nodeExec)
}

// recordFailure writes the row for a node that failed BEFORE it was invoked —
// a bad trigger request or an unusable config never reaches a sandbox, and the
// run still has to show which node refused.
func (e *GraphExecutionEngine) recordFailure(
	ctx context.Context,
	graphExec *domain.GraphExecution,
	node *domain.GraphNode,
	state *runState,
	inputs map[string]json.RawMessage,
	cause error,
	trace *domain.GraphExecutionTrace,
	at time.Time,
) {
	nodeExec := newNodeExecutionRow(graphExec, node, state.nextSeq(), rawToJSONMap(inputs), at)
	nodeExec.MarkRunning()
	nodeExec.MarkFailed(cause.Error())
	if err := e.nodeExecRepo.Create(ctx, nodeExec); err != nil {
		logger.FromContext(ctx).Warn("create failed node execution",
			"run", graphExec.ID.String(), "node", node.Name, "err", err)
	}
	_ = e.eventPublisher.Publish(ctx, EventNodeExecFailed, nodeExec.ID.String(), nodeExec)
	state.mu.Lock()
	state.status[node.Name] = flowlang.StatusFailed
	state.mu.Unlock()
	e.recordTrace(ctx, trace, node, nodeExec.SequenceNumber, nodeExec.Input, nil, "failed", cause.Error(), at, at)
}

// newNodeExecutionRow is the ONE shape of a node execution row: which run,
// which node, and WHICH BUNDLE it ran — recorded per execution so a finished
// run stays attributable after the graph is re-pinned.
func newNodeExecutionRow(
	graphExec *domain.GraphExecution,
	node *domain.GraphNode,
	seq int,
	input domain.JSONMap,
	at time.Time,
) *domain.NodeExecution {
	return &domain.NodeExecution{
		ID:               uuid.New(),
		GraphExecutionID: graphExec.ID,
		GraphNodeID:      node.ID,
		NodeType:         node.NodeType,
		NodeName:         node.Name,
		NodeRef:          node.NodeRef,
		SequenceNumber:   seq,
		Status:           domain.GraphExecRunning,
		Input:            input,
		CreatedAt:        at,
	}
}

// finalize settles the run: a failure keeps its reason, and a clean run answers
// with whatever the plan says the flow answered.
func (e *GraphExecutionEngine) finalize(
	ctx context.Context,
	graphExec *domain.GraphExecution,
	plan *flowlang.Plan,
	state *runState,
	runErr error,
	trace *domain.GraphExecutionTrace,
) {
	traceStatus := "completed"
	switch {
	case runErr != nil && ctx.Err() != nil:
		graphExec.MarkCancelled(state.done)
		_ = e.graphExecRepo.Update(ctx, graphExec)
		_ = e.eventPublisher.Publish(context.WithoutCancel(ctx), EventGraphExecCancelled, graphExec.ID.String(), graphExec)
		traceStatus = "cancelled"
	case runErr != nil:
		graphExec.MarkFailed(runErr.Error(), state.done)
		_ = e.graphExecRepo.Update(ctx, graphExec)
		_ = e.eventPublisher.Publish(ctx, EventGraphExecFailed, graphExec.ID.String(), graphExec)
		traceStatus = "failed"
	default:
		output, err := planResult(plan, state)
		if err != nil {
			graphExec.MarkFailed(err.Error(), state.done)
			_ = e.graphExecRepo.Update(ctx, graphExec)
			_ = e.eventPublisher.Publish(ctx, EventGraphExecFailed, graphExec.ID.String(), graphExec)
			traceStatus = "failed"
			break
		}
		graphExec.MarkCompleted(output, state.done)
		_ = e.graphExecRepo.Update(ctx, graphExec)
		_ = e.eventPublisher.Publish(ctx, EventGraphExecCompleted, graphExec.ID.String(), graphExec)
	}

	if trace != nil && e.traceRecorder != nil {
		_ = e.traceRecorder.CompleteTrace(context.WithoutCancel(ctx), trace, traceStatus)
	}
	logger.FromContext(ctx).Info("graph run finished",
		"run", graphExec.ID.String(), "status", string(graphExec.Status),
		"nodes", state.done, "total", graphExec.TotalNodes)
}

// planResult is how the flow answered its caller.
func planResult(plan *flowlang.Plan, state *runState) (domain.JSONMap, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	kind, slug := plan.Result(state.fired)
	switch kind {
	case flowlang.ResultFireAndForget:
		return domain.JSONMap{}, nil
	case flowlang.ResultSingleResponse:
		raw, ok := state.outputs[slug]["response"]
		if !ok {
			return nil, domain.ErrNoResponse
		}
		var out domain.JSONMap
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("node %q failed: %s", slug, responseInvalidReason)
		}
		return out, nil
	case flowlang.ResultMultipleResponses:
		return nil, domain.ErrMultipleResponses
	default:
		return nil, domain.ErrNoResponse
	}
}

// cleanupRun is the ONE terminal cleanup: every container, bridge and directory
// this run labelled is swept, and the handed token is revoked whether the run
// succeeded, failed or was cancelled.
func (e *GraphExecutionEngine) cleanupRun(ctx context.Context, runID uuid.UUID) {
	cleanCtx := context.WithoutCancel(ctx)
	if err := e.sidecars.SweepRun(cleanCtx, runID); err != nil {
		logger.FromContext(cleanCtx).Warn("run sweep failed", "run", runID.String(), "err", err)
	}

	e.mu.Lock()
	cred := e.runCredentials[runID]
	delete(e.runCredentials, runID)
	e.mu.Unlock()

	if cred.token == "" {
		return
	}
	// ERROR, not WARN (D-7): a revocation that fails leaves a live credential for
	// this run behind until its TTL expires, which is the one property the handed
	// token exists to bound. The message KEY is unchanged on purpose — §9.6's
	// acceptance greps for `secret_token_revoke_failed` — and the wrapped cause
	// from the secret source is passed through as-is.
	//
	// The run's persisted status is deliberately untouched here: cleanup runs
	// after the terminal transition, and failing a completed run would make the
	// user retry, minting another token that also cannot be revoked.
	if err := e.invoker.RevokeSecretToken(cleanCtx, cred.token); err != nil {
		recordSecretTokenRevocation(revokeOutcomeFailed)
		logger.FromContext(cleanCtx).Error("secret_token_revoke_failed", "run", runID.String(), "err", err)
		return
	}
	recordSecretTokenRevocation(revokeOutcomeOK)
	logger.FromContext(cleanCtx).Info("secret_token_revoked", "run", runID.String())
}

func (e *GraphExecutionEngine) credentialFor(runID uuid.UUID) runCredential {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runCredentials[runID]
}

// logNodeLogs surfaces a node's own log entries as runtime log lines. They are
// deliberately NOT persisted (D-13).
func (e *GraphExecutionEngine) logNodeLogs(ctx context.Context, runID uuid.UUID, slug string, logs []nodeabi.LogEntry) {
	for _, entry := range logs {
		logger.FromContext(ctx).Info("node_log",
			"run", runID.String(), "node", slug, "level", entry.Level, "message", entry.Message)
	}
}

func (e *GraphExecutionEngine) recordTrace(
	ctx context.Context,
	trace *domain.GraphExecutionTrace,
	node *domain.GraphNode,
	seq int,
	input, output domain.JSONMap,
	status, errMsg string,
	startedAt, completedAt time.Time,
) {
	if trace == nil || e.traceRecorder == nil {
		return
	}
	_ = e.traceRecorder.RecordNode(ctx, trace, node.ID, node.Name, string(node.NodeType),
		seq, input, output, node.Config, status, errMsg, startedAt, completedAt)
}

// nodeInvocation resolves what ONE node is called with: the values its fired
// upstream ports produced, and its config with any promoted port folded in.
//
// It is called under the run state's lock, so it reads the shared maps directly
// and returns copies.
func nodeInvocation(
	plan *flowlang.Plan,
	node *domain.GraphNode,
	slug string,
	outputs map[string]map[string]json.RawMessage,
	fired map[string]map[string]bool,
	graphInput domain.JSONMap,
) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	config, err := configObject(node.Config)
	if err != nil {
		return nil, nil, err
	}

	if node.Role == domain.NodeRoleTrigger {
		request, err := triggerRequest(config, graphInput)
		if err != nil {
			return nil, nil, err
		}
		return map[string]json.RawMessage{"request": request}, config, nil
	}

	inputs := map[string]json.RawMessage{}
	for _, edge := range plan.Edges {
		if edge.To != slug || !fired[edge.From][edge.FromPort] {
			continue
		}
		value, ok := outputs[edge.From][edge.FromPort]
		if !ok {
			continue
		}
		// A promoted wire delivers into the node's CONFIG, not into one of its
		// manifest inputs. Lowering already rewrote the target, so the runtime
		// never carries the document's promotion table.
		if key, promoted := flowlang.PromotedKey(edge.ToPort); promoted {
			config[key] = value
			continue
		}
		inputs[edge.ToPort] = value
	}
	return inputs, config, nil
}

// configObject copies a node's stored config into the CALL's shape. The row
// holds a JSON object or nothing at all; anything else is refused rather than
// handed to a bundle that would read it as an empty configuration.
func configObject(config domain.JSONMap) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if len(config) == 0 {
		return out, nil
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrNodeConfigInvalid, err)
	}
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrNodeConfigInvalid, err)
	}
	return out, nil
}

// triggerRequest builds the ONE input a trigger node receives. The run's input
// document is the request itself, never spread across the flow: a trigger that
// silently accepted a different method or path than it was configured for would
// answer a request the flow was never deployed to serve.
func triggerRequest(config map[string]json.RawMessage, input domain.JSONMap) (json.RawMessage, error) {
	method, err := configString(config, "method")
	if err != nil {
		return nil, err
	}
	path, err := configString(config, "path")
	if err != nil {
		return nil, err
	}
	if err := inputMatches(input, "method", method); err != nil {
		return nil, err
	}
	if err := inputMatches(input, "path", path); err != nil {
		return nil, err
	}
	headers, err := inputObject(input, "headers")
	if err != nil {
		return nil, err
	}
	query, err := inputObject(input, "query")
	if err != nil {
		return nil, err
	}
	body, ok := input["body"]
	if !ok || body == nil {
		body = map[string]any{}
	}
	request := map[string]any{
		"method":  method,
		"path":    path,
		"headers": headers,
		"query":   query,
		"body":    body,
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrTriggerInputInvalid, err)
	}
	return encoded, nil
}

func configString(config map[string]json.RawMessage, key string) (string, error) {
	raw, ok := config[key]
	if !ok {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%w: %s", domain.ErrNodeConfigInvalid, key)
	}
	return value, nil
}

func inputMatches(input domain.JSONMap, key, want string) error {
	value, ok := input[key]
	if !ok {
		return nil
	}
	got, isString := value.(string)
	if !isString || got != want {
		return fmt.Errorf("%w: %s", domain.ErrTriggerInputInvalid, key)
	}
	return nil
}

func inputObject(input domain.JSONMap, key string) (map[string]any, error) {
	value, ok := input[key]
	if !ok || value == nil {
		return map[string]any{}, nil
	}
	obj, isObject := value.(map[string]any)
	if !isObject {
		return nil, fmt.Errorf("%w: %s", domain.ErrTriggerInputInvalid, key)
	}
	return obj, nil
}

// rawToJSONMap decodes a node's raw port values for the row that records them.
// A value that cannot be decoded is kept as its literal JSON text rather than
// dropped: the row is evidence of what ran, and a hole in it is worse than a
// string.
func rawToJSONMap(in map[string]json.RawMessage) domain.JSONMap {
	if in == nil {
		return nil
	}
	out := make(domain.JSONMap, len(in))
	for k, raw := range in {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			out[k] = string(raw)
			continue
		}
		out[k] = value
	}
	return out
}

// recordTiming appends a per-node timing record under the execution ID.
// We bound retention to the most recent 10k nodes per execution so
// pathological graphs cannot leak unbounded memory.
func (e *GraphExecutionEngine) recordTiming(execID uuid.UUID, t nodeTimings) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rows := e.timings[execID]
	rows = append(rows, t)
	if len(rows) > 10000 {
		rows = rows[len(rows)-10000:]
	}
	e.timings[execID] = rows
}

// ParallelismReport aggregates the recorded per-node timings into a
// view useful for "why did my pipeline take so long?" debugging.
type ParallelismReport struct {
	ExecutionID        uuid.UUID     `json:"execution_id"`
	MaxParallelism     int           `json:"max_parallelism"`
	TotalNodes         int           `json:"total_nodes"`
	TotalWallClockMS   int64         `json:"total_wall_clock_ms"`
	TotalCPUTimeMS     int64         `json:"total_cpu_time_ms"`
	AverageUtilisation float64       `json:"average_utilisation"`
	CriticalPathMS     int64         `json:"critical_path_ms"`
	LaneUtilisation    []LaneReport  `json:"lane_utilisation"`
	Nodes              []nodeTimings `json:"nodes"`
}

// LaneReport summarises one lane (a virtual worker slot).
type LaneReport struct {
	Lane           int     `json:"lane"`
	NodeCount      int     `json:"node_count"`
	BusyMS         int64   `json:"busy_ms"`
	UtilisationPct float64 `json:"utilisation_pct"`
}

// GetParallelismReport returns aggregated timing data for the given
// execution. Returns an empty report (no error) when no timings have
// been recorded yet — useful for "still running" UIs that want to
// poll without distinguishing not-found from in-progress.
func (e *GraphExecutionEngine) GetParallelismReport(execID uuid.UUID) ParallelismReport {
	e.mu.Lock()
	rows := append([]nodeTimings(nil), e.timings[execID]...)
	maxPar := e.maxParallelism
	e.mu.Unlock()

	report := ParallelismReport{
		ExecutionID:    execID,
		MaxParallelism: maxPar,
		TotalNodes:     len(rows),
		Nodes:          rows,
	}
	if len(rows) == 0 {
		return report
	}

	// Wall-clock = max(completed) - min(started).
	first := rows[0].StartedAt
	last := rows[0].CompletedAt
	for _, r := range rows {
		if r.StartedAt.Before(first) {
			first = r.StartedAt
		}
		if r.CompletedAt.After(last) {
			last = r.CompletedAt
		}
		report.TotalCPUTimeMS += r.DurationMS
	}
	wall := last.Sub(first).Milliseconds()
	if wall < 1 {
		wall = 1
	}
	report.TotalWallClockMS = wall

	// Lane busy time. Lane assignments are heuristic so the per-lane
	// number is informational only — sum across lanes equals total CPU.
	laneBusy := make(map[int]int64)
	laneCount := make(map[int]int)
	for _, r := range rows {
		laneBusy[r.Lane] += r.DurationMS
		laneCount[r.Lane]++
	}
	for lane, busy := range laneBusy {
		report.LaneUtilisation = append(report.LaneUtilisation, LaneReport{
			Lane:           lane,
			NodeCount:      laneCount[lane],
			BusyMS:         busy,
			UtilisationPct: float64(busy) / float64(wall) * 100,
		})
	}
	report.AverageUtilisation = float64(report.TotalCPUTimeMS) / (float64(wall) * float64(maxPar)) * 100

	// Critical path = the longest chronological chain that reaches the
	// last completion. Sort by completed-ascending then walk backwards
	// taking the longest predecessor that finishes before each step.
	sortedByEnd := append([]nodeTimings(nil), rows...)
	slices.SortFunc(sortedByEnd, func(a, b nodeTimings) int { return a.CompletedAt.Compare(b.CompletedAt) })
	type cp struct {
		idx    int
		lenMS  int64
		prevIx int
	}
	dp := make([]cp, len(sortedByEnd))
	bestIdx := 0
	for i, r := range sortedByEnd {
		dp[i] = cp{idx: i, lenMS: r.DurationMS, prevIx: -1}
		for j := 0; j < i; j++ {
			if !sortedByEnd[j].CompletedAt.After(r.StartedAt) {
				if dp[j].lenMS+r.DurationMS > dp[i].lenMS {
					dp[i].lenMS = dp[j].lenMS + r.DurationMS
					dp[i].prevIx = j
				}
			}
		}
		if dp[i].lenMS > dp[bestIdx].lenMS {
			bestIdx = i
		}
	}
	report.CriticalPathMS = dp[bestIdx].lenMS
	// Walk back and tag the chain as on-critical-path on the public list.
	cur := bestIdx
	criticalIDs := map[uuid.UUID]struct{}{}
	for cur >= 0 {
		criticalIDs[sortedByEnd[cur].NodeID] = struct{}{}
		cur = dp[cur].prevIx
	}
	for i := range report.Nodes {
		if _, ok := criticalIDs[report.Nodes[i].NodeID]; ok {
			report.Nodes[i].OnCriticalPath = true
		}
	}
	slices.SortFunc(report.LaneUtilisation, func(a, b LaneReport) int { return a.Lane - b.Lane })
	return report
}
