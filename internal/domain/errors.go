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
)
