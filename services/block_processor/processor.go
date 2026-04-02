package block_processor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/IBM/sarama"
	"go.uber.org/zap"

	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/kafka"
	"github.com/bsv-blockchain/arcade/services/p2p_client"
	"github.com/bsv-blockchain/arcade/store"
)

type Processor struct {
	cfg      *config.Config
	logger   *zap.Logger
	store    store.Store
	consumer *kafka.ConsumerGroup
	producer *kafka.Producer
}

func New(cfg *config.Config, logger *zap.Logger, st store.Store) *Processor {
	return &Processor{
		cfg:    cfg,
		logger: logger.Named("block-processor"),
		store:  st,
	}
}

func (p *Processor) Name() string { return "block-processor" }

func (p *Processor) Start(ctx context.Context) error {
	producer, err := kafka.NewProducer(p.cfg.Kafka.Brokers)
	if err != nil {
		return fmt.Errorf("creating kafka producer for DLQ: %w", err)
	}
	p.producer = producer

	consumer, err := kafka.NewConsumerGroup(kafka.ConsumerConfig{
		Brokers:    p.cfg.Kafka.Brokers,
		GroupID:    p.cfg.Kafka.ConsumerGroup + "-block-processor",
		Topics:     []string{kafka.TopicBlock},
		Handler:    p.handleMessage,
		Producer:   p.producer,
		MaxRetries: p.cfg.Kafka.MaxRetries,
		Logger:     p.logger,
	})
	if err != nil {
		return fmt.Errorf("creating consumer group: %w", err)
	}
	p.consumer = consumer

	p.logger.Info("block processor started")
	return consumer.Run(ctx)
}

func (p *Processor) Stop() error {
	p.logger.Info("stopping block processor")
	var errs []error
	if p.consumer != nil {
		if err := p.consumer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if p.producer != nil {
		if err := p.producer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("errors stopping block processor: %v", errs)
	}
	return nil
}

func (p *Processor) handleMessage(ctx context.Context, msg *sarama.ConsumerMessage) error {
	var notification p2p_client.BlockNotification
	if err := json.Unmarshal(msg.Value, &notification); err != nil {
		return fmt.Errorf("unmarshaling block notification: %w", err)
	}

	logger := p.logger.With(
		zap.String("block_hash", notification.BlockHash),
		zap.Uint64("height", notification.Height),
	)

	// Idempotency check
	onChain, err := p.store.IsBlockOnChain(ctx, notification.BlockHash)
	if err != nil {
		return fmt.Errorf("checking block existence: %w", err)
	}
	if onChain {
		logger.Debug("block already processed, skipping")
		return nil
	}

	// Mark block as processed
	if err := p.store.MarkBlockProcessed(ctx, notification.BlockHash, notification.Height, true); err != nil {
		return fmt.Errorf("marking block processed: %w", err)
	}

	_ = time.Now() // placeholder for future block data fetching

	logger.Info("block processed successfully")
	return nil
}
