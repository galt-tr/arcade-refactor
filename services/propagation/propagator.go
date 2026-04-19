package propagation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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

// reaperLeaseName is the well-known key every replica uses to coordinate
// reaper ownership. A single lease per deployment — if you run separate
// propagation deployments against the same store, give them distinct consumer
// groups (which already differ by namespace) or override this constant.
const reaperLeaseName = "propagation-reaper"

type propagationMsg struct {
	TXID  string `json:"txid"`
	RawTx string `json:"raw_tx"`
}

type Propagator struct {
	cfg            *config.Config
	logger         *zap.Logger
	producer       *kafka.Producer
	store          store.Store
	leaser         store.Leaser
	teranodeClient *teranode.Client
	merkleClient   *merkleservice.Client
	consumer       *kafka.ConsumerGroup

	mu                sync.Mutex
	pendingMsgs       []propagationMsg
	merkleConcurrency int
	retryMaxAttempts  int
	retryBackoffMs    int
	reaperInterval    time.Duration
	reaperBatchSize   int
	holderID          string
	leaseTTL          time.Duration
}

// New constructs a Propagator. leaser may be nil, in which case the reaper
// runs unguarded — appropriate for tests and single-process deployments that
// don't need coordination. In production every replica should receive a
// non-nil Leaser so only one reaper is active at a time across the cluster.
func New(cfg *config.Config, logger *zap.Logger, producer *kafka.Producer, st store.Store, leaser store.Leaser, tc *teranode.Client, mc *merkleservice.Client) *Propagator {
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
	reaperInterval := time.Duration(cfg.Propagation.ReaperIntervalMs) * time.Millisecond
	if reaperInterval <= 0 {
		reaperInterval = 30 * time.Second
	}
	reaperBatch := cfg.Propagation.ReaperBatchSize
	if reaperBatch <= 0 {
		reaperBatch = 500
	}
	leaseTTL := time.Duration(cfg.Propagation.LeaseTTLMs) * time.Millisecond
	if leaseTTL <= 0 {
		// Default to 3× the tick interval so a slow or delayed tick doesn't
		// trigger a false-positive failover. This is the standard safety
		// factor for heartbeat-style leases.
		leaseTTL = 3 * reaperInterval
	}
	return &Propagator{
		cfg:               cfg,
		logger:            logger.Named("propagation"),
		producer:          producer,
		store:             st,
		leaser:            leaser,
		teranodeClient:    tc,
		merkleClient:      mc,
		merkleConcurrency: merkleConcurrency,
		retryMaxAttempts:  retryMax,
		retryBackoffMs:    retryBackoff,
		reaperInterval:    reaperInterval,
		reaperBatchSize:   reaperBatch,
		holderID:          newHolderID(),
		leaseTTL:          leaseTTL,
	}
}

// newHolderID returns a lease-holder identifier stable for this process's
// lifetime: "<hostname>-<8-hex-chars>". The random suffix disambiguates
// restarts — if an old expired-but-not-yet-purged record still names the
// previous incarnation by hostname alone, the new process will see it as a
// foreign holder and wait for TTL rather than believe it already owns the
// lease.
func newHolderID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return host + "-" + hex.EncodeToString(buf[:])
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

	// Kick off the durable-retry reaper alongside the Kafka consumer. It owns
	// all rebroadcast work for PENDING_RETRY rows, decoupled from the incoming
	// message flush cycle so a retry storm can't starve live traffic.
	go p.runReaper(ctx)

	p.logger.Info("propagation service started",
		zap.Duration("reaper_interval", p.reaperInterval),
		zap.Int("reaper_batch_size", p.reaperBatchSize),
	)
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

// flushBatch processes all accumulated messages as a batch. Retry work
// belongs to the reaper goroutine — it is no longer coupled to the consumer's
// drain-flush cycle, so live ingest doesn't have to wait on rebroadcasts.
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

// handleRetryableFailure marks a tx for durable retry. The two-call pattern
// (BumpRetryCount then SetPendingRetryFields) lets us compute the real
// exponential backoff from the post-increment count without double-
// incrementing. If retry_count exceeds retryMaxAttempts, the tx is rejected
// immediately and its retry bins are cleared.
func (p *Propagator) handleRetryableFailure(ctx context.Context, txid, rawTxHex string) {
	rawTx, err := hex.DecodeString(rawTxHex)
	if err != nil {
		p.logger.Error("invalid raw tx hex on retry", zap.String("txid", txid), zap.Error(err))
		return
	}

	retryCount, err := p.store.BumpRetryCount(ctx, txid)
	if err != nil {
		p.logger.Error("failed to bump retry count", zap.String("txid", txid), zap.Error(err))
		return
	}

	if retryCount > p.retryMaxAttempts {
		if err := p.store.ClearRetryState(ctx, txid, models.StatusRejected, "broadcast retries exhausted"); err != nil {
			p.logger.Error("failed to reject after retries exhausted", zap.String("txid", txid), zap.Error(err))
		}
		return
	}

	nextRetryAt := ComputeBackoff(p.retryBackoffMs, retryCount)
	if err := p.store.SetPendingRetryFields(ctx, txid, rawTx, nextRetryAt); err != nil {
		p.logger.Error("failed to set pending retry fields", zap.String("txid", txid), zap.Error(err))
		return
	}

	p.logger.Debug("transaction queued for retry",
		zap.String("txid", txid),
		zap.Int("attempt", retryCount),
		zap.Time("next_retry_at", nextRetryAt),
	)
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
