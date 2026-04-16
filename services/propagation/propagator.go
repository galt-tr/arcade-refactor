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
	cfg            *config.Config
	logger         *zap.Logger
	producer       *kafka.Producer
	store          store.Store
	teranodeClient *teranode.Client
	merkleClient   *merkleservice.Client
	consumer       *kafka.ConsumerGroup

	mu                sync.Mutex
	pendingMsgs       []propagationMsg
	merkleConcurrency int
}

func New(cfg *config.Config, logger *zap.Logger, producer *kafka.Producer, st store.Store, tc *teranode.Client, mc *merkleservice.Client) *Propagator {
	merkleConcurrency := cfg.Propagation.MerkleConcurrency
	if merkleConcurrency <= 0 {
		merkleConcurrency = 10
	}
	return &Propagator{
		cfg:               cfg,
		logger:            logger.Named("propagation"),
		producer:          producer,
		store:             st,
		teranodeClient:    tc,
		merkleClient:      mc,
		merkleConcurrency: merkleConcurrency,
	}
}

func (p *Propagator) Name() string { return "propagation" }

func (p *Propagator) Start(ctx context.Context) error {
	consumer, err := kafka.NewConsumerGroup(kafka.ConsumerConfig{
		Brokers:    p.cfg.Kafka.Brokers,
		GroupID:    p.cfg.Kafka.ConsumerGroup + "-propagation",
		Topics:     []string{kafka.TopicPropagation},
		Handler:    p.handleMessage,
		FlushFunc:  p.flushBatch,
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

// handleMessage accumulates a single propagation message into the pending batch.
// The consumer's drain-then-flush pattern calls flushBatch after all immediately
// available messages have been processed, so the batch size naturally matches
// what the client submitted — no configured threshold needed.
func (p *Propagator) handleMessage(ctx context.Context, msg *sarama.ConsumerMessage) error {
	var propMsg propagationMsg
	if err := json.Unmarshal(msg.Value, &propMsg); err != nil {
		return fmt.Errorf("unmarshaling propagation message: %w", err)
	}

	// Validate hex before accumulating
	if _, err := hex.DecodeString(propMsg.RawTx); err != nil {
		return fmt.Errorf("decoding raw tx hex: %w", err)
	}

	p.mu.Lock()
	p.pendingMsgs = append(p.pendingMsgs, propMsg)
	p.mu.Unlock()

	return nil
}

// flushBatch processes all accumulated messages as a batch.
func (p *Propagator) flushBatch() error {
	p.mu.Lock()
	batch := p.pendingMsgs
	p.pendingMsgs = nil
	p.mu.Unlock()

	if len(batch) == 0 {
		return nil
	}

	return p.processBatch(context.Background(), batch)
}

// processBatch handles a batch of propagation messages:
// 1. Register all txids with merkle service concurrently
// 2. Broadcast all raw txs to teranode endpoints in batch
// 3. Update status for each transaction
func (p *Propagator) processBatch(ctx context.Context, batch []propagationMsg) error {
	// Step 1: Register all txids with merkle service (mandatory)
	if p.merkleClient != nil && p.cfg.CallbackURL != "" {
		regs := make([]merkleservice.Registration, len(batch))
		for i, msg := range batch {
			regs[i] = merkleservice.Registration{
				TxID:        msg.TXID,
				CallbackURL: p.cfg.CallbackURL,
			}
		}
		if err := p.merkleClient.RegisterBatch(ctx, regs, p.merkleConcurrency); err != nil {
			return fmt.Errorf("merkle-service batch registration failed: %w", err)
		}
		p.logger.Debug("batch registered with merkle-service", zap.Int("count", len(batch)))
	}

	// Step 2: Broadcast all raw txs to teranode endpoints
	rawTxs := make([][]byte, len(batch))
	for i, msg := range batch {
		rawTx, _ := hex.DecodeString(msg.RawTx) // already validated in handleMessage
		rawTxs[i] = rawTx
	}

	var bestStatuses []*models.TransactionStatus
	if len(batch) == 1 {
		status := p.broadcastSingleToEndpoints(ctx, rawTxs[0], batch[0].TXID)
		bestStatuses = []*models.TransactionStatus{status}
	} else {
		bestStatuses = p.broadcastBatchToEndpoints(ctx, rawTxs, batch)
	}

	// Step 3: Update status for each transaction
	for i, status := range bestStatuses {
		if status != nil {
			if err := p.store.UpdateStatus(ctx, status); err != nil {
				p.logger.Error("failed to update status",
					zap.String("txid", batch[i].TXID),
					zap.Error(err),
				)
			}
		}
	}

	p.logger.Info("batch propagated", zap.Int("count", len(batch)))
	return nil
}

// broadcastSingleToEndpoints submits a single transaction to each teranode endpoint
// using POST /tx, matching the original ARC single-tx broadcast behavior.
func (p *Propagator) broadcastSingleToEndpoints(ctx context.Context, rawTx []byte, txid string) *models.TransactionStatus {
	endpoints := p.teranodeClient.GetEndpoints()
	if len(endpoints) == 0 {
		p.logger.Error("no teranode endpoints configured")
		return nil
	}

	type endpointResult struct {
		endpoint   string
		statusCode int
		err        error
	}

	resultCh := make(chan endpointResult, len(endpoints))
	submitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, endpoint := range endpoints {
		wg.Add(1)
		go func(ep string) {
			defer wg.Done()
			statusCode, err := p.teranodeClient.SubmitTransaction(submitCtx, ep, rawTx)
			resultCh <- endpointResult{endpoint: ep, statusCode: statusCode, err: err}
		}(endpoint)
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Find best status: 200→AcceptedByNetwork, 202→nil (no update), error→Rejected
	var bestStatus models.Status
	for result := range resultCh {
		if result.err != nil {
			p.logger.Debug("single broadcast endpoint failed",
				zap.String("txid", txid),
				zap.String("endpoint", result.endpoint),
				zap.Error(result.err),
			)
			if statusPriority(models.StatusRejected) > statusPriority(bestStatus) {
				bestStatus = models.StatusRejected
			}
			continue
		}
		p.logger.Debug("single broadcast endpoint succeeded",
			zap.String("txid", txid),
			zap.String("endpoint", result.endpoint),
			zap.Int("status_code", result.statusCode),
		)
		switch result.statusCode {
		case http.StatusOK:
			if statusPriority(models.StatusAcceptedByNetwork) > statusPriority(bestStatus) {
				bestStatus = models.StatusAcceptedByNetwork
			}
		case http.StatusAccepted:
			// 202 means no status update — matching original behavior
		}
	}

	if bestStatus == "" {
		return nil
	}

	return &models.TransactionStatus{
		TxID:      txid,
		Status:    bestStatus,
		Timestamp: time.Now(),
	}
}

// broadcastBatchToEndpoints submits all transactions to each teranode endpoint
// using the batch POST /txs endpoint. Uses binary success/failure logic matching
// the original: any endpoint success → AcceptedByNetwork for all, all fail → Rejected for all.
func (p *Propagator) broadcastBatchToEndpoints(ctx context.Context, rawTxs [][]byte, batch []propagationMsg) []*models.TransactionStatus {
	endpoints := p.teranodeClient.GetEndpoints()
	if len(endpoints) == 0 {
		p.logger.Error("no teranode endpoints configured")
		return make([]*models.TransactionStatus, len(batch))
	}

	type endpointResult struct {
		endpoint string
		err      error
	}

	resultCh := make(chan endpointResult, len(endpoints))
	submitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, endpoint := range endpoints {
		wg.Add(1)
		go func(ep string) {
			defer wg.Done()
			_, err := p.teranodeClient.SubmitTransactions(submitCtx, ep, rawTxs)
			resultCh <- endpointResult{endpoint: ep, err: err}
		}(endpoint)
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Binary outcome: any success → AcceptedByNetwork, all fail → Rejected
	anySuccess := false
	for result := range resultCh {
		if result.err != nil {
			p.logger.Debug("batch broadcast endpoint failed",
				zap.String("endpoint", result.endpoint),
				zap.Int("batch_size", len(batch)),
				zap.Error(result.err),
			)
		} else {
			p.logger.Debug("batch broadcast endpoint succeeded",
				zap.String("endpoint", result.endpoint),
				zap.Int("batch_size", len(batch)),
			)
			anySuccess = true
		}
	}

	p.logger.Debug("batch broadcast complete",
		zap.Int("batch_size", len(batch)),
		zap.Bool("any_success", anySuccess),
		zap.Int("endpoint_count", len(endpoints)),
	)

	now := time.Now()
	statuses := make([]*models.TransactionStatus, len(batch))
	status := models.StatusRejected
	if anySuccess {
		status = models.StatusAcceptedByNetwork
	}
	for i, msg := range batch {
		statuses[i] = &models.TransactionStatus{
			TxID:      msg.TXID,
			Status:    status,
			Timestamp: now,
		}
	}

	return statuses
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
