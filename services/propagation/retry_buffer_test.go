package propagation

import (
	"testing"
	"time"
)

func TestRetryBuffer_AddAndLen(t *testing.T) {
	rb := NewRetryBuffer(3, 500)

	if rb.Len() != 0 {
		t.Fatalf("expected empty buffer, got %d", rb.Len())
	}

	ok := rb.Add(&RetryEntry{TXID: "tx1", Attempt: 0, NextRetry: time.Now()})
	if !ok {
		t.Fatal("expected Add to succeed")
	}
	if rb.Len() != 1 {
		t.Fatalf("expected 1 entry, got %d", rb.Len())
	}

	// Adding same txid updates, doesn't increase count
	ok = rb.Add(&RetryEntry{TXID: "tx1", Attempt: 1, NextRetry: time.Now()})
	if !ok {
		t.Fatal("expected Add to succeed for update")
	}
	if rb.Len() != 1 {
		t.Fatalf("expected 1 entry after update, got %d", rb.Len())
	}
}

func TestRetryBuffer_CapacityLimit(t *testing.T) {
	rb := NewRetryBuffer(2, 500)

	rb.Add(&RetryEntry{TXID: "tx1", NextRetry: time.Now()})
	rb.Add(&RetryEntry{TXID: "tx2", NextRetry: time.Now()})

	if !rb.IsFull() {
		t.Fatal("expected buffer to be full")
	}

	ok := rb.Add(&RetryEntry{TXID: "tx3", NextRetry: time.Now()})
	if ok {
		t.Fatal("expected Add to fail when buffer is full")
	}
	if rb.Len() != 2 {
		t.Fatalf("expected 2 entries, got %d", rb.Len())
	}
}

func TestRetryBuffer_Remove(t *testing.T) {
	rb := NewRetryBuffer(10, 500)
	rb.Add(&RetryEntry{TXID: "tx1", NextRetry: time.Now()})
	rb.Add(&RetryEntry{TXID: "tx2", NextRetry: time.Now()})

	rb.Remove("tx1")
	if rb.Len() != 1 {
		t.Fatalf("expected 1 entry after remove, got %d", rb.Len())
	}

	// Removing non-existent is a no-op
	rb.Remove("tx999")
	if rb.Len() != 1 {
		t.Fatalf("expected 1 entry after no-op remove, got %d", rb.Len())
	}
}

func TestRetryBuffer_Ready(t *testing.T) {
	rb := NewRetryBuffer(10, 500)

	past := time.Now().Add(-1 * time.Second)
	future := time.Now().Add(1 * time.Hour)

	rb.Add(&RetryEntry{TXID: "ready1", NextRetry: past})
	rb.Add(&RetryEntry{TXID: "ready2", NextRetry: past})
	rb.Add(&RetryEntry{TXID: "notReady", NextRetry: future})

	ready := rb.Ready()
	if len(ready) != 2 {
		t.Fatalf("expected 2 ready entries, got %d", len(ready))
	}

	txids := make(map[string]bool)
	for _, e := range ready {
		txids[e.TXID] = true
	}
	if !txids["ready1"] || !txids["ready2"] {
		t.Fatalf("expected ready1 and ready2 in results, got %v", txids)
	}
}

func TestRetryBuffer_ComputeBackoff(t *testing.T) {
	rb := NewRetryBuffer(10, 500)

	// Attempt 0: ~500ms (±25%)
	before := time.Now()
	next := rb.ComputeBackoff(0)
	delay := next.Sub(before)
	if delay < 375*time.Millisecond || delay > 625*time.Millisecond+10*time.Millisecond {
		t.Fatalf("attempt 0 delay %v out of expected range [375ms, 625ms]", delay)
	}

	// Attempt 1: ~1000ms (±25%)
	before = time.Now()
	next = rb.ComputeBackoff(1)
	delay = next.Sub(before)
	if delay < 750*time.Millisecond || delay > 1250*time.Millisecond+10*time.Millisecond {
		t.Fatalf("attempt 1 delay %v out of expected range [750ms, 1250ms]", delay)
	}

	// High attempt: capped at 30s (±25%)
	before = time.Now()
	next = rb.ComputeBackoff(20)
	delay = next.Sub(before)
	if delay > 37500*time.Millisecond+10*time.Millisecond {
		t.Fatalf("high attempt delay %v exceeds cap + jitter", delay)
	}
}

func TestRetryBuffer_AddAfterRemoveFreeSpace(t *testing.T) {
	rb := NewRetryBuffer(1, 500)

	rb.Add(&RetryEntry{TXID: "tx1", NextRetry: time.Now()})
	if !rb.IsFull() {
		t.Fatal("expected full")
	}

	rb.Remove("tx1")
	ok := rb.Add(&RetryEntry{TXID: "tx2", NextRetry: time.Now()})
	if !ok {
		t.Fatal("expected Add to succeed after remove freed space")
	}
}
