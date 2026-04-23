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
	teranodeBatchCap  int
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
	teranodeBatchCap := cfg.Propagation.TeranodeMaxBatchSize
	if teranodeBatchCap <= 0 {
		teranodeBatchCap = 100
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
		teranodeBatchCap:  teranodeBatchCap,
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
		Broker:     p.producer.Broker(),
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
func (p *Propagator) handleMessage(ctx context.Context, msg *kafka.Message) error {
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

// txResult carries per-tx outcome of a broadcast, used by both the initial
// processBatch path and the reaper.
type txResult struct {
	status   *models.TransactionStatus
	errMsg   string
	rawTxHex string
}

// processBatch handles a batch of propagation messages:
// 1. Register all txids with merkle service concurrently
// 2. Broadcast all raw txs to teranode endpoints, chunked to teranodeBatchCap
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

	// Step 2: Broadcast in chunks bounded by teranodeBatchCap so a single
	// oversized Kafka flush doesn't blow past Teranode's /txs size limit.
	rawTxs := make([][]byte, len(batch))
	for i, msg := range batch {
		rawTx, _ := hex.DecodeString(msg.RawTx) // already validated in handleMessage
		rawTxs[i] = rawTx
	}
	results := p.broadcastInChunks(ctx, batch, rawTxs)

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

// broadcastInChunks splits a batch into teranodeBatchCap-sized chunks and
// broadcasts each via /txs, falling back to per-tx /tx within a chunk only
// when that chunk's batch broadcast is all-rejected. Returns per-tx results
// in the same order as the input.
func (p *Propagator) broadcastInChunks(ctx context.Context, batch []propagationMsg, rawTxs [][]byte) []txResult {
	results := make([]txResult, len(batch))
	chunkSize := p.teranodeBatchCap
	if chunkSize <= 0 {
		chunkSize = len(batch)
	}
	for start := 0; start < len(batch); start += chunkSize {
		end := start + chunkSize
		if end > len(batch) {
			end = len(batch)
		}
		p.broadcastChunk(ctx, batch[start:end], rawTxs[start:end], results[start:end])
	}
	return results
}

// broadcastChunk broadcasts a single chunk (≤ teranodeBatchCap). Single-tx
// chunks go to /tx; multi-tx chunks try /txs first and fall back to per-tx
// only on all-rejected. The per-tx fallback is logged as a single summary
// line to avoid flooding the log with one entry per transaction.
func (p *Propagator) broadcastChunk(ctx context.Context, chunk []propagationMsg, rawTxs [][]byte, out []txResult) {
	if len(chunk) == 1 {
		br := p.broadcastSingleToEndpoints(ctx, rawTxs[0], chunk[0].TXID)
		out[0] = txResult{status: br.Status, errMsg: br.ErrorMsg, rawTxHex: chunk[0].RawTx}
		return
	}

	batchStatuses := p.broadcastBatchToEndpoints(ctx, rawTxs, chunk)
	allRejected := len(batchStatuses) > 0
	for _, s := range batchStatuses {
		if s != nil && s.Status != models.StatusRejected {
			allRejected = false
			break
		}
	}
	if !allRejected {
		for i, s := range batchStatuses {
			out[i] = txResult{status: s, rawTxHex: chunk[i].RawTx}
		}
		return
	}

	// Fallback: per-tx classification for this chunk only. Summarise instead
	// of logging per call — one line per fallback, not N.
	var accepted, rejected, retryable int
	for i, msg := range chunk {
		br := p.broadcastSingleToEndpoints(ctx, rawTxs[i], msg.TXID)
		out[i] = txResult{status: br.Status, errMsg: br.ErrorMsg, rawTxHex: msg.RawTx}
		switch {
		case br.Status == nil:
			// no verdict (202 or all timed out)
		case br.Status.Status == models.StatusAcceptedByNetwork:
			accepted++
		case br.Status.Status == models.StatusRejected:
			if IsRetryableError(br.ErrorMsg) {
				retryable++
			} else {
				rejected++
			}
		}
	}
	p.logger.Info("per-tx fallback complete",
		zap.Int("chunk_size", len(chunk)),
		zap.Int("accepted", accepted),
		zap.Int("rejected", rejected),
		zap.Int("retryable", retryable),
	)
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

	// Find best status: 200→AcceptedByNetwork, 202→nil (no update), error→Rejected.
	// Per-endpoint outcomes are not logged — in a per-tx fallback path that
	// produces one DEBUG line per tx (N per chunk), drowning out useful signal.
	// Aggregate summaries are emitted by the caller (broadcastChunk).
	var bestStatus models.Status
	var lastErrMsg string
	for result := range resultCh {
		if result.err != nil {
			lastErrMsg = result.err.Error()
			if statusPriority(models.StatusRejected) > statusPriority(bestStatus) {
				bestStatus = models.StatusRejected
			}
			continue
		}
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
