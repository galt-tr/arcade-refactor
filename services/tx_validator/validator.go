package tx_validator

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/IBM/sarama"
	sdkTx "github.com/bsv-blockchain/go-sdk/transaction"
	"go.uber.org/zap"

	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/kafka"
	"github.com/bsv-blockchain/arcade/models"
	"github.com/bsv-blockchain/arcade/store"
	"github.com/bsv-blockchain/arcade/validator"
)

type Validator struct {
	cfg         *config.Config
	logger      *zap.Logger
	producer    *kafka.Producer
	store       store.Store
	txTracker   *store.TxTracker
	txValidator *validator.Validator
	consumer    *kafka.ConsumerGroup
}

func New(cfg *config.Config, logger *zap.Logger, producer *kafka.Producer, st store.Store, tracker *store.TxTracker, v *validator.Validator) *Validator {
	return &Validator{
		cfg:         cfg,
		logger:      logger.Named("tx-validator"),
		producer:    producer,
		store:       st,
		txTracker:   tracker,
		txValidator: v,
	}
}

func (v *Validator) Name() string { return "tx-validator" }

func (v *Validator) Start(ctx context.Context) error {
	consumer, err := kafka.NewConsumerGroup(kafka.ConsumerConfig{
		Brokers:    v.cfg.Kafka.Brokers,
		GroupID:    v.cfg.Kafka.ConsumerGroup + "-tx-validator",
		Topics:     []string{kafka.TopicTransaction},
		Handler:    v.handleMessage,
		Producer:   v.producer,
		MaxRetries: v.cfg.Kafka.MaxRetries,
		Logger:     v.logger,
	})
	if err != nil {
		return fmt.Errorf("creating consumer group: %w", err)
	}
	v.consumer = consumer

	v.logger.Info("tx validator started")
	return consumer.Run(ctx)
}

func (v *Validator) Stop() error {
	v.logger.Info("stopping tx validator")
	if v.consumer != nil {
		return v.consumer.Close()
	}
	return nil
}

type txMessage struct {
	Action string `json:"action,omitempty"`
	RawTx  string `json:"raw_tx,omitempty"`
}

func (v *Validator) handleMessage(ctx context.Context, msg *sarama.ConsumerMessage) error {
	var txMsg txMessage
	if err := json.Unmarshal(msg.Value, &txMsg); err != nil {
		return fmt.Errorf("unmarshaling message: %w", err)
	}

	if txMsg.Action == "submit" && txMsg.RawTx != "" {
		return v.handleNewTransaction(ctx, txMsg)
	}

	// State updates are handled by the callback handler directly
	return nil
}

func (v *Validator) handleNewTransaction(ctx context.Context, msg txMessage) error {
	rawBytes, err := hex.DecodeString(msg.RawTx)
	if err != nil {
		return fmt.Errorf("decoding hex tx: %w", err)
	}

	// Parse transaction using go-sdk (BEEF first, then raw bytes)
	var tx *sdkTx.Transaction
	beef, beefErr := sdkTx.NewBeefFromBytes(rawBytes)
	if beefErr == nil && beef != nil {
		tx = beef.FindTransactionForSigning("")
	}
	if tx == nil {
		var txErr error
		tx, txErr = sdkTx.NewTransactionFromBytes(rawBytes)
		if txErr != nil {
			v.logger.Debug("failed to parse transaction", zap.Error(txErr))
			return fmt.Errorf("failed to parse transaction: %w", txErr)
		}
	}

	txid := tx.TxID().String()
	logger := v.logger.With(zap.String("txid", txid))

	// Duplicate check via GetOrInsertStatus
	existingStatus, isNew, err := v.store.GetOrInsertStatus(ctx, &models.TransactionStatus{
		TxID:      txid,
		Timestamp: time.Now(),
	})
	if err != nil {
		return fmt.Errorf("checking duplicate: %w", err)
	}
	if !isNew {
		logger.Debug("duplicate transaction", zap.String("status", string(existingStatus.Status)))
		v.txTracker.Add(txid, existingStatus.Status)
		return nil
	}

	// Validate transaction
	if v.txValidator != nil {
		if valErr := v.txValidator.ValidateTransaction(ctx, tx, false, false); valErr != nil {
			logger.Info("transaction validation failed", zap.Error(valErr))
			v.store.UpdateStatus(ctx, &models.TransactionStatus{
				TxID:      txid,
				Status:    models.StatusRejected,
				ExtraInfo: valErr.Error(),
				Timestamp: time.Now(),
			})
			return nil // Don't retry — validation failure is permanent
		}
	}

	// Track and register with merkle service before propagation
	v.txTracker.Add(txid, existingStatus.Status)

	// Publish to propagation topic
	propMsg := map[string]string{
		"txid":   txid,
		"raw_tx": msg.RawTx,
	}
	if err := v.producer.Send(kafka.TopicPropagation, txid, propMsg); err != nil {
		return fmt.Errorf("publishing to propagation: %w", err)
	}

	logger.Info("transaction validated and queued for propagation")
	return nil
}
