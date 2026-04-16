package kafka

import (
	"errors"
	"testing"

	"github.com/IBM/sarama"
)

// mockSyncProducer implements sarama.SyncProducer for testing.
type mockSyncProducer struct {
	sendMessageFn  func(msg *sarama.ProducerMessage) (int32, int64, error)
	sendMessagesFn func(msgs []*sarama.ProducerMessage) error
	messages       []*sarama.ProducerMessage
	batchCalls     int
}

func (m *mockSyncProducer) SendMessage(msg *sarama.ProducerMessage) (int32, int64, error) {
	m.messages = append(m.messages, msg)
	if m.sendMessageFn != nil {
		return m.sendMessageFn(msg)
	}
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

func newTestProducer(sp *mockSyncProducer) *Producer {
	return &Producer{syncProducer: sp}
}

func TestSendBatch_Success(t *testing.T) {
	mock := &mockSyncProducer{}
	p := newTestProducer(mock)

	msgs := []KeyValue{
		{Key: "tx1", Value: map[string]string{"txid": "tx1"}},
		{Key: "tx2", Value: map[string]string{"txid": "tx2"}},
		{Key: "tx3", Value: map[string]string{"txid": "tx3"}},
	}

	err := p.SendBatch("test-topic", msgs)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if mock.batchCalls != 1 {
		t.Errorf("expected 1 batch call, got %d", mock.batchCalls)
	}
	if len(mock.messages) != 3 {
		t.Errorf("expected 3 messages, got %d", len(mock.messages))
	}
	for _, m := range mock.messages {
		if m.Topic != "test-topic" {
			t.Errorf("expected topic test-topic, got %s", m.Topic)
		}
	}
}

func TestSendBatch_EmptyReturnsNil(t *testing.T) {
	mock := &mockSyncProducer{}
	p := newTestProducer(mock)

	err := p.SendBatch("test-topic", nil)
	if err != nil {
		t.Fatalf("expected no error for empty batch, got: %v", err)
	}

	if mock.batchCalls != 0 {
		t.Errorf("expected 0 batch calls for empty batch, got %d", mock.batchCalls)
	}
}

func TestSendBatch_ErrorPropagation(t *testing.T) {
	mock := &mockSyncProducer{
		sendMessagesFn: func(msgs []*sarama.ProducerMessage) error {
			return errors.New("broker unavailable")
		},
	}
	p := newTestProducer(mock)

	msgs := []KeyValue{
		{Key: "tx1", Value: map[string]string{"txid": "tx1"}},
	}

	err := p.SendBatch("test-topic", msgs)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, errors.Unwrap(err)) && err.Error() == "" {
		t.Error("expected non-empty error message")
	}
}
