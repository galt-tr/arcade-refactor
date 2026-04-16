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
	retryBuf       *RetryBuffer

	mu                sync.Mutex
	pendingMsgs       []propagationMsg
	merkleConcurrency int
	retryMaxAttempts  int
}

func New(cfg *config.Config, logger *zap.Logger, producer *kafka.Producer, st store.Store, tc *teranode.Client, mc *merkleservice.Client) *Propagator {
	merkleConcurrency := cfg.Propagation.MerkleConcurrency
	if merkleConcurrency <= 0 {
		merkleConcurrency = 10
	}
	retryMax := cfg.Propagation.RetryMaxAttempts
	if retryMax <= 0 {
		retryMax = 5
	}
	retryBackoff := cfg.Propagation.RetryBackoffMs
	if retryBackoff <= 0 {
		retryBackoff = 500
	}
	retryBufSize := cfg.Propagation.RetryBufferSize
	if retryBufSize <= 0 {
		retryBufSize = 10000
	}
	return &Propagator{
		cfg:               cfg,
		logger:            logger.Named("propagation"),
		producer:          producer,
		store:             st,
		teranodeClient:    tc,
		merkleClient:      mc,
		retryBuf:          NewRetryBuffer(retryBufSize, retryBackoff),
		merkleConcurrency: merkleConcurrency,
		retryMaxAttempts:  retryMax,
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

	// Recover any pending retries from a previous run
	p.recoverPendingRetries(ctx)

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

// flushBatch processes all accumulated messages as a batch,
// then processes any ready retry entries.
func (p *Propagator) flushBatch() error {
	p.mu.Lock()
	batch := p.pendingMsgs
	p.pendingMsgs = nil
	p.mu.Unlock()

	ctx := context.Background()

	if len(batch) > 0 {
		if err := p.processBatch(ctx, batch); err != nil {
			return err
		}
	}

	// Process any ready retries
	p.processRetries(ctx)

	return nil
}

// processBatch handles a batch of propagation messages:
// 1. Register all txids with merkle service concurrently
// 2. Broadcast all raw txs to teranode endpoints in batch
// 3. Update status for each transaction
func (p *Propagator) processBatch(ctx context.Context, batch []propagationMsg) error {
	// Log batch summary for traceability
	txidSample := make([]string, 0, 5)
	for i, msg := range batch {
		if i >= 5 {
			break
		}
		txidSample = append(txidSample, msg.TXID)
	}
	p.logger.Info("processing batch",
		zap.Int("count", len(batch)),
		zap.Strings("txids_sample", txidSample),
	)

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

	type txResult struct {
		status   *models.TransactionStatus
		errMsg   string
		rawTxHex string
	}

	var results []txResult
	if len(batch) == 1 {
		br := p.broadcastSingleToEndpoints(ctx, rawTxs[0], batch[0].TXID)
		results = []txResult{{status: br.Status, errMsg: br.ErrorMsg, rawTxHex: batch[0].RawTx}}
	} else {
		// Batch broadcast — if it fails, fall back to single-tx for per-tx error classification
		batchStatuses := p.broadcastBatchToEndpoints(ctx, rawTxs, batch)
		allRejected := true
		for _, s := range batchStatuses {
			if s != nil && s.Status != models.StatusRejected {
				allRejected = false
				break
			}
		}
		if allRejected && len(batchStatuses) > 0 {
			// Fall back to single-tx to classify each error individually
			results = make([]txResult, len(batch))
			for i, msg := range batch {
				br := p.broadcastSingleToEndpoints(ctx, rawTxs[i], msg.TXID)
				results[i] = txResult{status: br.Status, errMsg: br.ErrorMsg, rawTxHex: msg.RawTx}
			}
		} else {
			results = make([]txResult, len(batch))
			for i, s := range batchStatuses {
				results[i] = txResult{status: s, rawTxHex: batch[i].RawTx}
			}
		}
	}

	// Step 3: Update status for each transaction, with retry classification
	for i, res := range results {
		if res.status == nil {
			continue
		}
		if res.status.Status == models.StatusRejected && IsRetryableError(res.errMsg) {
			p.handleRetryableFailure(ctx, batch[i].TXID, res.rawTxHex)
			continue
		}
		if err := p.store.UpdateStatus(ctx, res.status); err != nil {
			p.logger.Error("failed to update status",
				zap.String("txid", batch[i].TXID),
				zap.Error(err),
			)
		}
	}

	p.logger.Info("batch propagated", zap.Int("count", len(batch)))
	return nil
}

// broadcastResult holds the outcome of a single-tx broadcast across all endpoints.
type broadcastResult struct {
	Status   *models.TransactionStatus
	ErrorMsg string // best error message from endpoints (for retryable classification)
}

// broadcastSingleToEndpoints submits a single transaction to each teranode endpoint
// using POST /tx, matching the original ARC single-tx broadcast behavior.
// Returns the best status and the error message from the best-matching endpoint failure.
func (p *Propagator) broadcastSingleToEndpoints(ctx context.Context, rawTx []byte, txid string) broadcastResult {
	endpoints := p.teranodeClient.GetEndpoints()
	if len(endpoints) == 0 {
		p.logger.Error("no teranode endpoints configured")
		return broadcastResult{}
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
	var lastErrMsg string
	for result := range resultCh {
		if result.err != nil {
			p.logger.Debug("single broadcast endpoint failed",
				zap.String("txid", txid),
				zap.String("endpoint", result.endpoint),
				zap.Error(result.err),
			)
			lastErrMsg = result.err.Error()
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
		return broadcastResult{}
	}

	return broadcastResult{
		Status: &models.TransactionStatus{
			TxID:      txid,
			Status:    bestStatus,
			Timestamp: time.Now(),
		},
		ErrorMsg: lastErrMsg,
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
			p.logger.Warn("batch broadcast endpoint failed",
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

// handleRetryableFailure adds a transaction to the retry buffer or rejects it if
// the buffer is full or max attempts are exhausted.
func (p *Propagator) handleRetryableFailure(ctx context.Context, txid, rawTxHex string) {
	retryCount, err := p.store.IncrementRetryCount(ctx, txid)
	if err != nil {
		p.logger.Error("failed to increment retry count", zap.String("txid", txid), zap.Error(err))
	}

	if retryCount > p.retryMaxAttempts {
		if err := p.store.UpdateStatus(ctx, &models.TransactionStatus{
			TxID:      txid,
			Status:    models.StatusRejected,
			ExtraInfo: "broadcast retries exhausted",
			Timestamp: time.Now(),
		}); err != nil {
			p.logger.Error("failed to reject after retries exhausted", zap.String("txid", txid), zap.Error(err))
		}
		p.retryBuf.Remove(txid)
		return
	}

	entry := &RetryEntry{
		TXID:      txid,
		RawTxHex:  rawTxHex,
		Attempt:   retryCount,
		NextRetry: p.retryBuf.ComputeBackoff(retryCount),
	}
	if !p.retryBuf.Add(entry) {
		if err := p.store.UpdateStatus(ctx, &models.TransactionStatus{
			TxID:      txid,
			Status:    models.StatusRejected,
			ExtraInfo: "retry buffer full",
			Timestamp: time.Now(),
		}); err != nil {
			p.logger.Error("failed to reject (buffer full)", zap.String("txid", txid), zap.Error(err))
		}
		return
	}

	// Set status to PENDING_RETRY
	if err := p.store.UpdateStatus(ctx, &models.TransactionStatus{
		TxID:      txid,
		Status:    models.StatusPendingRetry,
		Timestamp: time.Now(),
	}); err != nil {
		p.logger.Error("failed to set pending retry status", zap.String("txid", txid), zap.Error(err))
	}

	p.logger.Debug("transaction queued for retry",
		zap.String("txid", txid),
		zap.Int("attempt", retryCount),
		zap.Int("buffer_size", p.retryBuf.Len()),
	)
}

// recoverPendingRetries transitions any stale PENDING_RETRY transactions to REJECTED
// on startup. Raw tx data is not persisted, so these cannot be retried after restart.
func (p *Propagator) recoverPendingRetries(ctx context.Context) {
	txs, err := p.store.GetPendingRetryTxs(ctx)
	if err != nil {
		p.logger.Warn("failed to query pending retries on startup", zap.Error(err))
		return
	}
	if len(txs) == 0 {
		return
	}

	rejected := 0
	for _, tx := range txs {
		if err := p.store.UpdateStatus(ctx, &models.TransactionStatus{
			TxID:      tx.TxID,
			Status:    models.StatusRejected,
			ExtraInfo: "broadcast retries interrupted by service restart",
			Timestamp: time.Now(),
		}); err != nil {
			p.logger.Error("failed to reject stale pending retry", zap.String("txid", tx.TxID), zap.Error(err))
			continue
		}
		rejected++
	}
	if rejected > 0 {
		p.logger.Info("rejected stale pending retries from previous run", zap.Int("count", rejected))
	}
}

// processRetries broadcasts ready retry entries individually and handles the results.
func (p *Propagator) processRetries(ctx context.Context) {
	ready := p.retryBuf.Ready()
	if len(ready) == 0 {
		return
	}

	p.logger.Debug("processing retries", zap.Int("count", len(ready)))

	for _, entry := range ready {
		rawTx, err := hex.DecodeString(entry.RawTxHex)
		if err != nil {
			p.logger.Error("invalid raw tx hex in retry buffer", zap.String("txid", entry.TXID))
			p.retryBuf.Remove(entry.TXID)
			continue
		}

		br := p.broadcastSingleToEndpoints(ctx, rawTx, entry.TXID)

		if br.Status != nil && br.Status.Status == models.StatusAcceptedByNetwork {
			// Success
			p.retryBuf.Remove(entry.TXID)
			if err := p.store.UpdateStatus(ctx, br.Status); err != nil {
				p.logger.Error("failed to update status after retry success", zap.String("txid", entry.TXID), zap.Error(err))
			}
			p.logger.Debug("retry succeeded", zap.String("txid", entry.TXID), zap.Int("attempt", entry.Attempt))
			continue
		}

		// Still failing
		p.retryBuf.Remove(entry.TXID)
		if IsRetryableError(br.ErrorMsg) {
			p.handleRetryableFailure(ctx, entry.TXID, entry.RawTxHex)
		} else {
			// Permanent failure on retry
			status := models.StatusRejected
			extraInfo := ""
			if br.ErrorMsg != "" {
				extraInfo = br.ErrorMsg
			}
			if err := p.store.UpdateStatus(ctx, &models.TransactionStatus{
				TxID:      entry.TXID,
				Status:    status,
				ExtraInfo: extraInfo,
				Timestamp: time.Now(),
			}); err != nil {
				p.logger.Error("failed to reject after retry", zap.String("txid", entry.TXID), zap.Error(err))
			}
		}
	}
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
