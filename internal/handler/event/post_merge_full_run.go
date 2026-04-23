// Package event — PostMergeFullRun is the thin handler-layer wrapper
// around the use-case that kicks off a full-suite test run on every
// post-merge CloudEvent (§B36).
//
// The actual fan-out logic lives in usecase.PostMergeFullRunTrigger
// because it needs repository + event-publisher dependencies that are
// already composed there. This file only exposes the event-handler
// surface so the Kafka consumer registration stays alongside the other
// event handlers.
package event

import (
	"context"

	kafka "github.com/sentiae/platform-kit/kafka"

	"github.com/sentiae/runtime-service/internal/usecase"
)

// PostMergeFullRunHandler wraps *usecase.PostMergeFullRunTrigger with a
// tiny Register helper so a Kafka consumer can subscribe to every
// supported event alias in one call.
type PostMergeFullRunHandler struct {
	trigger *usecase.PostMergeFullRunTrigger
}

// NewPostMergeFullRunHandler builds the wrapper. A nil trigger is
// tolerated: the resulting handler is a safe no-op so mis-wired
// deployments don't crash the Kafka consumer boot path.
func NewPostMergeFullRunHandler(trigger *usecase.PostMergeFullRunTrigger) *PostMergeFullRunHandler {
	return &PostMergeFullRunHandler{trigger: trigger}
}

// Handle dispatches the CloudEvent to the underlying trigger. Exposed
// so the consumer can wire one handler per event alias without calling
// Register.
func (h *PostMergeFullRunHandler) Handle(ctx context.Context, event kafka.CloudEvent) error {
	if h == nil || h.trigger == nil {
		return nil
	}
	return h.trigger.HandleGitEvent(ctx, event)
}

// Register subscribes the handler to every event alias the trigger
// understands (sentiae.git.pr.merged + sentiae.git.merge.completed and
// their bare equivalents). Safe to call with a nil consumer.
func (h *PostMergeFullRunHandler) Register(consumer interface {
	Handle(eventType string, handler kafka.EventHandler)
}) {
	if h == nil || h.trigger == nil || consumer == nil {
		return
	}
	for _, t := range usecase.FullRunEventTypes {
		consumer.Handle(t, h.Handle)
	}
}
