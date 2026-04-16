package propagation

import (
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

const maxBackoffCap = 30 * time.Second

// RetryEntry represents a transaction awaiting retry.
type RetryEntry struct {
	TXID       string
	RawTxHex   string
	Attempt    int
	NextRetry  time.Time
}

// RetryBuffer holds transactions that failed with retryable errors and
// are waiting for their backoff period to elapse before re-broadcast.
type RetryBuffer struct {
	mu            sync.Mutex
	entries       map[string]*RetryEntry // txid → entry
	maxSize       int
	baseBackoffMs int
}

// NewRetryBuffer creates a retry buffer with the given capacity and base backoff.
func NewRetryBuffer(maxSize, baseBackoffMs int) *RetryBuffer {
	return &RetryBuffer{
		entries:       make(map[string]*RetryEntry),
		maxSize:       maxSize,
		baseBackoffMs: baseBackoffMs,
	}
}

// Add inserts or updates a retry entry. Returns false if the buffer is full
// and the entry was not already present.
func (rb *RetryBuffer) Add(entry *RetryEntry) bool {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if _, exists := rb.entries[entry.TXID]; exists {
		rb.entries[entry.TXID] = entry
		return true
	}
	if len(rb.entries) >= rb.maxSize {
		return false
	}
	rb.entries[entry.TXID] = entry
	return true
}

// Ready returns all entries whose backoff period has elapsed.
func (rb *RetryBuffer) Ready() []*RetryEntry {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	now := time.Now()
	var ready []*RetryEntry
	for _, entry := range rb.entries {
		if now.After(entry.NextRetry) || now.Equal(entry.NextRetry) {
			ready = append(ready, entry)
		}
	}
	return ready
}

// Remove deletes an entry from the buffer by txid.
func (rb *RetryBuffer) Remove(txid string) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	delete(rb.entries, txid)
}

// IsFull returns true if the buffer is at capacity.
func (rb *RetryBuffer) IsFull() bool {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return len(rb.entries) >= rb.maxSize
}

// Len returns the current number of entries.
func (rb *RetryBuffer) Len() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return len(rb.entries)
}

// ComputeBackoff returns the next retry time for a given attempt number using
// exponential backoff with ±25% jitter, capped at 30 seconds.
func (rb *RetryBuffer) ComputeBackoff(attempt int) time.Time {
	base := time.Duration(rb.baseBackoffMs) * time.Millisecond
	delay := base * time.Duration(math.Pow(2, float64(attempt)))
	if delay > maxBackoffCap {
		delay = maxBackoffCap
	}

	// Apply ±25% jitter
	jitter := float64(delay) * 0.25
	delay = time.Duration(float64(delay) + (rand.Float64()*2-1)*jitter)
	if delay < 0 {
		delay = 0
	}

	return time.Now().Add(delay)
}
