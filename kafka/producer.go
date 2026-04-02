package kafka

import (
	"encoding/json"
	"fmt"

	"github.com/IBM/sarama"
)

type Producer struct {
	syncProducer  sarama.SyncProducer
	asyncProducer sarama.AsyncProducer
	brokers       []string
}

func NewProducer(brokers []string) (*Producer, error) {
	cfg := sarama.NewConfig()
	cfg.Producer.RequiredAcks = sarama.WaitForAll
	cfg.Producer.Retry.Max = 5
	cfg.Producer.Return.Successes = true
	cfg.Producer.Return.Errors = true

	syncProducer, err := sarama.NewSyncProducer(brokers, cfg)
	if err != nil {
		return nil, fmt.Errorf("creating sync producer: %w", err)
	}

	asyncCfg := sarama.NewConfig()
	asyncCfg.Producer.RequiredAcks = sarama.WaitForLocal
	asyncCfg.Producer.Retry.Max = 5
	asyncCfg.Producer.Return.Successes = true
	asyncCfg.Producer.Return.Errors = true

	asyncProducer, err := sarama.NewAsyncProducer(brokers, asyncCfg)
	if err != nil {
		syncProducer.Close()
		return nil, fmt.Errorf("creating async producer: %w", err)
	}

	return &Producer{
		syncProducer:  syncProducer,
		asyncProducer: asyncProducer,
		brokers:       brokers,
	}, nil
}

// Send publishes a message synchronously and waits for acknowledgement.
func (p *Producer) Send(topic string, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshaling message: %w", err)
	}

	msg := &sarama.ProducerMessage{
		Topic: topic,
		Key:   sarama.StringEncoder(key),
		Value: sarama.ByteEncoder(data),
	}

	_, _, err = p.syncProducer.SendMessage(msg)
	if err != nil {
		return fmt.Errorf("sending message to %s: %w", topic, err)
	}

	return nil
}

// SendAsync publishes a message asynchronously without waiting.
func (p *Producer) SendAsync(topic string, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshaling message: %w", err)
	}

	msg := &sarama.ProducerMessage{
		Topic: topic,
		Key:   sarama.StringEncoder(key),
		Value: sarama.ByteEncoder(data),
	}

	p.asyncProducer.Input() <- msg
	return nil
}

// Close shuts down both producers.
func (p *Producer) Close() error {
	var errs []error
	if err := p.syncProducer.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := p.asyncProducer.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("closing producers: %v", errs)
	}
	return nil
}
