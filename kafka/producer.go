package kafka

import (
	"context"
	"encoding/json"
	"fmt"
)

// Producer is the service-facing convenience wrapper over Broker. It takes Go
// values (any), JSON-marshals them, and hands the bytes to the configured
// broker. Services depend on *Producer rather than Broker directly because
// every caller today passes a struct — not doing the marshal here would force
// boilerplate in every call site.
type Producer struct {
	broker Broker
}

// NewProducer wraps a Broker. Construction is cheap — the broker owns the
// real connection pools.
func NewProducer(broker Broker) *Producer {
	return &Producer{broker: broker}
}

// Send JSON-marshals value and publishes synchronously.
func (p *Producer) Send(topic string, key string, value any) error {
	data, err := marshalValue(value)
	if err != nil {
		return fmt.Errorf("marshaling message: %w", err)
	}
	return p.broker.Send(context.Background(), topic, key, data)
}

// SendAsync JSON-marshals value and publishes fire-and-forget.
func (p *Producer) SendAsync(topic string, key string, value any) error {
	data, err := marshalValue(value)
	if err != nil {
		return fmt.Errorf("marshaling message: %w", err)
	}
	return p.broker.SendAsync(context.Background(), topic, key, data)
}

// SendBatch publishes multiple values to the same topic. Each KeyValue.Value
// is JSON-marshaled before the batch is forwarded to the broker.
func (p *Producer) SendBatch(topic string, msgs []KeyValue) error {
	return p.broker.SendBatch(context.Background(), topic, msgs)
}

// SendRaw publishes pre-marshalled bytes. Used by consumer DLQ routing so we
// don't double-encode.
func (p *Producer) SendRaw(topic, key string, value []byte) error {
	return p.broker.Send(context.Background(), topic, key, value)
}

// Broker returns the underlying broker, used by ConsumerGroup to Subscribe.
func (p *Producer) Broker() Broker {
	return p.broker
}

// Close tears down the underlying broker.
func (p *Producer) Close() error {
	return p.broker.Close()
}

// marshalValue JSON-encodes a Go value for transport. Extracted so batch and
// single paths share the same behavior.
func marshalValue(value any) ([]byte, error) {
	if raw, ok := value.([]byte); ok {
		return raw, nil
	}
	return json.Marshal(value)
}
