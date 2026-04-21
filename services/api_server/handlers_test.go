package api_server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/kafka"
	"github.com/bsv-blockchain/arcade/models"
	"github.com/bsv-blockchain/arcade/store"
	sdkTx "github.com/bsv-blockchain/go-sdk/transaction"
)

// mockSyncProducer implements sarama.SyncProducer for testing.
type mockSyncProducer struct {
	messages       []*sarama.ProducerMessage
	batchCalls     int
	sendMessagesFn func(msgs []*sarama.ProducerMessage) error
}

func (m *mockSyncProducer) SendMessage(msg *sarama.ProducerMessage) (int32, int64, error) {
	m.messages = append(m.messages, msg)
	return 0, 0, nil
}
func (m *mockSyncProducer) SendMessages(msgs []*sarama.ProducerMessage) error {
	m.batchCalls++
	m.messages = append(m.messages, msgs...)
	if m.sendMessagesFn != nil {
		return m.sendMessagesFn(msgs)
	}
	return nil
}
func (m *mockSyncProducer) Close() error                            { return nil }
func (m *mockSyncProducer) IsTransactional() bool                   { return false }
func (m *mockSyncProducer) BeginTxn() error                         { return nil }
func (m *mockSyncProducer) CommitTxn() error                        { return nil }
func (m *mockSyncProducer) AbortTxn() error                         { return nil }
func (m *mockSyncProducer) AddOffsetsToTxn(map[string][]*sarama.PartitionOffsetMetadata, string) error {
	return nil
}
func (m *mockSyncProducer) AddMessageToTxn(*sarama.ConsumerMessage, string, *string) error {
	return nil
}
func (m *mockSyncProducer) TxnStatus() sarama.ProducerTxnStatusFlag { return 0 }

// mockStore implements store.Store for testing callback handlers.
//
// InsertStump records calls under a composite "blockHash:subtreeIndex" key so
// tests can verify the full round-trip of a STUMP payload (including hex
// decoding of models.HexBytes). A mutex protects the stumps map because the
// end-to-end STUMP test fires deliveries concurrently to mirror
// merkle-service's 64-worker delivery pool.
type mockStore struct {
	mu                sync.Mutex
	updateStatusCalls []*models.TransactionStatus
	stumps            map[string]*models.Stump
	insertStumpErr    error
}

func (m *mockStore) UpdateStatus(_ context.Context, status *models.TransactionStatus) error {
	m.updateStatusCalls = append(m.updateStatusCalls, status)
	return nil
}

func (m *mockStore) GetOrInsertStatus(context.Context, *models.TransactionStatus) (*models.TransactionStatus, bool, error) {
	return nil, false, nil
}
func (m *mockStore) GetStatus(context.Context, string) (*models.TransactionStatus, error) {
	return nil, nil
}
func (m *mockStore) GetStatusesSince(context.Context, time.Time) ([]*models.TransactionStatus, error) {
	return nil, nil
}
func (m *mockStore) SetStatusByBlockHash(context.Context, string, models.Status) ([]string, error) {
	return nil, nil
}
func (m *mockStore) InsertBUMP(context.Context, string, uint64, []byte) error { return nil }
func (m *mockStore) GetBUMP(context.Context, string) (uint64, []byte, error)  { return 0, nil, nil }
func (m *mockStore) SetMinedByTxIDs(context.Context, string, []string) ([]*models.TransactionStatus, error) {
	return nil, nil
}
func (m *mockStore) InsertSubmission(context.Context, *models.Submission) error { return nil }
func (m *mockStore) GetSubmissionsByTxID(context.Context, string) ([]*models.Submission, error) {
	return nil, nil
}
func (m *mockStore) GetSubmissionsByToken(context.Context, string) ([]*models.Submission, error) {
	return nil, nil
}
func (m *mockStore) UpdateDeliveryStatus(context.Context, string, models.Status, int, *time.Time) error {
	return nil
}
func (m *mockStore) InsertStump(_ context.Context, stump *models.Stump) error {
	if m.insertStumpErr != nil {
		return m.insertStumpErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stumps == nil {
		m.stumps = make(map[string]*models.Stump)
	}
	// Copy the payload so later caller mutation can't race with assertions.
	dataCopy := append([]byte(nil), stump.StumpData...)
	m.stumps[fmt.Sprintf("%s:%d", stump.BlockHash, stump.SubtreeIndex)] = &models.Stump{
		BlockHash:    stump.BlockHash,
		SubtreeIndex: stump.SubtreeIndex,
		StumpData:    dataCopy,
	}
	return nil
}
func (m *mockStore) GetStumpsByBlockHash(context.Context, string) ([]*models.Stump, error) {
	return nil, nil
}
func (m *mockStore) DeleteStumpsByBlockHash(context.Context, string) error { return nil }
func (m *mockStore) BumpRetryCount(context.Context, string) (int, error) { return 0, nil }
func (m *mockStore) SetPendingRetryFields(context.Context, string, []byte, time.Time) error {
	return nil
}
func (m *mockStore) GetReadyRetries(context.Context, time.Time, int) ([]*store.PendingRetry, error) {
	return nil, nil
}
func (m *mockStore) ClearRetryState(context.Context, string, models.Status, string) error {
	return nil
}
func (m *mockStore) EnsureIndexes() error { return nil }
func (m *mockStore) Close() error         { return nil }

func makeMinimalTx() []byte {
	tx := sdkTx.NewTransaction()
	return tx.Bytes()
}

func setupServerWithStore(mock *mockSyncProducer, ms *mockStore) (*Server, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	producer := kafka.NewProducerWithSync(mock)
	srv := &Server{
		cfg:      &config.Config{},
		logger:   zap.NewNop(),
		producer: producer,
		store:    ms,
	}
	router := gin.New()
	srv.registerRoutes(router)
	return srv, router
}

func setupServer(mock *mockSyncProducer) (*Server, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	producer := kafka.NewProducerWithSync(mock)
	srv := &Server{
		cfg:      &config.Config{},
		logger:   zap.NewNop(),
		producer: producer,
	}
	router := gin.New()
	srv.registerRoutes(router)
	return srv, router
}

func TestHandleSubmitTransactions_BatchPublish(t *testing.T) {
	mock := &mockSyncProducer{}
	_, router := setupServer(mock)

	// Concatenate 3 minimal transactions
	txBytes := makeMinimalTx()
	body := bytes.Repeat(txBytes, 3)

	req := httptest.NewRequest(http.MethodPost, "/txs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if int(resp["submitted"].(float64)) != 3 {
		t.Errorf("expected submitted=3, got %v", resp["submitted"])
	}

	if mock.batchCalls != 1 {
		t.Errorf("expected 1 batch call, got %d", mock.batchCalls)
	}
	if len(mock.messages) != 3 {
		t.Errorf("expected 3 messages, got %d", len(mock.messages))
	}
}

func TestHandleSubmitTransactions_ParseFailure_NoPublish(t *testing.T) {
	mock := &mockSyncProducer{}
	_, router := setupServer(mock)

	// Valid tx followed by garbage
	body := append(makeMinimalTx(), 0xff, 0xfe, 0xfd)

	req := httptest.NewRequest(http.MethodPost, "/txs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	if mock.batchCalls != 0 {
		t.Errorf("expected 0 batch calls on parse failure, got %d", mock.batchCalls)
	}
}

func TestHandleSubmitTransactions_100Txs_SingleBatchCall(t *testing.T) {
	mock := &mockSyncProducer{}
	_, router := setupServer(mock)

	// Concatenate 100 minimal transactions
	txBytes := makeMinimalTx()
	body := bytes.Repeat(txBytes, 100)

	req := httptest.NewRequest(http.MethodPost, "/txs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if int(resp["submitted"].(float64)) != 100 {
		t.Errorf("expected submitted=100, got %v", resp["submitted"])
	}

	if mock.batchCalls != 1 {
		t.Errorf("expected exactly 1 batch call for 100 txs, got %d", mock.batchCalls)
	}
	if len(mock.messages) != 100 {
		t.Errorf("expected 100 messages in batch, got %d", len(mock.messages))
	}

	// Verify all messages go to the transaction topic
	for i, msg := range mock.messages {
		if msg.Topic != "arcade.transaction" {
			t.Errorf("message %d: expected topic arcade.transaction, got %s", i, msg.Topic)
		}
	}
}

func TestHandleSubmitTransactions_KafkaFailure_Returns500(t *testing.T) {
	mock := &mockSyncProducer{
		sendMessagesFn: func(msgs []*sarama.ProducerMessage) error {
			return errors.New("broker down")
		},
	}
	_, router := setupServer(mock)

	body := makeMinimalTx()

	req := httptest.NewRequest(http.MethodPost, "/txs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleCallback_SeenMultipleNodes_UpdatesStatus(t *testing.T) {
	ms := &mockStore{}
	_, router := setupServerWithStore(&mockSyncProducer{}, ms)

	payload := models.CallbackMessage{
		Type:  models.CallbackSeenMultipleNodes,
		TxIDs: []string{"tx1", "tx2"},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/merkle-service/callback", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if len(ms.updateStatusCalls) != 2 {
		t.Fatalf("expected 2 UpdateStatus calls, got %d", len(ms.updateStatusCalls))
	}
	for i, call := range ms.updateStatusCalls {
		if call.Status != models.StatusSeenMultipleNodes {
			t.Errorf("call %d: expected status %s, got %s", i, models.StatusSeenMultipleNodes, call.Status)
		}
	}
	if ms.updateStatusCalls[0].TxID != "tx1" {
		t.Errorf("expected first txid=tx1, got %s", ms.updateStatusCalls[0].TxID)
	}
	if ms.updateStatusCalls[1].TxID != "tx2" {
		t.Errorf("expected second txid=tx2, got %s", ms.updateStatusCalls[1].TxID)
	}
}

func TestHandleCallback_Stump_StorageError_Returns500(t *testing.T) {
	// When STUMP storage fails, we MUST return 5xx so merkle-service retries.
	// Swallowing the error with a 200 breaks the bump_builder's invariant that
	// every STUMP in a BLOCK_PROCESSED block is durably stored in Aerospike.
	mock := &mockSyncProducer{}
	ms := &mockStore{insertStumpErr: errors.New("SERVER_MEM_ERROR")}
	_, router := setupServerWithStore(mock, ms)

	payload := models.CallbackMessage{
		Type:      models.CallbackStump,
		BlockHash: "abc123",
		Stump:     []byte{0x01, 0x02},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/merkle-service/callback", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if len(mock.messages) != 0 {
		t.Errorf("expected 0 Kafka messages after storage failure, got %d", len(mock.messages))
	}
}

func TestHandleCallback_SeenMultipleNodes_EmptyTxIDs(t *testing.T) {
	ms := &mockStore{}
	_, router := setupServerWithStore(&mockSyncProducer{}, ms)

	payload := models.CallbackMessage{
		Type:  models.CallbackSeenMultipleNodes,
		TxIDs: []string{},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/merkle-service/callback", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if len(ms.updateStatusCalls) != 0 {
		t.Errorf("expected 0 UpdateStatus calls, got %d", len(ms.updateStatusCalls))
	}
}

// TestHandleCallback_FullBlockFlow_20Subtrees simulates the production delivery
// pattern that merkle-service executes for a 20,000-tx block split across 20
// subtrees:
//
//  1. Twenty STUMP callbacks (one per subtreeIndex 0..19) POSTed to
//     /api/v1/merkle-service/callback, each carrying a realistic ~8 KB payload
//     hex-encoded via models.HexBytes. merkle-service fires these in parallel
//     from a 64-worker delivery pool (merkle-service/internal/callback/delivery.go),
//     so we dispatch them concurrently here.
//  2. A single BLOCK_PROCESSED callback with just the block hash, which
//     merkle-service's stumpGate only releases AFTER every STUMP has returned
//     2xx (merkle-service/internal/callback/delivery.go stumpGate.Wait).
//
// The test asserts end-to-end correctness of the code path that production is
// returning 500s from:
//
//   - every STUMP returns 200
//   - all 20 STUMPs land in the store with the correct composite key and
//     byte-identical payload (hex round-trip through models.HexBytes)
//   - Kafka is not touched for STUMPs (they go to the store only)
//   - BLOCK_PROCESSED produces exactly one Kafka message on
//     arcade.block_processed, keyed by block hash, with the full
//     CallbackMessage JSON as the value
//   - retry semantics: a duplicated STUMP delivery still returns 200 and
//     overwrites cleanly, because merkle-service retries on any non-2xx
func TestHandleCallback_FullBlockFlow_20Subtrees(t *testing.T) {
	const (
		numSubtrees = 20
		stumpSize   = 8 * 1024 // ~8 KB — realistic for a subtree covering ~1000 txs (BRC-0074 BUMP format)
	)
	blockHash := "000000000000000000001234567890abcdef1234567890abcdef1234567890ab"

	// Deterministic per-subtree payload so byte-level assertions are stable.
	stumpPayloads := make([][]byte, numSubtrees)
	for i := range stumpPayloads {
		buf := make([]byte, stumpSize)
		for j := range buf {
			buf[j] = byte((i*31 + j) & 0xFF)
		}
		stumpPayloads[i] = buf
	}

	mock := &mockSyncProducer{}
	ms := &mockStore{}
	_, router := setupServerWithStore(mock, ms)

	// Phase 1: fire all 20 STUMPs in parallel.
	var wg sync.WaitGroup
	errCh := make(chan error, numSubtrees)
	for i := 0; i < numSubtrees; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := models.CallbackMessage{
				Type:         models.CallbackStump,
				BlockHash:    blockHash,
				SubtreeIndex: i,
				Stump:        stumpPayloads[i],
			}
			body, err := json.Marshal(payload)
			if err != nil {
				errCh <- fmt.Errorf("marshal subtree %d: %w", i, err)
				return
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/merkle-service/callback", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			// Match merkle-service's delivery headers exactly.
			req.Header.Set("X-Idempotency-Key", fmt.Sprintf("%s:%d:STUMP", blockHash, i))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				errCh <- fmt.Errorf("STUMP subtree %d: expected 200, got %d: %s", i, w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	// All 20 STUMPs must be in the store, bit-identical to what was sent.
	ms.mu.Lock()
	stored := len(ms.stumps)
	ms.mu.Unlock()
	if stored != numSubtrees {
		t.Fatalf("expected %d STUMPs stored, got %d", numSubtrees, stored)
	}
	for i := 0; i < numSubtrees; i++ {
		key := fmt.Sprintf("%s:%d", blockHash, i)
		ms.mu.Lock()
		stump, ok := ms.stumps[key]
		ms.mu.Unlock()
		if !ok {
			t.Errorf("missing stump for subtree %d (key=%q)", i, key)
			continue
		}
		if stump.BlockHash != blockHash {
			t.Errorf("subtree %d: blockHash = %q, want %q", i, stump.BlockHash, blockHash)
		}
		if stump.SubtreeIndex != i {
			t.Errorf("subtree %d: SubtreeIndex = %d, want %d", i, stump.SubtreeIndex, i)
		}
		if !bytes.Equal(stump.StumpData, stumpPayloads[i]) {
			t.Errorf("subtree %d: stump bytes differ after hex round-trip (got %d bytes, want %d)",
				i, len(stump.StumpData), len(stumpPayloads[i]))
		}
	}

	// STUMPs must not produce Kafka traffic — the bump_builder consumes
	// arcade.block_processed only, and STUMPs are writes to the store.
	if len(mock.messages) != 0 {
		t.Fatalf("expected 0 Kafka messages after STUMP phase, got %d", len(mock.messages))
	}

	// Phase 2: BLOCK_PROCESSED. Carries only the block hash; merkle-service
	// does not resend the stump bytes here.
	blockMsg := models.CallbackMessage{
		Type:      models.CallbackBlockProcessed,
		BlockHash: blockHash,
	}
	body, err := json.Marshal(blockMsg)
	if err != nil {
		t.Fatalf("marshal BLOCK_PROCESSED: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/merkle-service/callback", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Idempotency-Key", blockHash+":BLOCK_PROCESSED")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("BLOCK_PROCESSED: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Exactly one Kafka message on arcade.block_processed, keyed by block
	// hash, with the full CallbackMessage JSON as the value — this is what
	// the bump_builder consumer expects (services/bump_builder/builder.go).
	if len(mock.messages) != 1 {
		t.Fatalf("expected 1 Kafka message after BLOCK_PROCESSED, got %d", len(mock.messages))
	}
	kmsg := mock.messages[0]
	if kmsg.Topic != kafka.TopicBlockProcessed {
		t.Errorf("Kafka topic = %q, want %q", kmsg.Topic, kafka.TopicBlockProcessed)
	}
	gotKey, err := kmsg.Key.Encode()
	if err != nil {
		t.Fatalf("encode kafka key: %v", err)
	}
	if string(gotKey) != blockHash {
		t.Errorf("Kafka key = %q, want %q", gotKey, blockHash)
	}
	val, err := kmsg.Value.Encode()
	if err != nil {
		t.Fatalf("encode kafka value: %v", err)
	}
	var decoded models.CallbackMessage
	if err := json.Unmarshal(val, &decoded); err != nil {
		t.Fatalf("unmarshal kafka value: %v", err)
	}
	if decoded.Type != models.CallbackBlockProcessed {
		t.Errorf("kafka value Type = %q, want %q", decoded.Type, models.CallbackBlockProcessed)
	}
	if decoded.BlockHash != blockHash {
		t.Errorf("kafka value BlockHash = %q, want %q", decoded.BlockHash, blockHash)
	}

	// Phase 3: idempotency. merkle-service retries on any non-2xx with
	// linear backoff, so a second delivery of subtree 0 must still return
	// 200. Our store is upsert-on-(blockHash,subtreeIndex) so the count
	// stays at 20.
	retryPayload := models.CallbackMessage{
		Type:         models.CallbackStump,
		BlockHash:    blockHash,
		SubtreeIndex: 0,
		Stump:        stumpPayloads[0],
	}
	retryBody, _ := json.Marshal(retryPayload)
	retryReq := httptest.NewRequest(http.MethodPost, "/api/v1/merkle-service/callback", bytes.NewReader(retryBody))
	retryReq.Header.Set("Content-Type", "application/json")
	retryW := httptest.NewRecorder()
	router.ServeHTTP(retryW, retryReq)
	if retryW.Code != http.StatusOK {
		t.Fatalf("duplicate STUMP: expected 200, got %d: %s", retryW.Code, retryW.Body.String())
	}
	ms.mu.Lock()
	finalCount := len(ms.stumps)
	ms.mu.Unlock()
	if finalCount != numSubtrees {
		t.Errorf("expected stump count to stay at %d after duplicate, got %d", numSubtrees, finalCount)
	}
}
