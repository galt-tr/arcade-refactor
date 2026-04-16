package api_server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/kafka"
	"github.com/bsv-blockchain/arcade/models"
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
type mockStore struct {
	updateStatusCalls []*models.TransactionStatus
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
func (m *mockStore) IsBlockOnChain(context.Context, string) (bool, error)    { return false, nil }
func (m *mockStore) MarkBlockProcessed(context.Context, string, uint64, bool) error { return nil }
func (m *mockStore) HasAnyProcessedBlocks(context.Context) (bool, error)     { return false, nil }
func (m *mockStore) GetOnChainBlockAtHeight(context.Context, uint64) (string, bool, error) {
	return "", false, nil
}
func (m *mockStore) MarkBlockOffChain(context.Context, string) error         { return nil }
func (m *mockStore) InsertStump(context.Context, *models.Stump) error        { return m.insertStumpErr }
func (m *mockStore) GetStumpsByBlockHash(context.Context, string) ([]*models.Stump, error) {
	return nil, nil
}
func (m *mockStore) DeleteStumpsByBlockHash(context.Context, string) error { return nil }
func (m *mockStore) EnsureIndexes() error                                  { return nil }
func (m *mockStore) Close() error                                         { return nil }

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

func TestHandleCallback_Stump_StorageError_DoesNotPublishToKafka(t *testing.T) {
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

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// No messages should have been published to Kafka since storage failed
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
