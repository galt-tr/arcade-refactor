package propagation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IBM/sarama"

	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/merkleservice"
	"github.com/bsv-blockchain/arcade/models"
	"github.com/bsv-blockchain/arcade/store"
	"github.com/bsv-blockchain/arcade/teranode"
	"go.uber.org/zap"
)

// eventLog is a thread-safe ordered list of string events for verifying call ordering.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (e *eventLog) add(event string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
}

func (e *eventLog) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := make([]string, len(e.events))
	copy(cp, e.events)
	return cp
}

func (e *eventLog) count(prefix string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.events {
		if strings.HasPrefix(ev, prefix) {
			n++
		}
	}
	return n
}

// mockStore implements store.Store with only UpdateStatus having real logic.
type mockStore struct {
	store.Store // embed interface — all unimplemented methods panic if called
	mu          sync.Mutex
	updates     []*models.TransactionStatus
}

func (m *mockStore) EnsureIndexes() error { return nil }

func (m *mockStore) UpdateStatus(_ context.Context, status *models.TransactionStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updates = append(m.updates, status)
	return nil
}

func (m *mockStore) updateCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.updates)
}

// helpers

func makePropMsg(txid string) []byte {
	msg := propagationMsg{
		TXID:  txid,
		RawTx: "deadbeef",
	}
	b, _ := json.Marshal(msg)
	return b
}

func consumerMsg(payload []byte) *sarama.ConsumerMessage {
	return &sarama.ConsumerMessage{Value: payload}
}

func newMerkleServer(log *eventLog, statusCode int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TxID string `json:"txid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		log.add("register:" + req.TxID)
		w.WriteHeader(statusCode)
	}))
}

func newTeranodeServer(log *eventLog, statusCode int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/txs" {
			log.add("broadcast-batch")
		} else {
			log.add("broadcast")
		}
		w.WriteHeader(statusCode)
	}))
}

func newPropagator(merkleSrvURL string, teranodeSrvURL string, st store.Store) *Propagator {
	cfg := &config.Config{
		CallbackURL: "http://localhost:8080/callback",
	}
	cfg.Propagation.MerkleConcurrency = 10

	var mc *merkleservice.Client
	if merkleSrvURL != "" {
		mc = merkleservice.NewClient(merkleSrvURL, "", 5*time.Second)
	}

	tc := teranode.NewClient([]string{teranodeSrvURL}, "")

	return New(cfg, zap.NewNop(), nil, st, tc, mc)
}

// handleAndFlush is a helper that adds a message and flushes (simulating consumer behavior)
func handleAndFlush(t *testing.T, p *Propagator, payload []byte) error {
	t.Helper()
	if err := p.handleMessage(context.Background(), consumerMsg(payload)); err != nil {
		return err
	}
	return p.flushBatch()
}

// Test 1: Registration happens before broadcast on success (single message)
func TestHandleMessage_RegistrationBeforeBroadcast(t *testing.T) {
	log := &eventLog{}
	ms := &mockStore{}

	merkleSrv := newMerkleServer(log, http.StatusOK)
	defer merkleSrv.Close()

	teranodeSrv := newTeranodeServer(log, http.StatusOK)
	defer teranodeSrv.Close()

	p := newPropagator(merkleSrv.URL, teranodeSrv.URL, ms)

	err := handleAndFlush(t, p, makePropMsg("abc123"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	events := log.all()
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events, got %d: %v", len(events), events)
	}
	if events[0] != "register:abc123" {
		t.Errorf("expected first event to be register, got: %s", events[0])
	}
	if events[1] != "broadcast" {
		t.Errorf("expected second event to be 'broadcast' (single /tx), got: %s", events[1])
	}

	if ms.updateCount() != 1 {
		t.Errorf("expected 1 UpdateStatus call, got %d", ms.updateCount())
	}

	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.updates[0].Status != models.StatusAcceptedByNetwork {
		t.Errorf("expected AcceptedByNetwork status, got %s", ms.updates[0].Status)
	}
}

// Test 2: Merkle failure returns error and prevents broadcast
func TestHandleMessage_MerkleFailure_NoBroadcast(t *testing.T) {
	log := &eventLog{}
	ms := &mockStore{}

	merkleSrv := newMerkleServer(log, http.StatusInternalServerError)
	defer merkleSrv.Close()

	teranodeSrv := newTeranodeServer(log, http.StatusOK)
	defer teranodeSrv.Close()

	p := newPropagator(merkleSrv.URL, teranodeSrv.URL, ms)

	err := handleAndFlush(t, p, makePropMsg("abc123"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "merkle-service batch registration failed") {
		t.Errorf("expected error to contain 'merkle-service batch registration failed', got: %v", err)
	}

	if log.count("broadcast") != 0 {
		t.Error("teranode should not have received any requests")
	}
	if ms.updateCount() != 0 {
		t.Error("store should not have received any UpdateStatus calls")
	}
}

// Test 3: Merkle timeout returns error and prevents broadcast
func TestHandleMessage_MerkleTimeout_NoBroadcast(t *testing.T) {
	log := &eventLog{}
	ms := &mockStore{}

	done := make(chan struct{})
	merkleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-done
	}))

	teranodeSrv := newTeranodeServer(log, http.StatusOK)
	defer teranodeSrv.Close()

	cfg := &config.Config{
		CallbackURL: "http://localhost:8080/callback",
	}
	cfg.Propagation.MerkleConcurrency = 10
	mc := merkleservice.NewClient(merkleSrv.URL, "", 100*time.Millisecond)
	tc := teranode.NewClient([]string{teranodeSrv.URL}, "")
	p := New(cfg, zap.NewNop(), nil, ms, tc, mc)

	err := handleAndFlush(t, p, makePropMsg("abc123"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if log.count("broadcast") != 0 {
		t.Error("teranode should not have received any requests")
	}

	close(done)
	merkleSrv.Close()
}

// Test 4: Batch — all 5 messages registered then broadcast in single call
func TestHandleMessage_BatchAllRegistered(t *testing.T) {
	log := &eventLog{}
	ms := &mockStore{}

	merkleSrv := newMerkleServer(log, http.StatusOK)
	defer merkleSrv.Close()

	teranodeSrv := newTeranodeServer(log, http.StatusOK)
	defer teranodeSrv.Close()

	p := newPropagator(merkleSrv.URL, teranodeSrv.URL, ms)

	// Accumulate 5 messages
	for i := 0; i < 5; i++ {
		txid := fmt.Sprintf("tx%d", i)
		err := p.handleMessage(context.Background(), consumerMsg(makePropMsg(txid)))
		if err != nil {
			t.Fatalf("message %d: expected no error, got: %v", i, err)
		}
	}

	// Flush the batch
	if err := p.flushBatch(); err != nil {
		t.Fatalf("flush error: %v", err)
	}

	if log.count("register:") != 5 {
		t.Errorf("expected 5 register events, got %d", log.count("register:"))
	}
	// Single batch POST to teranode /txs
	if log.count("broadcast-batch") != 1 {
		t.Errorf("expected 1 batch broadcast call, got %d", log.count("broadcast-batch"))
	}
	if ms.updateCount() != 5 {
		t.Errorf("expected 5 UpdateStatus calls, got %d", ms.updateCount())
	}
}

// Test 5: No merkle client — registration skipped, broadcast proceeds
func TestHandleMessage_NoMerkleClient_SkipsRegistration(t *testing.T) {
	log := &eventLog{}
	ms := &mockStore{}

	merkleSrv := newMerkleServer(log, http.StatusOK)
	defer merkleSrv.Close()

	teranodeSrv := newTeranodeServer(log, http.StatusOK)
	defer teranodeSrv.Close()

	// nil merkle client
	p := newPropagator("", teranodeSrv.URL, ms)

	err := handleAndFlush(t, p, makePropMsg("abc123"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if log.count("register:") != 0 {
		t.Error("merkle server should not have received any requests")
	}
	if log.count("broadcast") != 1 {
		t.Error("teranode should have received exactly 1 broadcast request")
	}
	if log.count("broadcast-batch") != 0 {
		t.Error("single tx should not use batch endpoint")
	}
}

// Test 6: No callback URL — registration skipped, broadcast proceeds
func TestHandleMessage_NoCallbackURL_SkipsRegistration(t *testing.T) {
	log := &eventLog{}
	ms := &mockStore{}

	merkleSrv := newMerkleServer(log, http.StatusOK)
	defer merkleSrv.Close()

	teranodeSrv := newTeranodeServer(log, http.StatusOK)
	defer teranodeSrv.Close()

	cfg := &config.Config{
		CallbackURL: "", // empty
	}
	cfg.Propagation.MerkleConcurrency = 10
	mc := merkleservice.NewClient(merkleSrv.URL, "", 5*time.Second)
	tc := teranode.NewClient([]string{teranodeSrv.URL}, "")
	p := New(cfg, zap.NewNop(), nil, ms, tc, mc)

	err := handleAndFlush(t, p, makePropMsg("abc123"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if log.count("register:") != 0 {
		t.Error("merkle server should not have received any requests")
	}
	if log.count("broadcast") != 1 {
		t.Error("teranode should have received exactly 1 broadcast request")
	}
	if log.count("broadcast-batch") != 0 {
		t.Error("single tx should not use batch endpoint")
	}
}

// Test 7: Batch of 100 — all registered then broadcast in single call
func TestProcessBatch_100Transactions(t *testing.T) {
	var registerCount atomic.Int32
	merkleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registerCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer merkleSrv.Close()

	var batchBroadcastCount atomic.Int32
	teranodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		batchBroadcastCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer teranodeSrv.Close()

	ms := &mockStore{}
	p := newPropagator(merkleSrv.URL, teranodeSrv.URL, ms)

	// Accumulate 100 messages
	for i := 0; i < 100; i++ {
		txid := fmt.Sprintf("tx%03d", i)
		err := p.handleMessage(context.Background(), consumerMsg(makePropMsg(txid)))
		if err != nil {
			t.Fatalf("message %d: expected no error, got: %v", i, err)
		}
	}

	// Flush
	if err := p.flushBatch(); err != nil {
		t.Fatalf("flush error: %v", err)
	}

	if registerCount.Load() != 100 {
		t.Errorf("expected 100 merkle registrations, got %d", registerCount.Load())
	}
	if batchBroadcastCount.Load() != 1 {
		t.Errorf("expected 1 batch broadcast call, got %d", batchBroadcastCount.Load())
	}
	if ms.updateCount() != 100 {
		t.Errorf("expected 100 UpdateStatus calls, got %d", ms.updateCount())
	}
}

// Test 8: Merkle failure aborts entire batch — no broadcast
func TestProcessBatch_MerkleFailure_AbortsBatch(t *testing.T) {
	var broadcastCount atomic.Int32
	merkleSrv := newMerkleServer(&eventLog{}, http.StatusInternalServerError)
	defer merkleSrv.Close()

	teranodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		broadcastCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer teranodeSrv.Close()

	ms := &mockStore{}
	p := newPropagator(merkleSrv.URL, teranodeSrv.URL, ms)

	for i := 0; i < 5; i++ {
		_ = p.handleMessage(context.Background(), consumerMsg(makePropMsg(fmt.Sprintf("tx%d", i))))
	}

	err := p.flushBatch()
	if err == nil {
		t.Fatal("expected error from merkle failure")
	}

	if broadcastCount.Load() != 0 {
		t.Errorf("expected 0 broadcast calls, got %d", broadcastCount.Load())
	}
	if ms.updateCount() != 0 {
		t.Errorf("expected 0 UpdateStatus calls, got %d", ms.updateCount())
	}
}

// Test 9: Nil merkle client skips registration for batch
func TestProcessBatch_NilMerkleClient_SkipsRegistration(t *testing.T) {
	log := &eventLog{}
	ms := &mockStore{}

	merkleSrv := newMerkleServer(log, http.StatusOK)
	defer merkleSrv.Close()

	teranodeSrv := newTeranodeServer(log, http.StatusOK)
	defer teranodeSrv.Close()

	// nil merkle client
	p := newPropagator("", teranodeSrv.URL, ms)

	for i := 0; i < 5; i++ {
		_ = p.handleMessage(context.Background(), consumerMsg(makePropMsg(fmt.Sprintf("tx%d", i))))
	}

	if err := p.flushBatch(); err != nil {
		t.Fatalf("flush error: %v", err)
	}

	if log.count("register:") != 0 {
		t.Error("merkle server should not have been called")
	}
	if ms.updateCount() != 5 {
		t.Errorf("expected 5 UpdateStatus calls, got %d", ms.updateCount())
	}
}

// Test 10: Single transaction uses /tx endpoint, not /txs
func TestSingleTransaction_UsesTxEndpoint(t *testing.T) {
	log := &eventLog{}
	ms := &mockStore{}

	teranodeSrv := newTeranodeServer(log, http.StatusOK)
	defer teranodeSrv.Close()

	p := newPropagator("", teranodeSrv.URL, ms)

	err := handleAndFlush(t, p, makePropMsg("single-tx"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if log.count("broadcast") != 1 {
		t.Errorf("expected 1 broadcast event, got %d", log.count("broadcast"))
	}
	if log.count("broadcast-batch") != 0 {
		t.Error("single tx should hit /tx, not /txs")
	}
}

// Test 11: Batch transactions use /txs endpoint, not /tx
func TestBatchTransactions_UsesTxsEndpoint(t *testing.T) {
	log := &eventLog{}
	ms := &mockStore{}

	teranodeSrv := newTeranodeServer(log, http.StatusOK)
	defer teranodeSrv.Close()

	p := newPropagator("", teranodeSrv.URL, ms)

	for i := 0; i < 3; i++ {
		_ = p.handleMessage(context.Background(), consumerMsg(makePropMsg(fmt.Sprintf("tx%d", i))))
	}

	if err := p.flushBatch(); err != nil {
		t.Fatalf("flush error: %v", err)
	}

	if log.count("broadcast-batch") != 1 {
		t.Errorf("expected 1 batch broadcast, got %d", log.count("broadcast-batch"))
	}
	// Verify no single-tx broadcasts occurred
	events := log.all()
	for _, ev := range events {
		if ev == "broadcast" {
			t.Error("batch should not hit /tx single endpoint")
		}
	}
}

// Test 12: Single transaction 200 → AcceptedByNetwork
func TestSingleTransaction_Status200_AcceptedByNetwork(t *testing.T) {
	ms := &mockStore{}

	teranodeSrv := newTeranodeServer(&eventLog{}, http.StatusOK)
	defer teranodeSrv.Close()

	p := newPropagator("", teranodeSrv.URL, ms)

	err := handleAndFlush(t, p, makePropMsg("tx-200"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if ms.updateCount() != 1 {
		t.Fatalf("expected 1 UpdateStatus call, got %d", ms.updateCount())
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.updates[0].Status != models.StatusAcceptedByNetwork {
		t.Errorf("expected AcceptedByNetwork, got %s", ms.updates[0].Status)
	}
}

// Test 13: Single transaction 202 → no status update (matching original behavior)
func TestSingleTransaction_Status202_NoStatusUpdate(t *testing.T) {
	ms := &mockStore{}

	teranodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer teranodeSrv.Close()

	p := newPropagator("", teranodeSrv.URL, ms)

	err := handleAndFlush(t, p, makePropMsg("tx-202"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if ms.updateCount() != 0 {
		t.Errorf("expected 0 UpdateStatus calls for 202 response, got %d", ms.updateCount())
	}
}

// Test 14: Batch — any endpoint success → AcceptedByNetwork for all
func TestBatchTransactions_AnySuccess_AcceptedByNetwork(t *testing.T) {
	ms := &mockStore{}

	// First endpoint fails, second succeeds
	callCount := atomic.Int32{}
	teranodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer teranodeSrv.Close()

	cfg := &config.Config{}
	cfg.Propagation.MerkleConcurrency = 10
	// Two endpoints pointing to the same server (simulates multi-endpoint)
	tc := teranode.NewClient([]string{teranodeSrv.URL, teranodeSrv.URL}, "")
	p := New(cfg, zap.NewNop(), nil, ms, tc, nil)

	for i := 0; i < 3; i++ {
		_ = p.handleMessage(context.Background(), consumerMsg(makePropMsg(fmt.Sprintf("tx%d", i))))
	}

	if err := p.flushBatch(); err != nil {
		t.Fatalf("flush error: %v", err)
	}

	if ms.updateCount() != 3 {
		t.Fatalf("expected 3 UpdateStatus calls, got %d", ms.updateCount())
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	for i, u := range ms.updates {
		if u.Status != models.StatusAcceptedByNetwork {
			t.Errorf("tx %d: expected AcceptedByNetwork, got %s", i, u.Status)
		}
	}
}

// Test 15: Batch — all endpoints fail → Rejected for all
func TestBatchTransactions_AllFail_Rejected(t *testing.T) {
	ms := &mockStore{}

	teranodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer teranodeSrv.Close()

	p := newPropagator("", teranodeSrv.URL, ms)

	for i := 0; i < 3; i++ {
		_ = p.handleMessage(context.Background(), consumerMsg(makePropMsg(fmt.Sprintf("tx%d", i))))
	}

	if err := p.flushBatch(); err != nil {
		t.Fatalf("flush error: %v", err)
	}

	if ms.updateCount() != 3 {
		t.Fatalf("expected 3 UpdateStatus calls, got %d", ms.updateCount())
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	for i, u := range ms.updates {
		if u.Status != models.StatusRejected {
			t.Errorf("tx %d: expected Rejected, got %s", i, u.Status)
		}
	}
}

