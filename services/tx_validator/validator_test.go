package tx_validator

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"
	"time"

	sdkTx "github.com/bsv-blockchain/go-sdk/transaction"
	"go.uber.org/zap"

	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/kafka"
	"github.com/bsv-blockchain/arcade/models"
	"github.com/bsv-blockchain/arcade/store"
)

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

func makeKafkaMsg(payload []byte) *kafka.Message {
	return &kafka.Message{Value: payload}
}

func newTestValidator(broker *kafka.RecordingBroker, ms *mockStore) *Validator {
	producer := kafka.NewProducer(broker)
	cfg := &config.Config{}
	tracker := store.NewTxTracker()
	return New(cfg, zap.NewNop(), producer, ms, tracker, nil)
}

func TestValidatorBatch_AccumulateAndFlush(t *testing.T) {
	broker := &kafka.RecordingBroker{}
	ms := &mockStore{}
	v := newTestValidator(broker, ms)

	txHex := makeValidTxHex()

	// Process 100 messages — they accumulate (consumer drains channel first)
	for i := 0; i < 100; i++ {
		err := v.handleMessage(context.Background(), makeKafkaMsg(makeTxMsg(txHex)))
		if err != nil {
			t.Fatalf("message %d: unexpected error: %v", i, err)
		}
	}

	// Nothing published yet — consumer hasn't called flush
	if broker.BatchCalls != 0 {
		t.Errorf("expected 0 batch calls before flush, got %d", broker.BatchCalls)
	}

	// Flush (as the consumer would after draining the channel)
	if err := v.flushPropagation(context.Background()); err != nil {
		t.Fatalf("flush error: %v", err)
	}

	if broker.BatchCalls != 1 {
		t.Errorf("expected 1 batch call after flush, got %d", broker.BatchCalls)
	}
	if got := broker.MessageCount(); got != 100 {
		t.Errorf("expected 100 messages, got %d", got)
	}
}

func TestValidatorBatch_FlushIsNoOpWhenEmpty(t *testing.T) {
	broker := &kafka.RecordingBroker{}
	ms := &mockStore{}
	v := newTestValidator(broker, ms)

	// Flush with no pending messages should be a no-op
	if err := v.flushPropagation(context.Background()); err != nil {
		t.Fatalf("flush error: %v", err)
	}
	if broker.BatchCalls != 0 {
		t.Errorf("expected 0 batch calls for empty flush, got %d", broker.BatchCalls)
	}
}

func TestValidatorBatch_SingleTxWorks(t *testing.T) {
	broker := &kafka.RecordingBroker{}
	ms := &mockStore{}
	v := newTestValidator(broker, ms)

	txHex := makeValidTxHex()

	err := v.handleMessage(context.Background(), makeKafkaMsg(makeTxMsg(txHex)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Flush single message
	if err := v.flushPropagation(context.Background()); err != nil {
		t.Fatalf("flush error: %v", err)
	}

	if broker.BatchCalls != 1 {
		t.Errorf("expected 1 batch call, got %d", broker.BatchCalls)
	}
	if got := broker.MessageCount(); got != 1 {
		t.Errorf("expected 1 message, got %d", got)
	}
}
