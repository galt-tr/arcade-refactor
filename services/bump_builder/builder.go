package bump_builder

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/IBM/sarama"
	"go.uber.org/zap"

	"github.com/bsv-blockchain/arcade/bump"
	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/kafka"
	"github.com/bsv-blockchain/arcade/models"
	"github.com/bsv-blockchain/arcade/store"
)

type Builder struct {
	cfg      *config.Config
	logger   *zap.Logger
	store    store.Store
	consumer *kafka.ConsumerGroup
	producer *kafka.Producer
}

func New(cfg *config.Config, logger *zap.Logger, st store.Store) *Builder {
	return &Builder{
		cfg:    cfg,
		logger: logger.Named("bump-builder"),
		store:  st,
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

	b.logger.Info("bump builder started",
		zap.Int("grace_window_ms", b.cfg.BumpBuilder.GraceWindowMs),
	)
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

	// Grace window: merkle-service's stumpGate only waits for the first HTTP attempt
	// of each STUMP before releasing BLOCK_PROCESSED. STUMPs that got a 5xx on the
	// first attempt retry asynchronously and may land after BLOCK_PROCESSED.
	if grace := time.Duration(b.cfg.BumpBuilder.GraceWindowMs) * time.Millisecond; grace > 0 {
		logger.Debug("waiting grace window", zap.Duration("duration", grace))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(grace):
		}
	}

	// 1. Get all STUMPs for this block
	stumps, err := b.store.GetStumpsByBlockHash(ctx, blockHash)
	if err != nil {
		return fmt.Errorf("getting STUMPs for block: %w", err)
	}

	if len(stumps) == 0 {
		logger.Info("no STUMPs found — block has no tracked transactions, skipping BUMP construction")
		return nil
	}

	logger.Info("building compound BUMP", zap.Int("stump_count", len(stumps)))

	// 2. Fetch subtree hashes + coinbase BUMP from datahub
	logger.Debug("fetching block data from datahub", zap.Strings("datahub_urls", b.cfg.DatahubURLs))
	subtreeHashes, coinbaseBUMP, err := bump.FetchBlockDataForBUMP(ctx, b.cfg.DatahubURLs, blockHash)
	if err != nil {
		return fmt.Errorf("fetching block data: %w", err)
	}
	logger.Debug("datahub fetch succeeded", zap.Int("subtree_count", len(subtreeHashes)), zap.Bool("has_coinbase_bump", coinbaseBUMP != nil))

	if len(subtreeHashes) == 0 {
		logger.Warn("block has no subtrees, cannot construct BUMPs")
		return nil
	}

	// 3. Build compound BUMP (STUMPs are sparse — only for subtrees with tracked txs)
	compound, txids, err := bump.BuildCompoundBUMP(stumps, subtreeHashes, coinbaseBUMP)
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
