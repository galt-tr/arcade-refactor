package kafka

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/IBM/sarama"
	"go.uber.org/zap"
)

// MessageHandler processes a single Kafka message.
// Return an error to trigger retry/DLQ logic.
type MessageHandler func(ctx context.Context, msg *sarama.ConsumerMessage) error

// ConsumerGroup wraps a Sarama consumer group with dead-letter routing.
type ConsumerGroup struct {
	group      sarama.ConsumerGroup
	topics     []string
	handler    MessageHandler
	producer   *Producer
	maxRetries int
	logger     *zap.Logger
	ready      chan struct{}
}

type ConsumerConfig struct {
	Brokers    []string
	GroupID    string
	Topics     []string
	Handler    MessageHandler
	Producer   *Producer
	MaxRetries int
	Logger     *zap.Logger
}

func NewConsumerGroup(cfg ConsumerConfig) (*ConsumerGroup, error) {
	saramaCfg := sarama.NewConfig()
	saramaCfg.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{sarama.NewBalanceStrategyRoundRobin()}
	saramaCfg.Consumer.Offsets.Initial = sarama.OffsetOldest

	group, err := sarama.NewConsumerGroup(cfg.Brokers, cfg.GroupID, saramaCfg)
	if err != nil {
		return nil, fmt.Errorf("creating consumer group: %w", err)
	}

	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 5
	}

	return &ConsumerGroup{
		group:      group,
		topics:     cfg.Topics,
		handler:    cfg.Handler,
		producer:   cfg.Producer,
		maxRetries: maxRetries,
		logger:     cfg.Logger,
		ready:      make(chan struct{}),
	}, nil
}

// Run starts consuming messages. Blocks until context is cancelled.
func (c *ConsumerGroup) Run(ctx context.Context) error {
	for {
		if err := c.group.Consume(ctx, c.topics, c); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.logger.Error("consumer group error", zap.Error(err))
		}
		if ctx.Err() != nil {
			return nil
		}
		c.ready = make(chan struct{})
	}
}

// Ready returns a channel that is closed when the consumer is ready.
func (c *ConsumerGroup) Ready() <-chan struct{} {
	return c.ready
}

// Close shuts down the consumer group.
func (c *ConsumerGroup) Close() error {
	return c.group.Close()
}

// Setup is called at the beginning of a new consumer group session.
func (c *ConsumerGroup) Setup(sarama.ConsumerGroupSession) error {
	close(c.ready)
	return nil
}

// Cleanup is called at the end of a consumer group session.
func (c *ConsumerGroup) Cleanup(sarama.ConsumerGroupSession) error {
	return nil
}

// ConsumeClaim processes messages from a partition claim.
func (c *ConsumerGroup) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for {
		select {
		case msg, ok := <-claim.Messages():
			if !ok {
				return nil
			}
			if err := c.processWithRetry(session.Context(), msg); err != nil {
				c.sendToDLQ(msg, err)
			}
			session.MarkMessage(msg, "")
		case <-session.Context().Done():
			return nil
		}
	}
}

func (c *ConsumerGroup) processWithRetry(ctx context.Context, msg *sarama.ConsumerMessage) error {
	var lastErr error
	for attempt := 0; attempt < c.maxRetries; attempt++ {
		if err := c.handler(ctx, msg); err != nil {
			lastErr = err
			c.logger.Warn("message processing failed, retrying",
				zap.String("topic", msg.Topic),
				zap.Int("attempt", attempt+1),
				zap.Error(err),
			)
			continue
		}
		return nil
	}
	return lastErr
}

func (c *ConsumerGroup) sendToDLQ(msg *sarama.ConsumerMessage, processErr error) {
	dlqTopic := DLQTopic(msg.Topic)
	dlqMsg := map[string]any{
		"original_topic":  msg.Topic,
		"original_key":    string(msg.Key),
		"original_value":  string(msg.Value),
		"error":           processErr.Error(),
		"partition":       msg.Partition,
		"offset":          msg.Offset,
	}

	data, err := json.Marshal(dlqMsg)
	if err != nil {
		c.logger.Error("failed to marshal DLQ message", zap.Error(err))
		return
	}

	dlqProducerMsg := &sarama.ProducerMessage{
		Topic: dlqTopic,
		Key:   sarama.ByteEncoder(msg.Key),
		Value: sarama.ByteEncoder(data),
	}

	if _, _, err := c.producer.syncProducer.SendMessage(dlqProducerMsg); err != nil {
		c.logger.Error("failed to send to DLQ",
			zap.String("dlq_topic", dlqTopic),
			zap.Error(err),
		)
	} else {
		c.logger.Info("message sent to DLQ",
			zap.String("dlq_topic", dlqTopic),
			zap.String("original_topic", msg.Topic),
		)
	}
}
