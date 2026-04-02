package bump_builder

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/IBM/sarama"
	"go.uber.org/zap"

	"github.com/bsv-blockchain/arcade/kafka"
	"github.com/bsv-blockchain/arcade/models"
	"github.com/bsv-blockchain/arcade/store"
)

// StumpConsumer consumes STUMP messages and stores them in the store.
type StumpConsumer struct {
	logger   *zap.Logger
	store    store.Store
	consumer *kafka.ConsumerGroup
}

func NewStumpConsumer(brokers []string, groupID string, producer *kafka.Producer, maxRetries int, logger *zap.Logger, st store.Store) (*StumpConsumer, error) {
	sc := &StumpConsumer{
		logger: logger.Named("stump-consumer"),
		store:  st,
	}

	consumer, err := kafka.NewConsumerGroup(kafka.ConsumerConfig{
		Brokers:    brokers,
		GroupID:    groupID + "-stump",
		Topics:     []string{kafka.TopicStump},
		Handler:    sc.handleMessage,
		Producer:   producer,
		MaxRetries: maxRetries,
		Logger:     sc.logger,
	})
	if err != nil {
		return nil, err
	}
	sc.consumer = consumer

	return sc, nil
}

func (sc *StumpConsumer) Run(ctx context.Context) error {
	sc.logger.Info("stump consumer started")
	return sc.consumer.Run(ctx)
}

func (sc *StumpConsumer) Close() error {
	if sc.consumer != nil {
		return sc.consumer.Close()
	}
	return nil
}

func (sc *StumpConsumer) handleMessage(ctx context.Context, msg *sarama.ConsumerMessage) error {
	var callback models.CallbackMessage
	if err := json.Unmarshal(msg.Value, &callback); err != nil {
		return fmt.Errorf("unmarshaling stump message: %w", err)
	}

	if callback.BlockHash == "" || len(callback.Stump) == 0 {
		return fmt.Errorf("stump message missing required fields")
	}

	stump := &models.Stump{
		BlockHash:    callback.BlockHash,
		SubtreeIndex: callback.SubtreeIndex,
		StumpData:    callback.Stump,
	}

	if err := sc.store.InsertStump(ctx, stump); err != nil {
		return fmt.Errorf("storing STUMP: %w", err)
	}

	sc.logger.Debug("STUMP stored",
		zap.String("block_hash", callback.BlockHash),
		zap.Int("subtree_index", callback.SubtreeIndex),
	)
	return nil
}
