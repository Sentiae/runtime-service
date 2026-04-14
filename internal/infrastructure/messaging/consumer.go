package messaging

import (
	"context"
	"fmt"
	"log"

	kafka "github.com/sentiae/platform-kit/kafka"
)

// EventHandler processes a single inbound CloudEvent.
type EventHandler = kafka.EventHandler

// EventConsumer wraps a platform-kit kafka.Consumer. It subscribes to inbound
// event types (e.g. sentiae.canvas.code.execute_requested) and dispatches
// them to registered handlers.
type EventConsumer struct {
	consumer *kafka.KafkaConsumer
	topics   []string
	groupID  string
}

// NewEventConsumer constructs a consumer that joins the given consumer group
// and subscribes to the listed topics. Handlers are registered separately via
// Handle() before calling Start().
func NewEventConsumer(brokers []string, groupID string, topics []string) (*EventConsumer, error) {
	if len(brokers) == 0 {
		return nil, fmt.Errorf("kafka: at least one broker is required")
	}
	if groupID == "" {
		return nil, fmt.Errorf("kafka: group id is required")
	}
	if len(topics) == 0 {
		return nil, fmt.Errorf("kafka: at least one topic is required")
	}

	c, err := kafka.NewConsumer(kafka.ConsumerConfig{
		Brokers: brokers,
		GroupID: groupID,
		Topics:  topics,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create kafka consumer: %w", err)
	}

	log.Printf("[KAFKA] Consumer created: group=%s, topics=%v", groupID, topics)
	return &EventConsumer{consumer: c, topics: topics, groupID: groupID}, nil
}

// Handle registers a handler for a CloudEvent type.
func (c *EventConsumer) Handle(eventType string, handler EventHandler) {
	if c == nil || c.consumer == nil {
		return
	}
	c.consumer.Subscribe(eventType, handler)
	log.Printf("[KAFKA] Handler registered for event type: %s", eventType)
}

// Start begins consuming messages. It blocks until ctx is cancelled.
func (c *EventConsumer) Start(ctx context.Context) error {
	if c == nil || c.consumer == nil {
		return nil
	}
	return c.consumer.Start(ctx)
}

// Close shuts the consumer down.
func (c *EventConsumer) Close() error {
	if c == nil || c.consumer == nil {
		return nil
	}
	return c.consumer.Close()
}
