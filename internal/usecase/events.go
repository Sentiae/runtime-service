package usecase

import "context"

// EventPublisher defines the interface for publishing domain events.
// Implementations include Kafka publisher and no-op publisher.
type EventPublisher interface {
	Publish(ctx context.Context, eventType string, key string, data any) error
	Close() error
}

// -----------------------------------------------------------------------
// Runtime-service outbound event types (published to Kafka)
// -----------------------------------------------------------------------

const (
	// Graph lifecycle events
	EventGraphCreated  = "sentiae.runtime.graph.created"
	EventGraphUpdated  = "sentiae.runtime.graph.updated"
	EventGraphDeployed = "sentiae.runtime.graph.deployed"
	EventGraphDeleted  = "sentiae.runtime.graph.deleted"

	// Graph execution events
	EventGraphExecCreated   = "sentiae.runtime.graph.execution.created"
	EventGraphExecStarted   = "sentiae.runtime.graph.execution.started"
	EventGraphExecCompleted = "sentiae.runtime.graph.execution.completed"
	EventGraphExecFailed    = "sentiae.runtime.graph.execution.failed"
	EventGraphExecCancelled = "sentiae.runtime.graph.execution.cancelled"

	// Node execution events (within a graph)
	EventNodeExecStarted   = "sentiae.runtime.node.execution.started"
	EventNodeExecCompleted = "sentiae.runtime.node.execution.completed"
	EventNodeExecFailed    = "sentiae.runtime.node.execution.failed"

	// Debug events
	EventGraphDebugCreated   = "sentiae.runtime.graph.debug.created"
	EventGraphDebugStarted   = "sentiae.runtime.graph.debug.started"
	EventGraphDebugPaused    = "sentiae.runtime.graph.debug.paused"
	EventGraphDebugCompleted = "sentiae.runtime.graph.debug.completed"
	EventGraphDebugCancelled = "sentiae.runtime.graph.debug.cancelled"
	EventGraphDebugStep      = "sentiae.runtime.graph.debug.step"

	// Single execution events
	EventExecCreated   = "sentiae.runtime.execution.created"
	EventExecCompleted = "sentiae.runtime.execution.completed"
	EventExecFailed    = "sentiae.runtime.execution.failed"
)
