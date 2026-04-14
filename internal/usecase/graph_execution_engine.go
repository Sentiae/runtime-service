package usecase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"text/template"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// DefaultMaxParallelism caps how many nodes execute concurrently within
// a single wave. A node graph with hundreds of independent test nodes
// would otherwise spawn one Firecracker VM per node and exhaust the
// host. The DI container can override this via SetMaxParallelism.
const DefaultMaxParallelism = 4

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

// GraphExecutionEngine orchestrates the execution of node graphs using
// topological sorting and wave-based parallel execution.
type GraphExecutionEngine struct {
	graphRepo      repository.GraphDefinitionRepository
	nodeRepo       repository.GraphNodeRepository
	edgeRepo       repository.GraphEdgeRepository
	graphExecRepo  repository.GraphExecutionRepository
	nodeExecRepo   repository.NodeExecutionRepository
	executionUC    ExecutionUseCase
	eventPublisher EventPublisher
	traceRecorder  *GraphTraceRecorder
	httpClient     *http.Client

	maxParallelism int

	mu            sync.Mutex
	cancellations map[uuid.UUID]context.CancelFunc
	timings       map[uuid.UUID][]nodeTimings // per-execution-id
}

// NewGraphExecutionEngine creates a new graph execution engine
func NewGraphExecutionEngine(
	graphRepo repository.GraphDefinitionRepository,
	nodeRepo repository.GraphNodeRepository,
	edgeRepo repository.GraphEdgeRepository,
	graphExecRepo repository.GraphExecutionRepository,
	nodeExecRepo repository.NodeExecutionRepository,
	executionUC ExecutionUseCase,
	eventPublisher EventPublisher,
) *GraphExecutionEngine {
	return &GraphExecutionEngine{
		graphRepo:      graphRepo,
		nodeRepo:       nodeRepo,
		edgeRepo:       edgeRepo,
		graphExecRepo:  graphExecRepo,
		nodeExecRepo:   nodeExecRepo,
		executionUC:    executionUC,
		eventPublisher: eventPublisher,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		maxParallelism: DefaultMaxParallelism,
		cancellations:  make(map[uuid.UUID]context.CancelFunc),
		timings:        make(map[uuid.UUID][]nodeTimings),
	}
}

// SetTraceRecorder sets the trace recorder for execution tracing
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

// ExecuteGraph starts execution of a graph and returns the execution record.
// The actual execution runs asynchronously in a goroutine.
func (e *GraphExecutionEngine) ExecuteGraph(
	ctx context.Context,
	graphID, orgID, requestedBy uuid.UUID,
	input domain.JSONMap,
	debugMode bool,
) (*domain.GraphExecution, error) {
	// Load graph definition
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

	// Start async execution
	execCtx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.cancellations[graphExec.ID] = cancel
	e.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			e.mu.Lock()
			delete(e.cancellations, graphExec.ID)
			e.mu.Unlock()
		}()
		e.runGraph(execCtx, graphExec, graph, nodes, edges, input)
	}()

	return graphExec, nil
}

// ProcessPendingGraphs processes pending graph executions
func (e *GraphExecutionEngine) ProcessPendingGraphs(ctx context.Context, limit int) (int, error) {
	pending, err := e.graphExecRepo.FindPending(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("failed to find pending graph executions: %w", err)
	}

	processed := 0
	for i := range pending {
		exec := &pending[i]
		nodes, err := e.nodeRepo.FindByGraph(ctx, exec.GraphID)
		if err != nil {
			log.Printf("Failed to load nodes for graph execution %s: %v", exec.ID, err)
			continue
		}
		edges, err := e.edgeRepo.FindByGraph(ctx, exec.GraphID)
		if err != nil {
			log.Printf("Failed to load edges for graph execution %s: %v", exec.ID, err)
			continue
		}
		graph, err := e.graphRepo.FindByID(ctx, exec.GraphID)
		if err != nil {
			log.Printf("Failed to load graph for execution %s: %v", exec.ID, err)
			continue
		}

		execCtx, cancel := context.WithCancel(ctx)
		e.mu.Lock()
		e.cancellations[exec.ID] = cancel
		e.mu.Unlock()

		go func(ge *domain.GraphExecution) {
			defer func() {
				cancel()
				e.mu.Lock()
				delete(e.cancellations, ge.ID)
				e.mu.Unlock()
			}()
			e.runGraph(execCtx, ge, graph, nodes, edges, ge.Input)
		}(exec)

		processed++
	}

	return processed, nil
}

// CancelGraphExecution cancels a running graph execution
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

// GetGraphExecution returns a graph execution by ID
func (e *GraphExecutionEngine) GetGraphExecution(ctx context.Context, id uuid.UUID) (*domain.GraphExecution, error) {
	return e.graphExecRepo.FindByID(ctx, id)
}

// ListGraphExecutions returns graph executions for a graph
func (e *GraphExecutionEngine) ListGraphExecutions(ctx context.Context, graphID uuid.UUID, limit, offset int) ([]domain.GraphExecution, int64, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	return e.graphExecRepo.FindByGraph(ctx, graphID, limit, offset)
}

// GetNodeExecution returns a node execution by ID
func (e *GraphExecutionEngine) GetNodeExecution(ctx context.Context, id uuid.UUID) (*domain.NodeExecution, error) {
	return e.nodeExecRepo.FindByID(ctx, id)
}

// ListNodeExecutions returns node executions for a graph execution
func (e *GraphExecutionEngine) ListNodeExecutions(ctx context.Context, graphExecID uuid.UUID) ([]domain.NodeExecution, error) {
	return e.nodeExecRepo.FindByGraphExecution(ctx, graphExecID)
}

// runGraph is the core DAG execution algorithm
func (e *GraphExecutionEngine) runGraph(
	ctx context.Context,
	graphExec *domain.GraphExecution,
	graph *domain.GraphDefinition,
	nodes []domain.GraphNode,
	edges []domain.GraphEdge,
	input domain.JSONMap,
) {
	// Mark as running
	graphExec.MarkRunning()
	if err := e.graphExecRepo.Update(ctx, graphExec); err != nil {
		log.Printf("Failed to mark graph execution %s as running: %v", graphExec.ID, err)
		return
	}
	_ = e.eventPublisher.Publish(ctx, EventGraphExecStarted, graphExec.ID.String(), graphExec)

	// Start trace recording
	var trace *domain.GraphExecutionTrace
	if e.traceRecorder != nil {
		var err error
		trace, err = e.traceRecorder.StartTrace(ctx, graphExec.ID, graph.ID, graphExec.OrganizationID, input)
		if err != nil {
			log.Printf("Warning: failed to start trace for graph execution %s: %v", graphExec.ID, err)
		}
	}

	// Build adjacency and in-degree maps
	inDegree := make(map[uuid.UUID]int, len(nodes))
	adjacency := make(map[uuid.UUID][]uuid.UUID)
	nodeMap := make(map[uuid.UUID]*domain.GraphNode, len(nodes))
	for i := range nodes {
		inDegree[nodes[i].ID] = 0
		nodeMap[nodes[i].ID] = &nodes[i]
	}
	for _, edge := range edges {
		adjacency[edge.SourceNodeID] = append(adjacency[edge.SourceNodeID], edge.TargetNodeID)
		inDegree[edge.TargetNodeID]++
	}

	// Wave-based execution
	outputs := make(map[uuid.UUID]domain.JSONMap)
	remaining := make(map[uuid.UUID]bool, len(nodes))
	for _, n := range nodes {
		remaining[n.ID] = true
	}

	completedNodes := 0
	seqNum := 0
	var execErr error

	for len(remaining) > 0 {
		// Check for context cancellation
		if ctx.Err() != nil {
			execErr = ctx.Err()
			break
		}

		// Find ready nodes (in-degree 0 and still remaining)
		var ready []*domain.GraphNode
		for id := range remaining {
			if inDegree[id] == 0 {
				ready = append(ready, nodeMap[id])
			}
		}

		if len(ready) == 0 {
			execErr = domain.ErrGraphHasCycle
			break
		}

		// Sort for deterministic execution order
		sort.Slice(ready, func(i, j int) bool {
			if ready[i].SortOrder != ready[j].SortOrder {
				return ready[i].SortOrder < ready[j].SortOrder
			}
			return ready[i].Name < ready[j].Name
		})

		// Execute ready nodes in parallel, bounded by maxParallelism.
		// We use a buffered channel as a counting semaphore — every
		// goroutine grabs a slot before doing work and releases it on
		// exit, capping in-flight execution without throwing away
		// fairness or ordering.
		maxPar := e.MaxParallelism()
		if maxPar <= 0 {
			maxPar = DefaultMaxParallelism
		}
		sem := make(chan struct{}, maxPar)

		var wg sync.WaitGroup
		var mu sync.Mutex
		errCh := make(chan error, len(ready))
		laneAssign := func() int {
			// Lane assignment is just the order goroutines acquire
			// the semaphore, modulo maxPar. Used for the
			// parallelism-report visualisation only.
			return int(time.Now().UnixNano()) % maxPar
		}

		for _, node := range ready {
			wg.Add(1)
			go func(n *domain.GraphNode) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()
				lane := laneAssign()

				nodeSeq := func() int {
					mu.Lock()
					defer mu.Unlock()
					seqNum++
					return seqNum
				}()

				// Resolve inputs for this node
				nodeInput := e.resolveNodeInput(n.ID, edges, outputs, input)

				startedAt := time.Now().UTC()

				// Create node execution record
				nodeExec := &domain.NodeExecution{
					ID:               uuid.New(),
					GraphExecutionID: graphExec.ID,
					GraphNodeID:      n.ID,
					NodeType:         n.NodeType,
					NodeName:         n.Name,
					SequenceNumber:   nodeSeq,
					Status:           domain.GraphExecRunning,
					Input:            nodeInput,
					CreatedAt:        time.Now().UTC(),
				}
				nodeExec.MarkRunning()
				if err := e.nodeExecRepo.Create(ctx, nodeExec); err != nil {
					log.Printf("Warning: failed to create node execution record: %v", err)
				}

				_ = e.eventPublisher.Publish(ctx, EventNodeExecStarted, nodeExec.ID.String(), nodeExec)

				// Execute based on node type
				output, execID, err := e.executeNode(ctx, n, nodeInput, graphExec)
				completedAt := time.Now().UTC()

				if err != nil {
					e.recordTiming(graphExec.ID, nodeTimings{
						NodeID:      n.ID,
						NodeName:    n.Name,
						Lane:        lane,
						StartedAt:   startedAt,
						CompletedAt: completedAt,
						DurationMS:  completedAt.Sub(startedAt).Milliseconds(),
					})
					nodeExec.MarkFailed(err.Error())
					nodeExec.ExecutionID = execID
					_ = e.nodeExecRepo.Update(ctx, nodeExec)
					_ = e.eventPublisher.Publish(ctx, EventNodeExecFailed, nodeExec.ID.String(), nodeExec)

					// Record trace
					if trace != nil && e.traceRecorder != nil {
						_ = e.traceRecorder.RecordNode(ctx, trace, n.ID, n.Name, string(n.NodeType), nodeSeq, nodeInput, nil, n.Config, "failed", err.Error(), startedAt, completedAt)
					}

					errCh <- fmt.Errorf("node %q failed: %w", n.Name, err)
					return
				}

				nodeExec.MarkCompleted(output)
				nodeExec.ExecutionID = execID
				_ = e.nodeExecRepo.Update(ctx, nodeExec)
				_ = e.eventPublisher.Publish(ctx, EventNodeExecCompleted, nodeExec.ID.String(), nodeExec)

				// Store output, record trace, and persist timings
				mu.Lock()
				outputs[n.ID] = output
				completedNodes++
				mu.Unlock()
				e.recordTiming(graphExec.ID, nodeTimings{
					NodeID:      n.ID,
					NodeName:    n.Name,
					Lane:        lane,
					StartedAt:   startedAt,
					CompletedAt: completedAt,
					DurationMS:  completedAt.Sub(startedAt).Milliseconds(),
				})

				if trace != nil && e.traceRecorder != nil {
					_ = e.traceRecorder.RecordNode(ctx, trace, n.ID, n.Name, string(n.NodeType), nodeSeq, nodeInput, output, n.Config, "completed", "", startedAt, completedAt)
				}
			}(node)
		}

		wg.Wait()
		close(errCh)

		// Check for errors
		for err := range errCh {
			if execErr == nil {
				execErr = err
			}
		}
		if execErr != nil {
			break
		}

		// Remove completed nodes and update in-degrees
		for _, node := range ready {
			delete(remaining, node.ID)
			for _, next := range adjacency[node.ID] {
				inDegree[next]--
			}
		}
	}

	// Finalize execution
	if execErr != nil {
		graphExec.MarkFailed(execErr.Error(), completedNodes)
		_ = e.graphExecRepo.Update(ctx, graphExec)
		_ = e.eventPublisher.Publish(ctx, EventGraphExecFailed, graphExec.ID.String(), graphExec)
	} else {
		// Collect output from output nodes
		graphOutput := e.collectGraphOutput(nodes, outputs)
		graphExec.MarkCompleted(graphOutput, completedNodes)
		_ = e.graphExecRepo.Update(ctx, graphExec)
		_ = e.eventPublisher.Publish(ctx, EventGraphExecCompleted, graphExec.ID.String(), graphExec)
	}

	// Complete trace
	if trace != nil && e.traceRecorder != nil {
		status := "completed"
		if execErr != nil {
			status = "failed"
		}
		_ = e.traceRecorder.CompleteTrace(ctx, trace, status)
	}

	log.Printf("Graph execution %s finished: status=%s, nodes=%d/%d", graphExec.ID, graphExec.Status, completedNodes, graphExec.TotalNodes)
}

// resolveNodeInput collects inputs for a node from upstream outputs and graph input
func (e *GraphExecutionEngine) resolveNodeInput(
	nodeID uuid.UUID,
	edges []domain.GraphEdge,
	outputs map[uuid.UUID]domain.JSONMap,
	graphInput domain.JSONMap,
) domain.JSONMap {
	result := make(domain.JSONMap)

	// Start with graph-level input as base
	for k, v := range graphInput {
		result[k] = v
	}

	// Overlay outputs from upstream nodes via edges
	for _, edge := range edges {
		if edge.TargetNodeID != nodeID {
			continue
		}
		srcOutput, ok := outputs[edge.SourceNodeID]
		if !ok {
			continue
		}

		// Map source port value to target port
		if val, ok := srcOutput[edge.SourcePort]; ok {
			result[edge.TargetPort] = val
		} else {
			// If source port not found, pass the entire output
			result[edge.TargetPort] = srcOutput
		}
	}

	return result
}

// collectGraphOutput aggregates output from output-type nodes
func (e *GraphExecutionEngine) collectGraphOutput(nodes []domain.GraphNode, outputs map[uuid.UUID]domain.JSONMap) domain.JSONMap {
	result := make(domain.JSONMap)
	for _, node := range nodes {
		if node.NodeType == domain.GraphNodeTypeOutput {
			if out, ok := outputs[node.ID]; ok {
				for k, v := range out {
					result[k] = v
				}
			}
		}
	}
	// If no output nodes, return all outputs from the last nodes
	if len(result) == 0 {
		for _, out := range outputs {
			for k, v := range out {
				result[k] = v
			}
		}
	}
	return result
}

// executeNode dispatches execution to the appropriate handler based on node type
func (e *GraphExecutionEngine) executeNode(
	ctx context.Context,
	node *domain.GraphNode,
	input domain.JSONMap,
	graphExec *domain.GraphExecution,
) (domain.JSONMap, *uuid.UUID, error) {
	switch node.NodeType {
	case domain.GraphNodeTypeCode:
		return e.executeCodeNode(ctx, node, input, graphExec)
	case domain.GraphNodeTypeTransform:
		out, err := e.executeTransformNode(node, input)
		return out, nil, err
	case domain.GraphNodeTypeCondition:
		out, err := e.executeConditionNode(node, input)
		return out, nil, err
	case domain.GraphNodeTypeHTTP:
		out, err := e.executeHTTPNode(ctx, node, input)
		return out, nil, err
	case domain.GraphNodeTypeInput:
		return input, nil, nil
	case domain.GraphNodeTypeOutput:
		return input, nil, nil
	default:
		return nil, nil, fmt.Errorf("unsupported node type: %s", node.NodeType)
	}
}

// executeCodeNode runs code in a Firecracker VM via the existing ExecutionUseCase
func (e *GraphExecutionEngine) executeCodeNode(
	ctx context.Context,
	node *domain.GraphNode,
	input domain.JSONMap,
	graphExec *domain.GraphExecution,
) (domain.JSONMap, *uuid.UUID, error) {
	if node.Language == nil {
		return nil, nil, fmt.Errorf("code node %q missing language", node.Name)
	}

	// Convert input to stdin JSON
	stdinBytes, _ := json.Marshal(input)

	exec, err := e.executionUC.ExecuteSync(ctx, CreateExecutionInput{
		OrganizationID: graphExec.OrganizationID,
		RequestedBy:    graphExec.RequestedBy,
		NodeID:         &node.ID,
		WorkflowID:     &graphExec.GraphID,
		Language:       *node.Language,
		Code:           node.Code,
		Stdin:          string(stdinBytes),
		Args:           input,
		Resources:      &node.Resources,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("code execution failed: %w", err)
	}

	execID := exec.ID
	output := domain.JSONMap{
		"stdout":    exec.Stdout,
		"stderr":    exec.Stderr,
		"exit_code": exec.ExitCode,
		"output":    exec.Stdout,
	}

	if exec.ExitCode != nil && *exec.ExitCode != 0 {
		return output, &execID, fmt.Errorf("code exited with code %d: %s", *exec.ExitCode, exec.Stderr)
	}

	// Try to parse stdout as JSON for richer output
	var parsed domain.JSONMap
	if err := json.Unmarshal([]byte(exec.Stdout), &parsed); err == nil {
		for k, v := range parsed {
			output[k] = v
		}
	}

	return output, &execID, nil
}

// executeTransformNode applies a template transformation to input
func (e *GraphExecutionEngine) executeTransformNode(node *domain.GraphNode, input domain.JSONMap) (domain.JSONMap, error) {
	tmplStr, _ := node.Config["template"].(string)
	if tmplStr == "" {
		// Pass-through if no template
		return input, nil
	}

	tmpl, err := template.New("transform").Parse(tmplStr)
	if err != nil {
		return nil, fmt.Errorf("invalid transform template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, input); err != nil {
		return nil, fmt.Errorf("transform template execution failed: %w", err)
	}

	// Try to parse result as JSON
	var result domain.JSONMap
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		return domain.JSONMap{"output": buf.String()}, nil
	}
	return result, nil
}

// executeConditionNode evaluates a condition and returns which branch to take
func (e *GraphExecutionEngine) executeConditionNode(node *domain.GraphNode, input domain.JSONMap) (domain.JSONMap, error) {
	expr, _ := node.Config["expression"].(string)
	field, _ := node.Config["field"].(string)
	expected, _ := node.Config["value"]

	if field != "" {
		actual, exists := input[field]
		match := exists && fmt.Sprintf("%v", actual) == fmt.Sprintf("%v", expected)
		branch := "false"
		if match {
			branch = "true"
		}
		return domain.JSONMap{"branch": branch, "value": actual}, nil
	}

	if expr != "" {
		// Simple truthy check on the expression field from input
		val, exists := input[expr]
		branch := "false"
		if exists && val != nil && val != false && val != 0 && val != "" {
			branch = "true"
		}
		return domain.JSONMap{"branch": branch, "value": val}, nil
	}

	return domain.JSONMap{"branch": "true"}, nil
}

// executeHTTPNode makes an HTTP request
func (e *GraphExecutionEngine) executeHTTPNode(ctx context.Context, node *domain.GraphNode, input domain.JSONMap) (domain.JSONMap, error) {
	urlStr, _ := node.Config["url"].(string)
	method, _ := node.Config["method"].(string)
	if urlStr == "" {
		return nil, fmt.Errorf("http node %q missing url", node.Name)
	}
	if method == "" {
		method = "GET"
	}

	var bodyReader io.Reader
	if bodyTmpl, ok := node.Config["body"].(string); ok && bodyTmpl != "" {
		bodyReader = bytes.NewBufferString(bodyTmpl)
	}

	req, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if headers, ok := node.Config["headers"].(map[string]any); ok {
		for k, v := range headers {
			req.Header.Set(k, fmt.Sprintf("%v", v))
		}
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read http response: %w", err)
	}

	output := domain.JSONMap{
		"status_code": resp.StatusCode,
		"body":        string(respBody),
	}

	// Try to parse response as JSON
	var parsed domain.JSONMap
	if err := json.Unmarshal(respBody, &parsed); err == nil {
		output["data"] = parsed
	}

	return output, nil
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
	sort.Slice(sortedByEnd, func(i, j int) bool {
		return sortedByEnd[i].CompletedAt.Before(sortedByEnd[j].CompletedAt)
	})
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
	sort.Slice(report.LaneUtilisation, func(i, j int) bool {
		return report.LaneUtilisation[i].Lane < report.LaneUtilisation[j].Lane
	})
	return report
}
