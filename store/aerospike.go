package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	aero "github.com/aerospike/aerospike-client-go/v7"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/models"
)

func isKeyNotFound(err error) bool {
	return errors.Is(err, aero.ErrKeyNotFound)
}

const (
	setTransactions   = "transactions"
	setBumps          = "bumps"
	setStumps         = "stumps"
	setSubmissions    = "submissions"
	setProcessedBlocks = "processed_blocks"
)

// Ensure AerospikeStore implements Store
var _ Store = (*AerospikeStore)(nil)

type AerospikeStore struct {
	client    *aero.Client
	namespace string
	batchSize int
}

func NewAerospikeStore(cfg config.Aero) (*AerospikeStore, error) {
	hosts := make([]*aero.Host, 0, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		hostname, portStr, err := net.SplitHostPort(h)
		if err != nil {
			// No port specified, use default
			hosts = append(hosts, aero.NewHost(h, 3000))
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("invalid port in host %q: %w", h, err)
		}
		hosts = append(hosts, aero.NewHost(hostname, port))
	}

	policy := aero.NewClientPolicy()
	policy.ConnectionQueueSize = cfg.PoolSize

	client, err := aero.NewClientWithPolicyAndHost(policy, hosts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to aerospike: %w", err)
	}

	s := &AerospikeStore{
		client:    client,
		namespace: cfg.Namespace,
		batchSize: cfg.BatchSize,
	}

	if err := s.EnsureIndexes(); err != nil {
		client.Close()
		return nil, fmt.Errorf("creating indexes: %w", err)
	}

	return s, nil
}

func (s *AerospikeStore) Healthy() bool {
	return s.client.IsConnected()
}

func (s *AerospikeStore) Close() error {
	s.client.Close()
	return nil
}

func (s *AerospikeStore) EnsureIndexes() error {
	indexes := []struct {
		set, bin, name string
		indexType      aero.IndexType
	}{
		{setStumps, "block_hash", "idx_stumps_block_hash", aero.STRING},
		{setTransactions, "block_hash", "idx_tx_block_hash", aero.STRING},
		{setSubmissions, "txid", "idx_sub_txid", aero.STRING},
		{setSubmissions, "callback_token", "idx_sub_callback_token", aero.STRING},
		{setProcessedBlocks, "block_height", "idx_pb_block_height", aero.NUMERIC},
		{setTransactions, "status", "idx_tx_status", aero.STRING},
	}
	for _, idx := range indexes {
		_, err := s.client.CreateIndex(nil, s.namespace, idx.set, idx.name, idx.bin, idx.indexType)
		if err != nil {
			if !strings.Contains(err.Error(), "Index already exists") {
				return fmt.Errorf("creating index %s: %w", idx.name, err)
			}
		}
	}
	return nil
}

func (s *AerospikeStore) key(set string, pk string) (*aero.Key, error) {
	return aero.NewKey(s.namespace, set, pk)
}

// --- Transaction Status Operations ---

func (s *AerospikeStore) GetOrInsertStatus(_ context.Context, status *models.TransactionStatus) (*models.TransactionStatus, bool, error) {
	key, err := s.key(setTransactions, status.TxID)
	if err != nil {
		return nil, false, err
	}

	// Try to read existing
	rec, err := s.client.Get(nil, key)
	if err != nil && !isKeyNotFound(err) {
		return nil, false, fmt.Errorf("get status: %w", err)
	}
	if rec != nil {
		existing := recordToStatus(rec, status.TxID)
		return existing, false, nil
	}

	// Insert new
	now := time.Now()
	if status.Timestamp.IsZero() {
		status.Timestamp = now
	}
	st := string(models.StatusReceived)
	if status.Status != "" {
		st = string(status.Status)
	}

	bins := aero.BinMap{
		"txid":       status.TxID,
		"status":     st,
		"timestamp":  status.Timestamp.UnixMilli(),
		"created_at": now.UnixMilli(),
	}

	policy := aero.NewWritePolicy(0, 0)
	policy.RecordExistsAction = aero.CREATE_ONLY
	if err := s.client.Put(policy, key, bins); err != nil {
		// Race condition: someone else inserted — re-read
		rec, getErr := s.client.Get(nil, key)
		if getErr != nil && !isKeyNotFound(getErr) {
			return nil, false, fmt.Errorf("re-read after conflict: %w", getErr)
		}
		if rec != nil {
			return recordToStatus(rec, status.TxID), false, nil
		}
		return nil, false, fmt.Errorf("insert status: %w", err)
	}

	status.Status = models.Status(st)
	status.CreatedAt = now
	return status, true, nil
}

func (s *AerospikeStore) UpdateStatus(_ context.Context, status *models.TransactionStatus) error {
	key, err := s.key(setTransactions, status.TxID)
	if err != nil {
		return err
	}

	bins := aero.BinMap{
		"status":    string(status.Status),
		"timestamp": status.Timestamp.UnixMilli(),
	}
	if status.BlockHash != "" {
		bins["block_hash"] = status.BlockHash
	}
	if status.BlockHeight > 0 {
		bins["block_height"] = int(status.BlockHeight)
	}
	if status.ExtraInfo != "" {
		bins["extra_info"] = status.ExtraInfo
	}
	if len(status.MerklePath) > 0 {
		bins["merkle_path"] = []byte(status.MerklePath)
	}

	return s.client.Put(nil, key, bins)
}

func (s *AerospikeStore) GetStatus(ctx context.Context, txid string) (*models.TransactionStatus, error) {
	key, err := s.key(setTransactions, txid)
	if err != nil {
		return nil, err
	}

	rec, err := s.client.Get(nil, key)
	if err != nil && !isKeyNotFound(err) {
		return nil, fmt.Errorf("get status %s: %w", txid, err)
	}
	if rec == nil {
		return nil, nil
	}

	status := recordToStatus(rec, txid)
	s.enrichMerklePath(ctx, status)
	return status, nil
}

func (s *AerospikeStore) GetStatusesSince(_ context.Context, _ time.Time) ([]*models.TransactionStatus, error) {
	// Scan all transactions — for TxTracker loading
	stmt := aero.NewStatement(s.namespace, setTransactions)
	rs, err := s.client.Query(nil, stmt)
	if err != nil {
		return nil, fmt.Errorf("query statuses: %w", err)
	}
	defer rs.Close()

	var results []*models.TransactionStatus
	for rec := range rs.Results() {
		if rec.Err != nil {
			return nil, rec.Err
		}
		txid := getString(rec.Record, "txid")
		results = append(results, recordToStatus(rec.Record, txid))
	}
	return results, nil
}

func (s *AerospikeStore) SetStatusByBlockHash(_ context.Context, blockHash string, newStatus models.Status) ([]string, error) {
	stmt := aero.NewStatement(s.namespace, setTransactions)
	stmt.SetFilter(aero.NewEqualFilter("block_hash", blockHash))

	rs, err := s.client.Query(nil, stmt)
	if err != nil {
		return nil, fmt.Errorf("query by block hash: %w", err)
	}
	defer rs.Close()

	var txids []string
	for rec := range rs.Results() {
		if rec.Err != nil {
			continue
		}
		txid := getString(rec.Record, "txid")
		if txid == "" {
			continue
		}
		key, err := s.key(setTransactions, txid)
		if err != nil {
			continue
		}
		bins := aero.BinMap{"status": string(newStatus), "timestamp": time.Now().UnixMilli()}
		s.client.Put(nil, key, bins)
		txids = append(txids, txid)
	}
	return txids, nil
}

func (s *AerospikeStore) IncrementRetryCount(_ context.Context, txid string) (int, error) {
	key, err := s.key(setTransactions, txid)
	if err != nil {
		return 0, err
	}
	rec, err := s.client.Operate(nil, key, aero.AddOp(aero.NewBin("retry_count", 1)), aero.GetOp())
	if err != nil {
		return 0, fmt.Errorf("increment retry count %s: %w", txid, err)
	}
	if v, ok := rec.Bins["retry_count"]; ok {
		if n, ok := v.(int); ok {
			return n, nil
		}
	}
	return 1, nil
}

func (s *AerospikeStore) GetPendingRetryTxs(_ context.Context) ([]*models.TransactionStatus, error) {
	stmt := aero.NewStatement(s.namespace, setTransactions)
	stmt.SetFilter(aero.NewEqualFilter("status", string(models.StatusPendingRetry)))

	rs, err := s.client.Query(nil, stmt)
	if err != nil {
		return nil, fmt.Errorf("query pending retry txs: %w", err)
	}
	defer rs.Close()

	var results []*models.TransactionStatus
	for rec := range rs.Results() {
		if rec.Err != nil {
			return nil, rec.Err
		}
		txid := getString(rec.Record, "txid")
		results = append(results, recordToStatus(rec.Record, txid))
	}
	return results, nil
}

func (s *AerospikeStore) SetMinedByTxIDs(_ context.Context, blockHash string, txids []string) ([]*models.TransactionStatus, error) {
	now := time.Now()
	var statuses []*models.TransactionStatus

	for i := 0; i < len(txids); i += s.batchSize {
		end := i + s.batchSize
		if end > len(txids) {
			end = len(txids)
		}
		batch := txids[i:end]

		records := make([]aero.BatchRecordIfc, len(batch))
		for j, txid := range batch {
			key, err := s.key(setTransactions, txid)
			if err != nil {
				continue
			}
			ops := []*aero.Operation{
				aero.PutOp(aero.NewBin("status", string(models.StatusMined))),
				aero.PutOp(aero.NewBin("block_hash", blockHash)),
				aero.PutOp(aero.NewBin("timestamp", now.UnixMilli())),
			}
			records[j] = aero.NewBatchWrite(aero.NewBatchWritePolicy(), key, ops...)
		}

		s.client.BatchOperate(nil, records)

		for _, txid := range batch {
			statuses = append(statuses, &models.TransactionStatus{
				TxID:      txid,
				Status:    models.StatusMined,
				BlockHash: blockHash,
				Timestamp: now,
			})
		}
	}

	return statuses, nil
}

// --- BUMP Operations ---

func (s *AerospikeStore) InsertBUMP(_ context.Context, blockHash string, blockHeight uint64, bumpData []byte) error {
	key, err := s.key(setBumps, blockHash)
	if err != nil {
		return err
	}
	bins := aero.BinMap{
		"block_hash":   blockHash,
		"block_height": int(blockHeight),
		"bump_data":    bumpData,
	}
	return s.client.Put(nil, key, bins)
}

func (s *AerospikeStore) GetBUMP(_ context.Context, blockHash string) (uint64, []byte, error) {
	key, err := s.key(setBumps, blockHash)
	if err != nil {
		return 0, nil, err
	}
	rec, err := s.client.Get(nil, key)
	if err != nil && !isKeyNotFound(err) {
		return 0, nil, fmt.Errorf("get bump %s: %w", blockHash, err)
	}
	if rec == nil {
		return 0, nil, ErrNotFound
	}

	var height uint64
	if v, ok := rec.Bins["block_height"]; ok {
		if n, ok := v.(int); ok {
			height = uint64(n)
		}
	}
	var data []byte
	if v, ok := rec.Bins["bump_data"]; ok {
		if b, ok := v.([]byte); ok {
			data = b
		}
	}
	return height, data, nil
}

// --- STUMP Operations (keyed by blockHash:subtreeIndex) ---

func (s *AerospikeStore) InsertStump(_ context.Context, stump *models.Stump) error {
	pk := fmt.Sprintf("%s:%d", stump.BlockHash, stump.SubtreeIndex)
	key, err := s.key(setStumps, pk)
	if err != nil {
		return err
	}
	bins := aero.BinMap{
		"block_hash":    stump.BlockHash,
		"subtree_index": stump.SubtreeIndex,
		"stump_data":    stump.StumpData,
	}
	return s.client.Put(nil, key, bins)
}

func (s *AerospikeStore) GetStumpsByBlockHash(_ context.Context, blockHash string) ([]*models.Stump, error) {
	stmt := aero.NewStatement(s.namespace, setStumps)
	stmt.SetFilter(aero.NewEqualFilter("block_hash", blockHash))

	rs, err := s.client.Query(nil, stmt)
	if err != nil {
		return nil, fmt.Errorf("query stumps: %w", err)
	}
	defer rs.Close()

	var stumps []*models.Stump
	for rec := range rs.Results() {
		if rec.Err != nil {
			return nil, rec.Err
		}
		stump := &models.Stump{
			BlockHash: getString(rec.Record, "block_hash"),
		}
		if v, ok := rec.Record.Bins["subtree_index"]; ok {
			if n, ok := v.(int); ok {
				stump.SubtreeIndex = n
			}
		}
		if v, ok := rec.Record.Bins["stump_data"]; ok {
			if b, ok := v.([]byte); ok {
				stump.StumpData = b
			}
		}
		stumps = append(stumps, stump)
	}
	return stumps, nil
}

func (s *AerospikeStore) DeleteStumpsByBlockHash(_ context.Context, blockHash string) error {
	stumps, err := s.GetStumpsByBlockHash(context.Background(), blockHash)
	if err != nil {
		return err
	}

	for i := 0; i < len(stumps); i += s.batchSize {
		end := i + s.batchSize
		if end > len(stumps) {
			end = len(stumps)
		}
		batch := stumps[i:end]

		keys := make([]*aero.Key, len(batch))
		for j, st := range batch {
			pk := fmt.Sprintf("%s:%d", st.BlockHash, st.SubtreeIndex)
			keys[j], _ = s.key(setStumps, pk)
		}

		records := make([]aero.BatchRecordIfc, len(keys))
		for j, key := range keys {
			records[j] = aero.NewBatchDelete(nil, key)
		}
		s.client.BatchOperate(nil, records)
	}
	return nil
}

// --- Submission Operations ---

func (s *AerospikeStore) InsertSubmission(_ context.Context, sub *models.Submission) error {
	key, err := s.key(setSubmissions, sub.SubmissionID)
	if err != nil {
		return err
	}
	bins := aero.BinMap{
		"submission_id":       sub.SubmissionID,
		"txid":                sub.TxID,
		"callback_url":        sub.CallbackURL,
		"callback_token":      sub.CallbackToken,
		"full_status_updates": sub.FullStatusUpdates,
		"created_at":          sub.CreatedAt.UnixMilli(),
	}
	return s.client.Put(nil, key, bins)
}

func (s *AerospikeStore) GetSubmissionsByTxID(_ context.Context, txid string) ([]*models.Submission, error) {
	stmt := aero.NewStatement(s.namespace, setSubmissions)
	stmt.SetFilter(aero.NewEqualFilter("txid", txid))

	rs, err := s.client.Query(nil, stmt)
	if err != nil {
		return nil, err
	}
	defer rs.Close()

	var subs []*models.Submission
	for rec := range rs.Results() {
		if rec.Err != nil {
			continue
		}
		subs = append(subs, recordToSubmission(rec.Record))
	}
	return subs, nil
}

func (s *AerospikeStore) GetSubmissionsByToken(_ context.Context, token string) ([]*models.Submission, error) {
	stmt := aero.NewStatement(s.namespace, setSubmissions)
	stmt.SetFilter(aero.NewEqualFilter("callback_token", token))

	rs, err := s.client.Query(nil, stmt)
	if err != nil {
		return nil, err
	}
	defer rs.Close()

	var subs []*models.Submission
	for rec := range rs.Results() {
		if rec.Err != nil {
			continue
		}
		subs = append(subs, recordToSubmission(rec.Record))
	}
	return subs, nil
}

func (s *AerospikeStore) UpdateDeliveryStatus(_ context.Context, submissionID string, lastStatus models.Status, retryCount int, nextRetry *time.Time) error {
	key, err := s.key(setSubmissions, submissionID)
	if err != nil {
		return err
	}
	bins := aero.BinMap{
		"last_delivered_status": string(lastStatus),
		"retry_count":           retryCount,
	}
	if nextRetry != nil {
		bins["next_retry_at"] = nextRetry.UnixMilli()
	}
	return s.client.Put(nil, key, bins)
}

// --- Block Tracking Operations ---

func (s *AerospikeStore) IsBlockOnChain(_ context.Context, blockHash string) (bool, error) {
	key, err := s.key(setProcessedBlocks, blockHash)
	if err != nil {
		return false, err
	}
	rec, err := s.client.Get(nil, key, "on_chain")
	if err != nil && !isKeyNotFound(err) {
		return false, err
	}
	if rec == nil {
		return false, nil
	}
	if v, ok := rec.Bins["on_chain"]; ok {
		if b, ok := v.(int); ok {
			return b == 1, nil
		}
	}
	return false, nil
}

func (s *AerospikeStore) MarkBlockProcessed(_ context.Context, blockHash string, blockHeight uint64, onChain bool) error {
	key, err := s.key(setProcessedBlocks, blockHash)
	if err != nil {
		return err
	}
	onChainVal := 0
	if onChain {
		onChainVal = 1
	}
	bins := aero.BinMap{
		"block_hash":   blockHash,
		"block_height": int(blockHeight),
		"on_chain":     onChainVal,
	}
	return s.client.Put(nil, key, bins)
}

func (s *AerospikeStore) HasAnyProcessedBlocks(_ context.Context) (bool, error) {
	stmt := aero.NewStatement(s.namespace, setProcessedBlocks)
	rs, err := s.client.Query(nil, stmt)
	if err != nil {
		return false, err
	}
	defer rs.Close()

	for rec := range rs.Results() {
		if rec.Err == nil {
			return true, nil
		}
	}
	return false, nil
}

func (s *AerospikeStore) GetOnChainBlockAtHeight(_ context.Context, height uint64) (string, bool, error) {
	stmt := aero.NewStatement(s.namespace, setProcessedBlocks)
	stmt.SetFilter(aero.NewEqualFilter("block_height", int(height)))

	rs, err := s.client.Query(nil, stmt)
	if err != nil {
		return "", false, err
	}
	defer rs.Close()

	for rec := range rs.Results() {
		if rec.Err != nil {
			continue
		}
		if v, ok := rec.Record.Bins["on_chain"]; ok {
			if n, ok := v.(int); ok && n == 1 {
				return getString(rec.Record, "block_hash"), true, nil
			}
		}
	}
	return "", false, nil
}

func (s *AerospikeStore) MarkBlockOffChain(_ context.Context, blockHash string) error {
	key, err := s.key(setProcessedBlocks, blockHash)
	if err != nil {
		return err
	}
	return s.client.Put(nil, key, aero.BinMap{"on_chain": 0})
}

// --- Helpers ---

func recordToStatus(rec *aero.Record, txid string) *models.TransactionStatus {
	status := &models.TransactionStatus{TxID: txid}
	if v, ok := rec.Bins["status"]; ok {
		if s, ok := v.(string); ok {
			status.Status = models.Status(s)
		}
	}
	if v, ok := rec.Bins["block_hash"]; ok {
		if s, ok := v.(string); ok {
			status.BlockHash = s
		}
	}
	if v, ok := rec.Bins["block_height"]; ok {
		if n, ok := v.(int); ok {
			status.BlockHeight = uint64(n)
		}
	}
	if v, ok := rec.Bins["extra_info"]; ok {
		if s, ok := v.(string); ok {
			status.ExtraInfo = s
		}
	}
	if v, ok := rec.Bins["merkle_path"]; ok {
		if b, ok := v.([]byte); ok {
			status.MerklePath = b
		}
	}
	if v, ok := rec.Bins["competing_txs"]; ok {
		switch ct := v.(type) {
		case []byte:
			json.Unmarshal(ct, &status.CompetingTxs)
		case string:
			json.Unmarshal([]byte(ct), &status.CompetingTxs)
		}
	}
	if v, ok := rec.Bins["timestamp"]; ok {
		if ms, ok := v.(int); ok {
			status.Timestamp = time.UnixMilli(int64(ms))
		}
	}
	if v, ok := rec.Bins["created_at"]; ok {
		if ms, ok := v.(int); ok {
			status.CreatedAt = time.UnixMilli(int64(ms))
		}
	}
	return status
}

// enrichMerklePath fetches the compound BUMP for a mined/immutable transaction
// and extracts the per-tx minimal merkle path if not already present.
func (s *AerospikeStore) enrichMerklePath(ctx context.Context, status *models.TransactionStatus) {
	if status == nil || len(status.MerklePath) > 0 || status.BlockHash == "" {
		return
	}
	if status.Status != models.StatusMined && status.Status != models.StatusImmutable {
		return
	}
	_, bumpData, err := s.GetBUMP(ctx, status.BlockHash)
	if err != nil || len(bumpData) == 0 {
		return
	}
	status.MerklePath = extractMinimalPathForTx(bumpData, status.TxID)
}

// extractMinimalPathForTx extracts a per-tx minimal merkle path from a compound BUMP.
func extractMinimalPathForTx(bumpData []byte, txid string) []byte {
	compound, err := transaction.NewMerklePathFromBinary(bumpData)
	if err != nil {
		return nil
	}
	txHash, err := chainhash.NewHashFromHex(txid)
	if err != nil {
		return nil
	}

	var txOffset uint64
	found := false
	if len(compound.Path) > 0 {
		for _, leaf := range compound.Path[0] {
			if leaf.Hash != nil && *leaf.Hash == *txHash {
				txOffset = leaf.Offset
				found = true
				break
			}
		}
	}
	if !found {
		return nil
	}

	mp := &transaction.MerklePath{
		BlockHeight: compound.BlockHeight,
		Path:        make([][]*transaction.PathElement, len(compound.Path)),
	}
	offset := txOffset
	for level := 0; level < len(compound.Path); level++ {
		if level == 0 {
			for _, leaf := range compound.Path[level] {
				if leaf.Offset == offset {
					mp.Path[level] = append(mp.Path[level], leaf)
					break
				}
			}
		}
		sibOffset := offset ^ 1
		for _, leaf := range compound.Path[level] {
			if leaf.Offset == sibOffset {
				mp.Path[level] = append(mp.Path[level], leaf)
				break
			}
		}
		offset = offset >> 1
	}
	return mp.Bytes()
}

func recordToSubmission(rec *aero.Record) *models.Submission {
	sub := &models.Submission{}
	if v, ok := rec.Bins["submission_id"]; ok {
		if s, ok := v.(string); ok {
			sub.SubmissionID = s
		}
	}
	if v, ok := rec.Bins["txid"]; ok {
		if s, ok := v.(string); ok {
			sub.TxID = s
		}
	}
	if v, ok := rec.Bins["callback_url"]; ok {
		if s, ok := v.(string); ok {
			sub.CallbackURL = s
		}
	}
	if v, ok := rec.Bins["callback_token"]; ok {
		if s, ok := v.(string); ok {
			sub.CallbackToken = s
		}
	}
	if v, ok := rec.Bins["full_status_updates"]; ok {
		if b, ok := v.(bool); ok {
			sub.FullStatusUpdates = b
		}
	}
	if v, ok := rec.Bins["created_at"]; ok {
		if ms, ok := v.(int); ok {
			sub.CreatedAt = time.UnixMilli(int64(ms))
		}
	}
	if v, ok := rec.Bins["retry_count"]; ok {
		if n, ok := v.(int); ok {
			sub.RetryCount = n
		}
	}
	return sub
}

func getString(rec *aero.Record, bin string) string {
	if v, ok := rec.Bins[bin]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
