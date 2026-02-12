package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/IBM/sarama"
	"github.com/google/uuid"
)

// EventPublisher defines the interface for publishing domain events
type EventPublisher interface {
	Publish(ctx context.Context, eventType string, key string, data any) error
	Close() error
}

// CloudEvent represents a CloudEvents-compliant event
type CloudEvent struct {
	SpecVersion string `json:"specversion"`
	Type        string `json:"type"`
	Source      string `json:"source"`
	ID          string `json:"id"`
	Time        string `json:"time"`
	Data        any    `json:"data"`
}

// KafkaPublisher implements EventPublisher using Kafka with Sarama
type KafkaPublisher struct {
	producer sarama.SyncProducer
	topic    string
}

// NewKafkaPublisher creates a new Kafka publisher
func NewKafkaPublisher(brokers []string, topic string) (*KafkaPublisher, error) {
	config := sarama.NewConfig()
	config.Producer.Return.Successes = true
	config.Producer.Return.Errors = true
	config.Producer.RequiredAcks = sarama.WaitForLocal
	config.Producer.Retry.Max = 3
	config.Producer.Retry.Backoff = 100 * time.Millisecond
	config.Producer.Timeout = 10 * time.Second
	config.Producer.Compression = sarama.CompressionSnappy

	producer, err := sarama.NewSyncProducer(brokers, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kafka producer: %w", err)
	}

	log.Printf("[KAFKA] Producer connected to brokers: %v, topic: %s", brokers, topic)
	return &KafkaPublisher{
		producer: producer,
		topic:    topic,
	}, nil
}

// Publish publishes a CloudEvents-compliant event to Kafka
func (p *KafkaPublisher) Publish(ctx context.Context, eventType string, key string, data any) error {
	if p.producer == nil {
		return nil
	}

	event := CloudEvent{
		SpecVersion: "1.0",
		Type:        eventType,
		Source:      "runtime-service",
		ID:          uuid.New().String(),
		Time:        time.Now().UTC().Format(time.RFC3339),
		Data:        data,
	}

	eventJSON, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	msg := &sarama.ProducerMessage{
		Topic: p.topic,
		Key:   sarama.StringEncoder(key),
		Value: sarama.ByteEncoder(eventJSON),
		Headers: []sarama.RecordHeader{
			{Key: []byte("content-type"), Value: []byte("application/cloudevents+json")},
			{Key: []byte("ce-type"), Value: []byte(eventType)},
			{Key: []byte("ce-source"), Value: []byte("runtime-service")},
		},
	}

	partition, offset, err := p.producer.SendMessage(msg)
	if err != nil {
		return fmt.Errorf("failed to send message to kafka: %w", err)
	}

	log.Printf("[KAFKA] Event published: type=%s, topic=%s, partition=%d, offset=%d", eventType, p.topic, partition, offset)
	return nil
}

// Close closes the Kafka producer connection
func (p *KafkaPublisher) Close() error {
	if p.producer != nil {
		return p.producer.Close()
	}
	return nil
}

// NoopPublisher is a no-op event publisher for when Kafka is disabled
type NoopPublisher struct{}

// NewNoopPublisher creates a new no-op publisher
func NewNoopPublisher() *NoopPublisher {
	return &NoopPublisher{}
}

// Publish is a no-op
func (p *NoopPublisher) Publish(ctx context.Context, eventType string, key string, data any) error {
	log.Printf("[EVENT-NOOP] Event dropped: type=%s, key=%s", eventType, key)
	return nil
}

// Close is a no-op
func (p *NoopPublisher) Close() error {
	return nil
}
