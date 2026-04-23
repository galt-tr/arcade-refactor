//go:build postgres

// Package postgres tests run against embedded-postgres by default. They're
// gated behind the "postgres" build tag because embedded-postgres downloads
// ~80MB of bundled binaries on first run, which is too costly for the default
// `go test ./...` invocation. Run with:
//
//     go test -tags=postgres ./store/postgres/...
//
// If ARCADE_POSTGRES_DSN is set the tests use that external Postgres instead
// of spinning up embedded — useful for CI or running against a real cluster.
package postgres

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/models"
	"github.com/bsv-blockchain/arcade/store"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	var cfg config.Postgres
	if dsn := os.Getenv("ARCADE_POSTGRES_DSN"); dsn != "" {
		cfg = config.Postgres{DSN: dsn, MaxConns: 4}
	} else {
		dir := t.TempDir()
		cfg = config.Postgres{
			Embedded:         true,
			EmbeddedUser:     "arcade",
			EmbeddedPassword: "arcade",
			EmbeddedDatabase: "arcade",
			EmbeddedDataDir:  dir + "/data",
			EmbeddedCacheDir: dir + "/cache",
			MaxConns:         4,
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.EnsureIndexes(); err != nil {
		_ = s.Close()
		t.Fatalf("EnsureIndexes: %v", err)
	}
	// Each test uses a fresh DSN or fresh tables; when sharing an external DSN
	// we truncate to prevent cross-test bleed.
	if os.Getenv("ARCADE_POSTGRES_DSN") != "" {
		if _, err := s.pool.Exec(ctx,
			`TRUNCATE transactions, bumps, stumps, submissions, leases`); err != nil {
			t.Fatalf("truncate: %v", err)
		}
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestGetOrInsertStatus_InsertsNew(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	in := &models.TransactionStatus{TxID: "abc", Status: models.StatusReceived}
	got, inserted, err := s.GetOrInsertStatus(ctx, in)
	if err != nil {
		t.Fatalf("GetOrInsertStatus: %v", err)
	}
	if !inserted {
		t.Fatal("expected inserted=true for new txid")
	}
	if got.TxID != "abc" || got.Status != models.StatusReceived {
		t.Fatalf("unexpected status: %+v", got)
	}
}

func TestGetOrInsertStatus_ReturnsExisting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	first := &models.TransactionStatus{TxID: "abc", Status: models.StatusReceived}
	if _, inserted, err := s.GetOrInsertStatus(ctx, first); err != nil || !inserted {
		t.Fatalf("first insert: inserted=%v err=%v", inserted, err)
	}

	second := &models.TransactionStatus{TxID: "abc", Status: models.StatusSentToNetwork}
	got, inserted, err := s.GetOrInsertStatus(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("expected inserted=false for existing txid")
	}
	if got.Status != models.StatusReceived {
		t.Fatalf("expected existing status RECEIVED, got %s", got.Status)
	}
}

// Postgres handles the CAS natively (ON CONFLICT DO NOTHING); the test still
// asserts that N concurrent inserts collapse to exactly one winner.
func TestGetOrInsertStatus_ConcurrentRace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const N = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	var inserted int

	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, ok, err := s.GetOrInsertStatus(ctx, &models.TransactionStatus{
				TxID: "racey", Status: models.StatusReceived,
			})
			if err != nil {
				t.Errorf("concurrent insert: %v", err)
				return
			}
			if ok {
				mu.Lock()
				inserted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if inserted != 1 {
		t.Fatalf("expected exactly 1 successful insert, got %d", inserted)
	}
}

func TestPendingRetryLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	txid := "retry-tx"
	rawTx := []byte{0x01, 0x02}
	nextRetry := time.Now().Add(-time.Second) // already due

	if _, _, err := s.GetOrInsertStatus(ctx, &models.TransactionStatus{TxID: txid, Status: models.StatusReceived}); err != nil {
		t.Fatal(err)
	}

	n, err := s.BumpRetryCount(ctx, txid)
	if err != nil || n != 1 {
		t.Fatalf("BumpRetryCount: n=%d err=%v", n, err)
	}

	if err := s.SetPendingRetryFields(ctx, txid, rawTx, nextRetry); err != nil {
		t.Fatal(err)
	}

	ready, err := s.GetReadyRetries(ctx, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].TxID != txid {
		t.Fatalf("GetReadyRetries: %+v", ready)
	}
	if ready[0].RetryCount != 1 {
		t.Fatalf("expected RetryCount=1, got %d", ready[0].RetryCount)
	}

	if err := s.ClearRetryState(ctx, txid, models.StatusRejected, "final"); err != nil {
		t.Fatal(err)
	}
	ready, err = s.GetReadyRetries(ctx, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 0 {
		t.Fatalf("expected 0 ready retries after clear, got %d", len(ready))
	}
}

func TestGetReadyRetries_SkipsFutureEntries(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Now()
	cases := []struct {
		txid    string
		delay   time.Duration
		isReady bool
	}{
		{"past-1", -2 * time.Second, true},
		{"past-2", -time.Second, true},
		{"future-1", time.Hour, false},
	}
	for _, c := range cases {
		if _, _, err := s.GetOrInsertStatus(ctx, &models.TransactionStatus{TxID: c.txid, Status: models.StatusReceived}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetPendingRetryFields(ctx, c.txid, []byte{0xff}, now.Add(c.delay)); err != nil {
			t.Fatal(err)
		}
	}

	ready, err := s.GetReadyRetries(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range ready {
		got[r.TxID] = true
	}
	for _, c := range cases {
		if got[c.txid] != c.isReady {
			t.Errorf("%s: isReady=%v, got=%v", c.txid, c.isReady, got[c.txid])
		}
	}
}

func TestSetStatusByBlockHash_UpdatesAllInBlock(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	blockHash := "bh-1"
	txids := []string{"t1", "t2", "t3"}
	for _, txid := range txids {
		if _, _, err := s.GetOrInsertStatus(ctx, &models.TransactionStatus{
			TxID: txid, Status: models.StatusMined, BlockHash: blockHash, Timestamp: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	updated, err := s.SetStatusByBlockHash(ctx, blockHash, models.StatusSeenOnNetwork)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 3 {
		t.Fatalf("expected 3 updated txids, got %d", len(updated))
	}
	for _, txid := range txids {
		got, _ := s.GetStatus(ctx, txid)
		if got == nil || got.Status != models.StatusSeenOnNetwork {
			t.Errorf("%s: expected SEEN_ON_NETWORK, got %+v", txid, got)
		}
		if got.BlockHash != "" {
			t.Errorf("%s: expected empty BlockHash after reorg, got %s", txid, got.BlockHash)
		}
	}
}

func TestSubmissions_InsertAndQueryByTxID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	sub := &models.Submission{
		SubmissionID: "sub-1",
		TxID:         "tx-a",
		CallbackURL:  "https://example.test/cb",
		CreatedAt:    time.Now(),
	}
	if err := s.InsertSubmission(ctx, sub); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSubmissionsByTxID(ctx, "tx-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SubmissionID != "sub-1" {
		t.Fatalf("GetSubmissionsByTxID: %+v", got)
	}
}

func TestLease_AcquireAndRenew(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	held, err := s.TryAcquireOrRenew(ctx, "reaper", "holder-a", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if held.IsZero() {
		t.Fatal("expected non-zero heldUntil for fresh lease")
	}

	// Same holder can renew.
	renewed, err := s.TryAcquireOrRenew(ctx, "reaper", "holder-a", time.Second)
	if err != nil || renewed.IsZero() {
		t.Fatalf("renew: heldUntil=%v err=%v", renewed, err)
	}

	// Different holder is blocked while the current lease is live.
	blocked, err := s.TryAcquireOrRenew(ctx, "reaper", "holder-b", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !blocked.IsZero() {
		t.Fatal("expected zero heldUntil for contention")
	}
}

func TestBumpRetryCount_UnknownTxID(t *testing.T) {
	s := newTestStore(t)
	_, err := s.BumpRetryCount(context.Background(), "ghost")
	if err == nil {
		t.Fatal("expected error for unknown txid")
	}
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestBUMPInsertAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.InsertBUMP(ctx, "bh-bump", 42, []byte{0xde, 0xad, 0xbe, 0xef}); err != nil {
		t.Fatal(err)
	}
	h, data, err := s.GetBUMP(ctx, "bh-bump")
	if err != nil {
		t.Fatal(err)
	}
	if h != 42 || len(data) != 4 {
		t.Fatalf("unexpected bump: h=%d data=%x", h, data)
	}

	if _, _, err := s.GetBUMP(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing bump, got %v", err)
	}
}
