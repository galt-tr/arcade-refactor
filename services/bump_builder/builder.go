package bump_builder

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/IBM/sarama"
	"go.uber.org/zap"

	"github.com/bsv-blockchain/arcade/bump"
	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/kafka"
	"github.com/bsv-blockchain/arcade/models"
	"github.com/bsv-blockchain/arcade/store"
)

type Builder struct {
	cfg           *config.Config
	logger        *zap.Logger
	store         store.Store
	txTracker     *store.TxTracker
	consumer      *kafka.ConsumerGroup
	producer      *kafka.Producer
	stumpConsumer *StumpConsumer
}

func New(cfg *config.Config, logger *zap.Logger, st store.Store, tracker *store.TxTracker) *Builder {
	return &Builder{
		cfg:       cfg,
		logger:    logger.Named("bump-builder"),
		store:     st,
		txTracker: tracker,
	}
}

func (b *Builder) Name() string { return "bump-builder" }

func (b *Builder) Start(ctx context.Context) error {
	producer, err := kafka.NewProducer(b.cfg.Kafka.Brokers)
	if err != nil {
		return fmt.Errorf("creating kafka producer: %w", err)
	}
	b.producer = producer

	consumer, err := kafka.NewConsumerGroup(kafka.ConsumerConfig{
		Brokers:    b.cfg.Kafka.Brokers,
		GroupID:    b.cfg.Kafka.ConsumerGroup + "-bump-builder",
		Topics:     []string{kafka.TopicBlockProcessed},
		Handler:    b.handleMessage,
		Producer:   b.producer,
		MaxRetries: b.cfg.Kafka.MaxRetries,
		Logger:     b.logger,
	})
	if err != nil {
		return fmt.Errorf("creating consumer group: %w", err)
	}
	b.consumer = consumer

	// Start the STUMP consumer in a goroutine
	stumpConsumer, err := NewStumpConsumer(
		b.cfg.Kafka.Brokers,
		b.cfg.Kafka.ConsumerGroup,
		b.producer,
		b.cfg.Kafka.MaxRetries,
		b.logger,
		b.store,
	)
	if err != nil {
		return fmt.Errorf("creating stump consumer: %w", err)
	}
	b.stumpConsumer = stumpConsumer
	go stumpConsumer.Run(ctx)

	b.logger.Info("bump builder started")
	return consumer.Run(ctx)
}

func (b *Builder) Stop() error {
	b.logger.Info("stopping bump builder")
	var errs []error
	if b.consumer != nil {
		if err := b.consumer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if b.stumpConsumer != nil {
		if err := b.stumpConsumer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if b.producer != nil {
		if err := b.producer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("errors stopping bump builder: %v", errs)
	}
	return nil
}

func (b *Builder) handleMessage(ctx context.Context, msg *sarama.ConsumerMessage) error {
	var callback models.CallbackMessage
	if err := json.Unmarshal(msg.Value, &callback); err != nil {
		return fmt.Errorf("unmarshaling block processed message: %w", err)
	}

	blockHash := callback.BlockHash
	if blockHash == "" {
		return fmt.Errorf("empty block hash in block_processed message")
	}

	logger := b.logger.With(zap.String("block_hash", blockHash))

	// 1. Get all STUMPs for this block
	stumps, err := b.store.GetStumpsByBlockHash(ctx, blockHash)
	if err != nil {
		return fmt.Errorf("getting STUMPs for block: %w", err)
	}

	if len(stumps) == 0 {
		return fmt.Errorf("no STUMPs found for block %s, may not be stored yet", blockHash)
	}

	logger.Info("building compound BUMP", zap.Int("stump_count", len(stumps)))

	// 2. Fetch subtree hashes + coinbase BUMP from datahub
	subtreeHashes, coinbaseBUMP, err := bump.FetchBlockDataForBUMP(ctx, b.cfg.DatahubURLs, blockHash)
	if err != nil {
		return fmt.Errorf("fetching block data: %w", err)
	}

	if len(subtreeHashes) == 0 {
		logger.Warn("block has no subtrees, cannot construct BUMPs")
		return nil
	}

	// 3. Build compound BUMP
	compound, txids, err := bump.BuildCompoundBUMP(stumps, subtreeHashes, coinbaseBUMP, b.txTracker)
	if err != nil {
		return fmt.Errorf("building compound BUMP: %w", err)
	}

	blockHeight := uint64(compound.BlockHeight)

	// 4. Store compound BUMP as binary
	if err := b.store.InsertBUMP(ctx, blockHash, blockHeight, compound.Bytes()); err != nil {
		return fmt.Errorf("storing BUMP: %w", err)
	}

	// 5. Set tracked transactions to MINED
	if len(txids) > 0 {
		if _, err := b.store.SetMinedByTxIDs(ctx, blockHash, txids); err != nil {
			logger.Error("failed to set mined status", zap.Error(err))
		} else {
			logger.Info("set transactions to MINED",
				zap.Int("count", len(txids)),
			)
		}
	}

	// 6. Prune STUMPs
	if err := b.store.DeleteStumpsByBlockHash(ctx, blockHash); err != nil {
		logger.Warn("failed to clean up STUMPs", zap.Error(err))
	}

	logger.Info("BUMP built successfully",
		zap.Int("tracked_txids", len(txids)),
		zap.Int("stumps_pruned", len(stumps)),
	)
	return nil
}
