package propagation

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/IBM/sarama"
	"go.uber.org/zap"

	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/kafka"
	"github.com/bsv-blockchain/arcade/merkleservice"
	"github.com/bsv-blockchain/arcade/models"
	"github.com/bsv-blockchain/arcade/store"
	"github.com/bsv-blockchain/arcade/teranode"
)

type propagationMsg struct {
	TXID  string `json:"txid"`
	RawTx string `json:"raw_tx"`
}

type Propagator struct {
	cfg             *config.Config
	logger          *zap.Logger
	producer        *kafka.Producer
	store           store.Store
	teranodeClient  *teranode.Client
	merkleClient    *merkleservice.Client
	consumer        *kafka.ConsumerGroup
}

func New(cfg *config.Config, logger *zap.Logger, producer *kafka.Producer, st store.Store, tc *teranode.Client, mc *merkleservice.Client) *Propagator {
	return &Propagator{
		cfg:            cfg,
		logger:         logger.Named("propagation"),
		producer:       producer,
		store:          st,
		teranodeClient: tc,
		merkleClient:   mc,
	}
}

func (p *Propagator) Name() string { return "propagation" }

func (p *Propagator) Start(ctx context.Context) error {
	consumer, err := kafka.NewConsumerGroup(kafka.ConsumerConfig{
		Brokers:    p.cfg.Kafka.Brokers,
		GroupID:    p.cfg.Kafka.ConsumerGroup + "-propagation",
		Topics:     []string{kafka.TopicPropagation},
		Handler:    p.handleMessage,
		Producer:   p.producer,
		MaxRetries: p.cfg.Kafka.MaxRetries,
		Logger:     p.logger,
	})
	if err != nil {
		return fmt.Errorf("creating consumer group: %w", err)
	}
	p.consumer = consumer

	p.logger.Info("propagation service started")
	return consumer.Run(ctx)
}

func (p *Propagator) Stop() error {
	p.logger.Info("stopping propagation service")
	if p.consumer != nil {
		return p.consumer.Close()
	}
	return nil
}

func (p *Propagator) handleMessage(ctx context.Context, msg *sarama.ConsumerMessage) error {
	var propMsg propagationMsg
	if err := json.Unmarshal(msg.Value, &propMsg); err != nil {
		return fmt.Errorf("unmarshaling propagation message: %w", err)
	}

	logger := p.logger.With(zap.String("txid", propMsg.TXID))

	rawTx, err := hex.DecodeString(propMsg.RawTx)
	if err != nil {
		return fmt.Errorf("decoding raw tx hex: %w", err)
	}

	// Register with merkle-service BEFORE broadcasting (best-effort)
	if p.merkleClient != nil && p.cfg.CallbackURL != "" {
		if err := p.merkleClient.Register(ctx, propMsg.TXID, p.cfg.CallbackURL); err != nil {
			logger.Warn("failed to register with merkle-service", zap.Error(err))
		}
	}

	// Broadcast to all datahub endpoints concurrently via teranode client
	bestStatus := p.broadcastToEndpoints(ctx, rawTx, propMsg.TXID)

	// Update transaction status based on best result
	if bestStatus != nil {
		if err := p.store.UpdateStatus(ctx, bestStatus); err != nil {
			logger.Error("failed to update status", zap.Error(err))
		}
	}

	logger.Info("transaction propagated")
	return nil
}

// broadcastToEndpoints submits to all teranode endpoints concurrently and returns the best status.
func (p *Propagator) broadcastToEndpoints(ctx context.Context, rawTx []byte, txid string) *models.TransactionStatus {
	endpoints := p.teranodeClient.GetEndpoints()
	if len(endpoints) == 0 {
		p.logger.Error("no teranode endpoints configured")
		return nil
	}

	resultCh := make(chan *models.TransactionStatus, len(endpoints))
	submitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, endpoint := range endpoints {
		wg.Add(1)
		go func(ep string) {
			defer wg.Done()
			statusCode, err := p.teranodeClient.SubmitTransaction(submitCtx, ep, rawTx)
			if err != nil {
				p.logger.Debug("endpoint rejected tx",
					zap.String("txid", txid),
					zap.String("endpoint", ep),
					zap.Error(err),
				)
				resultCh <- &models.TransactionStatus{
					TxID:      txid,
					Status:    models.StatusRejected,
					Timestamp: time.Now(),
					ExtraInfo: err.Error(),
				}
				return
			}

			var txStatus models.Status
			switch statusCode {
			case http.StatusOK:
				txStatus = models.StatusAcceptedByNetwork
			case http.StatusNoContent, http.StatusAccepted:
				txStatus = models.StatusSentToNetwork
			default:
				return
			}

			resultCh <- &models.TransactionStatus{
				TxID:      txid,
				Status:    txStatus,
				Timestamp: time.Now(),
			}
		}(endpoint)
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	var best *models.TransactionStatus
	for status := range resultCh {
		if status == nil {
			continue
		}
		if best == nil || statusPriority(status.Status) > statusPriority(best.Status) {
			best = status
		}
	}

	return best
}

// statusPriority returns a numeric priority for broadcast result aggregation.
func statusPriority(s models.Status) int {
	switch s {
	case models.StatusAcceptedByNetwork:
		return 3
	case models.StatusSentToNetwork:
		return 2
	case models.StatusRejected:
		return 1
	default:
		return 0
	}
}
