package tx_validator

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/IBM/sarama"
	sdkTx "github.com/bsv-blockchain/go-sdk/transaction"
	"go.uber.org/zap"

	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/kafka"
	"github.com/bsv-blockchain/arcade/models"
	"github.com/bsv-blockchain/arcade/store"
)

// mockSyncProducer implements sarama.SyncProducer for testing.
type mockSyncProducer struct {
	mu             sync.Mutex
	messages       []*sarama.ProducerMessage
	batchCalls     int
	sendMessagesFn func(msgs []*sarama.ProducerMessage) error
}

func (m *mockSyncProducer) SendMessage(msg *sarama.ProducerMessage) (int32, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, msg)
	return 0, 0, nil
}
func (m *mockSyncProducer) SendMessages(msgs []*sarama.ProducerMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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

// mockStore implements store.Store for testing.
type mockStore struct {
	store.Store
	mu      sync.Mutex
	updates []*models.TransactionStatus
	inserts []*models.TransactionStatus
}

func (m *mockStore) EnsureIndexes() error { return nil }

func (m *mockStore) GetOrInsertStatus(_ context.Context, status *models.TransactionStatus) (*models.TransactionStatus, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inserts = append(m.inserts, status)
	return &models.TransactionStatus{TxID: status.TxID, Status: models.StatusReceived, Timestamp: time.Now()}, true, nil
}

func (m *mockStore) UpdateStatus(_ context.Context, status *models.TransactionStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updates = append(m.updates, status)
	return nil
}

func makeValidTxHex() string {
	tx := sdkTx.NewTransaction()
	return hex.EncodeToString(tx.Bytes())
}

func makeTxMsg(rawTxHex string) []byte {
	msg := txMessage{Action: "submit", RawTx: rawTxHex}
	b, _ := json.Marshal(msg)
	return b
}

func consumerMsg(payload []byte) *sarama.ConsumerMessage {
	return &sarama.ConsumerMessage{Value: payload}
}

func newTestValidator(mock *mockSyncProducer, ms *mockStore) *Validator {
	producer := kafka.NewProducerWithSync(mock)
	cfg := &config.Config{}
	tracker := store.NewTxTracker()
	return New(cfg, zap.NewNop(), producer, ms, tracker, nil)
}

func TestValidatorBatch_AccumulateAndFlush(t *testing.T) {
	mock := &mockSyncProducer{}
	ms := &mockStore{}
	v := newTestValidator(mock, ms)

	txHex := makeValidTxHex()

	// Process 100 messages — they accumulate (consumer drains channel first)
	for i := 0; i < 100; i++ {
		err := v.handleMessage(context.Background(), consumerMsg(makeTxMsg(txHex)))
		if err != nil {
			t.Fatalf("message %d: unexpected error: %v", i, err)
		}
	}

	// Nothing published yet — consumer hasn't called flush
	mock.mu.Lock()
	if mock.batchCalls != 0 {
		t.Errorf("expected 0 batch calls before flush, got %d", mock.batchCalls)
	}
	mock.mu.Unlock()

	// Flush (as the consumer would after draining the channel)
	if err := v.flushPropagation(); err != nil {
		t.Fatalf("flush error: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.batchCalls != 1 {
		t.Errorf("expected 1 batch call after flush, got %d", mock.batchCalls)
	}
	// All 100 messages are duplicates (same tx), but the first one should be new
	// Actually, only the first insert returns isNew=true (our mock always returns true),
	// so all 100 should produce propagation messages
	if len(mock.messages) != 100 {
		t.Errorf("expected 100 messages, got %d", len(mock.messages))
	}
}

func TestValidatorBatch_FlushIsNoOpWhenEmpty(t *testing.T) {
	mock := &mockSyncProducer{}
	ms := &mockStore{}
	v := newTestValidator(mock, ms)

	// Flush with no pending messages should be a no-op
	if err := v.flushPropagation(); err != nil {
		t.Fatalf("flush error: %v", err)
	}
	mock.mu.Lock()
	if mock.batchCalls != 0 {
		t.Errorf("expected 0 batch calls for empty flush, got %d", mock.batchCalls)
	}
	mock.mu.Unlock()
}

func TestValidatorBatch_SingleTxWorks(t *testing.T) {
	mock := &mockSyncProducer{}
	ms := &mockStore{}
	v := newTestValidator(mock, ms)

	txHex := makeValidTxHex()

	err := v.handleMessage(context.Background(), consumerMsg(makeTxMsg(txHex)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Flush single message
	if err := v.flushPropagation(); err != nil {
		t.Fatalf("flush error: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.batchCalls != 1 {
		t.Errorf("expected 1 batch call, got %d", mock.batchCalls)
	}
	if len(mock.messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(mock.messages))
	}
}
