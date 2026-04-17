package bump_builder

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"go.uber.org/zap"

	"github.com/bsv-blockchain/arcade/bump"
	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/models"
	"github.com/bsv-blockchain/arcade/store"
)

// --- Mock Store ---

type mockStore struct {
	store.Store // embed — panics on unimplemented methods

	mu              sync.Mutex
	stumps          map[string][]*models.Stump // blockHash → stumps
	bumps           map[string][]byte          // blockHash → bumpData
	minedCalls      []minedCall
	deletedBlocks   []string
	getStumpsErr    error
	insertBUMPErr   error
	insertStumpErr  error
	setMinedErr     error
	deleteStumpsErr error
}

type minedCall struct {
	blockHash string
	txids     []string
}

func newMockStore() *mockStore {
	return &mockStore{
		stumps: make(map[string][]*models.Stump),
		bumps:  make(map[string][]byte),
	}
}

func (m *mockStore) EnsureIndexes() error { return nil }

func (m *mockStore) InsertStump(_ context.Context, stump *models.Stump) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.insertStumpErr != nil {
		return m.insertStumpErr
	}
	m.stumps[stump.BlockHash] = append(m.stumps[stump.BlockHash], stump)
	return nil
}

func (m *mockStore) GetStumpsByBlockHash(_ context.Context, blockHash string) ([]*models.Stump, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getStumpsErr != nil {
		return nil, m.getStumpsErr
	}
	return m.stumps[blockHash], nil
}

func (m *mockStore) InsertBUMP(_ context.Context, blockHash string, _ uint64, bumpData []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.insertBUMPErr != nil {
		return m.insertBUMPErr
	}
	m.bumps[blockHash] = bumpData
	return nil
}

func (m *mockStore) SetMinedByTxIDs(_ context.Context, blockHash string, txids []string) ([]*models.TransactionStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setMinedErr != nil {
		return nil, m.setMinedErr
	}
	m.minedCalls = append(m.minedCalls, minedCall{blockHash, txids})
	var statuses []*models.TransactionStatus
	for _, txid := range txids {
		statuses = append(statuses, &models.TransactionStatus{
			TxID:      txid,
			Status:    models.StatusMined,
			BlockHash: blockHash,
			Timestamp: time.Now(),
		})
	}
	return statuses, nil
}

func (m *mockStore) DeleteStumpsByBlockHash(_ context.Context, blockHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteStumpsErr != nil {
		return m.deleteStumpsErr
	}
	m.deletedBlocks = append(m.deletedBlocks, blockHash)
	delete(m.stumps, blockHash)
	return nil
}

func (m *mockStore) addStump(blockHash string, subtreeIndex int, stumpData []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stumps[blockHash] = append(m.stumps[blockHash], &models.Stump{
		BlockHash:    blockHash,
		SubtreeIndex: subtreeIndex,
		StumpData:    stumpData,
	})
}

// --- Helpers ---

// makeBlockProcessedMsg creates a Kafka ConsumerMessage with a block_processed payload.
func makeBlockProcessedMsg(blockHash string) *sarama.ConsumerMessage {
	callback := models.CallbackMessage{
		Type:      models.CallbackBlockProcessed,
		BlockHash: blockHash,
	}
	data, _ := json.Marshal(callback)
	return &sarama.ConsumerMessage{
		Topic: "arcade.block_processed",
		Value: data,
	}
}

// newDatahubServer creates a test HTTP server that serves binary block data for BUMP construction.
// Binary format: header (80) | txCount (varint) | sizeBytes (varint) |
// subtreeCount (varint) | subtreeHashes (N×32) | coinbaseTx (variable) |
// blockHeight (varint) | coinbaseBUMPLen (varint)
func newDatahubServer(subtreeHexHashes []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer

		// Block header (80 zero bytes)
		buf.Write(make([]byte, 80))

		// Transaction count (varint: 1)
		buf.WriteByte(0x01)

		// Size in bytes (varint: 0 — not used by parser)
		buf.WriteByte(0x00)

		// Subtree count (varint)
		buf.Write(transaction.VarInt(len(subtreeHexHashes)).Bytes())

		// Subtree hashes (32 bytes each)
		for _, hexHash := range subtreeHexHashes {
			hashBytes, _ := hex.DecodeString(hexHash)
			padded := make([]byte, 32)
			copy(padded, hashBytes)
			buf.Write(padded)
		}

		// Minimal coinbase transaction
		coinbaseTx := transaction.NewTransaction()
		buf.Write(coinbaseTx.Bytes())

		// Block height (varint: 1)
		buf.WriteByte(0x01)

		// Coinbase BUMP length (varint: 0 = no BUMP)
		buf.WriteByte(0x00)

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(buf.Bytes())
	}))
}

// newDatahubServerWithCoinbaseBUMP creates a test server that includes coinbase BUMP data.
func newDatahubServerWithCoinbaseBUMP(subtreeHexHashes []string, coinbaseBUMP []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer

		buf.Write(make([]byte, 80))       // header
		buf.WriteByte(0x01)               // txCount
		buf.WriteByte(0x00)               // sizeBytes
		buf.Write(transaction.VarInt(len(subtreeHexHashes)).Bytes()) // subtreeCount

		for _, hexHash := range subtreeHexHashes {
			hashBytes, _ := hex.DecodeString(hexHash)
			padded := make([]byte, 32)
			copy(padded, hashBytes)
			buf.Write(padded)
		}

		coinbaseTx := transaction.NewTransaction()
		buf.Write(coinbaseTx.Bytes())

		buf.WriteByte(0x01) // blockHeight

		// Coinbase BUMP length + data
		buf.Write(transaction.VarInt(len(coinbaseBUMP)).Bytes())
		buf.Write(coinbaseBUMP)

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(buf.Bytes())
	}))
}

func newFailingDatahubServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
}

func newTestBuilder(st store.Store, datahubURL string) *Builder {
	cfg := &config.Config{
		DatahubURLs: []string{datahubURL},
	}
	return &Builder{
		cfg:    cfg,
		logger: zap.NewNop().Named("bump-builder"),
		store:  st,
	}
}

// makeMinimalSTUMP creates a minimal valid BRC-74 STUMP binary with a single level-0 leaf.
// This is the simplest valid merkle path: one transaction, one level, block height 1.
func makeMinimalSTUMP(txidHex string) []byte {
	// BRC-74 binary format:
	// - blockHeight: varint (1 byte for small values)
	// - treeHeight: varint (1 = single level)
	// - nLeaves: varint (1 leaf)
	// - leaf: offset (varint 0) + flags (0x01 = txid) + 32-byte hash
	txidBytes, _ := hex.DecodeString(txidHex)
	if len(txidBytes) != 32 {
		// Pad to 32 bytes
		padded := make([]byte, 32)
		copy(padded, txidBytes)
		txidBytes = padded
	}

	buf := []byte{
		0x01,       // blockHeight = 1
		0x01,       // treeHeight = 1 (one level)
		0x01,       // nLeaves at level 0 = 1
		0x00,       // offset = 0
		0x01,       // flags: bit 0 set = this is a txid (client txid flag)
	}
	buf = append(buf, txidBytes...)
	return buf
}

// --- Tests ---

func TestBuilder_HandleMessage_NoSTUMPs_ReturnsNil(t *testing.T) {
	// A block with no STUMPs means no tracked transactions in it — this is
	// legitimate (STUMPs are sparse) and must not return an error or trigger DLQ.
	ms := newMockStore()
	datahub := newDatahubServer([]string{})
	defer datahub.Close()

	b := newTestBuilder(ms, datahub.URL)

	if err := b.handleMessage(context.Background(), makeBlockProcessedMsg("blockhash123")); err != nil {
		t.Fatalf("expected nil when no STUMPs found, got: %v", err)
	}
	if len(ms.bumps) != 0 {
		t.Errorf("expected no BUMP stored, got %d", len(ms.bumps))
	}
}

func TestBuilder_HandleMessage_GetStumpsError_Propagated(t *testing.T) {
	ms := newMockStore()
	ms.getStumpsErr = errors.New("INDEX_NOTFOUND")

	datahub := newDatahubServer(nil)
	defer datahub.Close()

	b := newTestBuilder(ms, datahub.URL)

	err := b.handleMessage(context.Background(), makeBlockProcessedMsg("blockhash123"))
	if err == nil {
		t.Fatal("expected error from GetStumpsByBlockHash, got nil")
	}
	if !containsStr(err.Error(), "INDEX_NOTFOUND") {
		t.Errorf("expected INDEX_NOTFOUND in error, got: %v", err)
	}
}

func TestBuilder_HandleMessage_DatahubFailure_ReturnsError(t *testing.T) {
	ms := newMockStore()
	blockHash := "aabbccdd00000000000000000000000000000000000000000000000000000000"
	stumpData := makeMinimalSTUMP("1111111111111111111111111111111111111111111111111111111111111111")
	ms.addStump(blockHash, 0, stumpData)

	datahub := newFailingDatahubServer()
	defer datahub.Close()

	b := newTestBuilder(ms, datahub.URL)

	err := b.handleMessage(context.Background(), makeBlockProcessedMsg(blockHash))
	if err == nil {
		t.Fatal("expected error from datahub failure, got nil")
	}
	if !containsStr(err.Error(), "fetching block data") {
		t.Errorf("expected 'fetching block data' error, got: %v", err)
	}
}

func TestBuilder_HandleMessage_InsertBUMPError_ReturnsError(t *testing.T) {
	ms := newMockStore()
	blockHash := "aabbccdd00000000000000000000000000000000000000000000000000000000"

	// Need a valid STUMP that produces a valid BUMP. Use a single-subtree scenario.
	txidHex := "1111111111111111111111111111111111111111111111111111111111111111"
	stumpData := makeMinimalSTUMP(txidHex)
	ms.addStump(blockHash, 0, stumpData)
	ms.insertBUMPErr = errors.New("SERVER_MEM_ERROR")

	// Datahub returns one subtree hash (single-subtree block: STUMP = BUMP)
	subtreeHash := "2222222222222222222222222222222222222222222222222222222222222222"
	datahub := newDatahubServer([]string{subtreeHash})
	defer datahub.Close()

	b := newTestBuilder(ms, datahub.URL)

	err := b.handleMessage(context.Background(), makeBlockProcessedMsg(blockHash))
	if err == nil {
		t.Fatal("expected error from InsertBUMP, got nil")
	}
	if !containsStr(err.Error(), "storing BUMP") {
		t.Errorf("expected 'storing BUMP' error, got: %v", err)
	}
}

func TestBuilder_HandleMessage_HappyPath_SingleSubtree(t *testing.T) {
	ms := newMockStore()
	blockHash := "aabbccdd00000000000000000000000000000000000000000000000000000000"
	txidHex := "1111111111111111111111111111111111111111111111111111111111111111"

	stumpData := makeMinimalSTUMP(txidHex)
	ms.addStump(blockHash, 0, stumpData)

	// Single subtree: its hash doesn't matter much for the builder flow
	subtreeHash := "2222222222222222222222222222222222222222222222222222222222222222"
	datahub := newDatahubServer([]string{subtreeHash})
	defer datahub.Close()

	b := newTestBuilder(ms, datahub.URL)

	err := b.handleMessage(context.Background(), makeBlockProcessedMsg(blockHash))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify BUMP was stored
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if _, ok := ms.bumps[blockHash]; !ok {
		t.Error("expected BUMP to be stored")
	}

	// Verify STUMPs were pruned
	if len(ms.deletedBlocks) != 1 || ms.deletedBlocks[0] != blockHash {
		t.Errorf("expected STUMPs for %s to be deleted, got: %v", blockHash, ms.deletedBlocks)
	}
}

func TestBuilder_HandleMessage_HappyPath_WithTracker(t *testing.T) {
	ms := newMockStore()
	blockHash := "aabbccdd00000000000000000000000000000000000000000000000000000000"
	txidHex := "1111111111111111111111111111111111111111111111111111111111111111"

	stumpData := makeMinimalSTUMP(txidHex)
	ms.addStump(blockHash, 0, stumpData)

	subtreeHash := "2222222222222222222222222222222222222222222222222222222222222222"
	datahub := newDatahubServer([]string{subtreeHash})
	defer datahub.Close()

	b := newTestBuilder(ms, datahub.URL)

	err := b.handleMessage(context.Background(), makeBlockProcessedMsg(blockHash))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify BUMP was stored and STUMPs pruned
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if _, ok := ms.bumps[blockHash]; !ok {
		t.Error("expected BUMP to be stored")
	}
	if len(ms.deletedBlocks) != 1 {
		t.Errorf("expected STUMPs to be pruned, got %d deletes", len(ms.deletedBlocks))
	}
}

func TestBuilder_HandleMessage_EmptyBlockHash_ReturnsError(t *testing.T) {
	ms := newMockStore()
	datahub := newDatahubServer(nil)
	defer datahub.Close()

	b := newTestBuilder(ms, datahub.URL)

	err := b.handleMessage(context.Background(), makeBlockProcessedMsg(""))
	if err == nil {
		t.Fatal("expected error for empty block hash, got nil")
	}
}

func TestBuilder_HandleMessage_InvalidJSON_ReturnsError(t *testing.T) {
	ms := newMockStore()
	datahub := newDatahubServer(nil)
	defer datahub.Close()

	b := newTestBuilder(ms, datahub.URL)

	msg := &sarama.ConsumerMessage{
		Topic: "arcade.block_processed",
		Value: []byte("not json"),
	}
	err := b.handleMessage(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

// TestBuilder_LateSTUMP_ArrivesDuringGraceWindow simulates a late-retry STUMP that
// lands after BLOCK_PROCESSED but within the grace window. The builder should wait
// the grace window, then find the STUMP and build the BUMP successfully.
func TestBuilder_LateSTUMP_ArrivesDuringGraceWindow(t *testing.T) {
	ms := newMockStore()
	blockHash := "aabbccdd00000000000000000000000000000000000000000000000000000000"
	txidHex := "1111111111111111111111111111111111111111111111111111111111111111"

	subtreeHash := "2222222222222222222222222222222222222222222222222222222222222222"
	datahub := newDatahubServer([]string{subtreeHash})
	defer datahub.Close()

	b := newTestBuilder(ms, datahub.URL)
	b.cfg.BumpBuilder.GraceWindowMs = 100 // short grace for the test

	// Insert STUMP mid-grace-window from another goroutine
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = ms.InsertStump(context.Background(), &models.Stump{
			BlockHash:    blockHash,
			SubtreeIndex: 0,
			StumpData:    makeMinimalSTUMP(txidHex),
		})
	}()

	if err := b.handleMessage(context.Background(), makeBlockProcessedMsg(blockHash)); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	ms.mu.Lock()
	defer ms.mu.Unlock()
	if _, ok := ms.bumps[blockHash]; !ok {
		t.Error("expected BUMP to be stored after late STUMP landed in grace window")
	}
}

// TestBuilder_E2E_InsertStump_GetStumps_BuildBUMP verifies the full STUMP→BUMP workflow:
// store STUMPs via InsertStump → query via GetStumpsByBlockHash → build BUMP.
func TestBuilder_E2E_InsertStump_GetStumps_BuildBUMP(t *testing.T) {
	ms := newMockStore()
	blockHash := "aabbccdd00000000000000000000000000000000000000000000000000000000"
	txid1 := "1111111111111111111111111111111111111111111111111111111111111111"
	txid2 := "3333333333333333333333333333333333333333333333333333333333333333"

	// Store STUMPs via the store interface (mimicking stump consumer)
	for i, txid := range []string{txid1, txid2} {
		stump := &models.Stump{
			BlockHash:    blockHash,
			SubtreeIndex: i,
			StumpData:    makeMinimalSTUMP(txid),
		}
		if err := ms.InsertStump(context.Background(), stump); err != nil {
			t.Fatalf("InsertStump %d failed: %v", i, err)
		}
	}

	// Verify STUMPs are retrievable
	stumps, err := ms.GetStumpsByBlockHash(context.Background(), blockHash)
	if err != nil {
		t.Fatalf("GetStumpsByBlockHash failed: %v", err)
	}
	if len(stumps) != 2 {
		t.Fatalf("expected 2 STUMPs, got %d", len(stumps))
	}

	// Build BUMP via handleMessage
	subtreeHash1 := "2222222222222222222222222222222222222222222222222222222222222222"
	subtreeHash2 := "4444444444444444444444444444444444444444444444444444444444444444"
	datahub := newDatahubServer([]string{subtreeHash1, subtreeHash2})
	defer datahub.Close()

	b := newTestBuilder(ms, datahub.URL)

	if err := b.handleMessage(context.Background(), makeBlockProcessedMsg(blockHash)); err != nil {
		t.Fatalf("handleMessage failed: %v", err)
	}

	// Verify BUMP was stored
	ms.mu.Lock()
	defer ms.mu.Unlock()
	bumpData, ok := ms.bumps[blockHash]
	if !ok {
		t.Fatal("expected BUMP to be stored")
	}
	if len(bumpData) == 0 {
		t.Error("expected non-empty BUMP data")
	}

	// Verify STUMPs were cleaned up
	if len(ms.deletedBlocks) != 1 || ms.deletedBlocks[0] != blockHash {
		t.Errorf("expected STUMPs for %s to be deleted, got: %v", blockHash, ms.deletedBlocks)
	}
}

func TestBuilder_E2E_BUMPExtractionWithRealisticSTUMP(t *testing.T) {
	// This test uses realistic STUMPs (built via go-sdk types) to verify
	// the full flow: store STUMPs → build BUMP → extract per-tx path.
	// The bump/bump_test.go suite covers ExtractMinimalPathForTx exhaustively;
	// this test verifies the builder stores data that extraction can consume.
	ms := newMockStore()
	blockHash := "aabbccdd00000000000000000000000000000000000000000000000000000000"

	// Build a realistic STUMP with 2 leaves using go-sdk types
	txHash1Bytes, _ := hex.DecodeString("1111111111111111111111111111111111111111111111111111111111111111")
	txHash2Bytes, _ := hex.DecodeString("2222222222222222222222222222222222222222222222222222222222222222")
	h1, _ := chainhash.NewHash(txHash1Bytes)
	h2, _ := chainhash.NewHash(txHash2Bytes)

	isTxid := true
	isNotTxid := false
	stumpMP := transaction.NewMerklePath(1, [][]*transaction.PathElement{
		{
			{Offset: 0, Hash: h1, Txid: &isTxid},
			{Offset: 1, Hash: h2, Txid: &isNotTxid},
		},
	})
	stumpData := stumpMP.Bytes()
	ms.addStump(blockHash, 0, stumpData)

	subtreeHash := "3333333333333333333333333333333333333333333333333333333333333333"
	datahub := newDatahubServer([]string{subtreeHash})
	defer datahub.Close()

	b := newTestBuilder(ms, datahub.URL)

	if err := b.handleMessage(context.Background(), makeBlockProcessedMsg(blockHash)); err != nil {
		t.Fatalf("handleMessage failed: %v", err)
	}

	ms.mu.Lock()
	bumpData, ok := ms.bumps[blockHash]
	ms.mu.Unlock()
	if !ok || len(bumpData) == 0 {
		t.Fatal("expected non-empty BUMP data to be stored")
	}

	// Extract minimal path for the tracked txid
	result := bump.ExtractMinimalPathForTx(bumpData, h1.String())
	if result == nil {
		t.Fatal("ExtractMinimalPathForTx returned nil for tracked txid")
	}
	if len(result) == 0 {
		t.Fatal("ExtractMinimalPathForTx returned empty bytes")
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && searchStr(s, substr)
}

func searchStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
