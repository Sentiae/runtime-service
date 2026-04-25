package messaging

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	kafka "github.com/sentiae/platform-kit/kafka"
)

// EventPublisher defines the interface for publishing domain events
type EventPublisher interface {
	Publish(ctx context.Context, eventType string, key string, data any) error
	// EnsureTopics pre-creates the registered Kafka topic set on boot.
	EnsureTopics(ctx context.Context) error
	Close() error
}

// platformKitAdapter wraps a platform-kit kafka.Publisher as an EventPublisher.
type platformKitAdapter struct {
	pub kafka.Publisher
}

// NewKafkaPublisher creates a new EventPublisher backed by platform-kit/kafka.
func NewKafkaPublisher(cfg kafka.PublisherConfig) (EventPublisher, error) {
	pub, err := kafka.NewPublisher(cfg)
	if err != nil {
		return nil, err
	}
	return &platformKitAdapter{pub: pub}, nil
}

func (a *platformKitAdapter) Publish(ctx context.Context, eventType, key string, data any) error {
	md, err := toMetadataMap(data)
	if err != nil {
		return err
	}
	et := eventType
	if strings.HasPrefix(et, "sentiae.") {
		et = et[len("sentiae."):]
	}
	return a.pub.Publish(ctx, et, kafka.EventData{
		ResourceID:   key,
		ResourceType: resourceTypeFromEvent(et),
		Metadata:     md,
	})
}

func (a *platformKitAdapter) EnsureTopics(ctx context.Context) error {
	return a.pub.EnsureTopics(ctx)
}

func (a *platformKitAdapter) Close() error {
	return a.pub.Close()
}

// NoopPublisher is a no-op event publisher for when Kafka is disabled
type NoopPublisher struct{}

// NewNoopPublisher creates a new no-op publisher
func NewNoopPublisher() *NoopPublisher {
	return &NoopPublisher{}
}

// Publish is a no-op
func (p *NoopPublisher) Publish(_ context.Context, eventType, key string, _ any) error {
	log.Printf("[EVENT-NOOP] Event dropped: type=%s, key=%s", eventType, key)
	return nil
}

// EnsureTopics is a no-op
func (p *NoopPublisher) EnsureTopics(_ context.Context) error {
	return nil
}

// Close is a no-op
func (p *NoopPublisher) Close() error {
	return nil
}

func toMetadataMap(data any) (map[string]any, error) {
	if data == nil {
		return nil, nil
	}
	if m, ok := data.(map[string]any); ok {
		return m, nil
	}
	b, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return m, nil
}

func resourceTypeFromEvent(eventType string) string {
	parts := strings.Split(eventType, ".")
	if len(parts) >= 2 {
		return parts[1]
	}
	return "unknown"
}
