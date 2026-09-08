package domain

import "errors"

// Common domain errors
var (
	// Generic errors
	ErrInvalidID   = errors.New("invalid ID")
	ErrInvalidData = errors.New("invalid data")

	// Execution errors
	ErrExecutionNotFound     = errors.New("execution not found")
	ErrExecutionAlreadyDone  = errors.New("execution already in terminal state")
	ErrInvalidLanguage       = errors.New("unsupported programming language")
	ErrEmptyCode             = errors.New("code cannot be empty")
	ErrResourceLimitExceeded = errors.New("resource limit exceeded")
	ErrTimeoutExceeded       = errors.New("execution timeout exceeded")

	// MicroVM errors
	ErrVMNotFound          = errors.New("microVM not found")
	ErrVMNotReady          = errors.New("microVM is not in ready state")
	ErrVMAlreadyTerminated = errors.New("microVM is already terminated")
	ErrVMPoolExhausted     = errors.New("no available microVMs in pool")
	ErrVMStartFailed       = errors.New("microVM failed to start")

	// Snapshot errors
	ErrSnapshotNotFound    = errors.New("snapshot not found")
	ErrSnapshotCreateFail  = errors.New("failed to create snapshot")
	ErrSnapshotRestoreFail = errors.New("failed to restore snapshot")

	// Resource errors
	ErrInvalidVCPU        = errors.New("invalid vCPU count")
	ErrInvalidMemory      = errors.New("invalid memory size")
	ErrInvalidTimeout     = errors.New("invalid timeout duration")
	ErrInvalidNetworkMode = errors.New("invalid network mode")

	// VM Instance errors
	ErrVMInstanceNotFound = errors.New("VM instance not found")
	ErrInvalidVMState     = errors.New("invalid VM instance state")

	// Scheduler errors
	ErrNoHostAvailable = errors.New("no host available with sufficient resources")
	ErrHostNotFound    = errors.New("host not found")

	// Graph errors
	ErrGraphNotFound          = errors.New("graph definition not found")
	ErrGraphNotActive         = errors.New("graph is not active")
	ErrGraphHasCycle          = errors.New("graph contains a cycle")
	ErrGraphNodeNotFound      = errors.New("graph node not found")
	ErrGraphEdgeNotFound      = errors.New("graph edge not found")
	ErrGraphExecutionNotFound = errors.New("graph execution not found")
	ErrNodeExecutionNotFound  = errors.New("node execution not found")

	// Debug errors
	ErrDebugSessionNotFound  = errors.New("debug session not found")
	ErrDebugSessionNotPaused = errors.New("debug session is not paused")

	// Trace/replay errors
	ErrTraceNotFound          = errors.New("execution trace not found")
	ErrTracesNotComparable    = errors.New("traces must be from the same graph")
	ErrReplaySessionNotFound  = errors.New("replay session not found")
	ErrReplayIndexOutOfBounds = errors.New("replay index out of bounds")

	// Terminal session errors
	ErrTerminalSessionNotFound = errors.New("terminal session not found")
	ErrTerminalSessionClosed   = errors.New("terminal session is already closed")
	ErrTerminalVMNotReady      = errors.New("terminal VM is not ready")

	// Phase 4 — a graph node IS a built bundle (D-9). These are the refusals of
	// the interpreter-shaped inputs the bundle model retires, and of the
	// node-runner faults that must fail closed rather than run something
	// unpinned.
	ErrLegacyGraph           = errors.New("graph has nodes without node_ref (legacy interpreter graph)")
	ErrNodeRefRequired       = errors.New("node_ref is required")
	ErrLegacyNodeInput       = errors.New("node_type, language and code are retired; supply node_ref")
	ErrInvalidNodeRef        = errors.New("invalid node_ref")
	ErrInvalidPortSpec       = errors.New("invalid port spec")
	ErrInvalidSecretSpec     = errors.New("invalid secret spec")
	ErrInvalidRole           = errors.New("role must be \"\", trigger or respond")
	ErrInvalidEgressPattern  = errors.New("invalid egress pattern")
	ErrPlanInvalid           = errors.New("execution plan is invalid")
	ErrSeededOutputsRetired  = errors.New("seeded_outputs is retired (T-RUN-PARTIAL-RERUN)")
	ErrSecretTokenRequired   = errors.New("graph declares secrets but no secret token was handed (x-sentiae-secret-token)")
	ErrSecretTokenUnexpected = errors.New("secret token handed to a graph that declares no secrets")
	// The flow-run environment travels beside the handed token on
	// x-sentiae-flow-environment (R-18). It is NOT app.environment: that one
	// names where this RUNTIME is deployed, while this one names which of the
	// org's environments the run resolves its secrets from, so a wrong value
	// would read another environment's secret rather than fail.
	ErrEnvironmentRequired   = errors.New("secret token handed without a flow environment (x-sentiae-flow-environment)")
	ErrEnvironmentInvalid    = errors.New("flow environment must be dev, preview or prod (x-sentiae-flow-environment)")
	ErrEnvironmentUnexpected = errors.New("flow environment handed to a graph that declares no secrets")
	ErrRequiredSecretAbsent  = errors.New("required secret absent")
	ErrTriggerInputInvalid   = errors.New("trigger input is not a request object")
	ErrNodeConfigInvalid     = errors.New("node config is not a JSON object")
	ErrNodeFailed            = errors.New("node failed")
	ErrNoResponse            = errors.New("no_response")
	ErrMultipleResponses     = errors.New("multiple_responses")
	ErrGraphDebugRetired     = errors.New("graph debug sessions are retired (T-RUN-DEBUG-SESSIONS-REBUILD)")
	ErrBundlePullFailed      = errors.New("bundle image pull failed")
	ErrNodeRunnerNotReady    = errors.New("node runner is not configured")
	ErrNodeRunnerBusy        = errors.New("node runner has no free invocation subnet")

	// Egress audit drain refusals (D-395). Blind: the sidecar's log carries no
	// sidecar_bound anchor, so nothing proves it was readable. Gap: the decision
	// sequence has a hole or a repeat. Malformed: a decision line that does not
	// parse as one.
	ErrEgressAuditBlind     = errors.New("egress audit: sidecar log has no anchor line")
	ErrEgressAuditGap       = errors.New("egress audit: decision sequence is incomplete")
	ErrEgressAuditMalformed = errors.New("egress audit: malformed sidecar log line")

	// §9.2 hermetic chain integrity. ErrHashMismatch is returned when
	// a step's verified prior-artifact digest does not match what the
	// store rehydrates — indicates corruption or tampering.
	ErrHashMismatch = errors.New("artifact: hash mismatch between step and store")
)
