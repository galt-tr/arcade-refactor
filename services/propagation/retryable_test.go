package propagation

import "testing"

func TestIsRetryableError(t *testing.T) {
	tests := []struct {
		name     string
		errMsg   string
		expected bool
	}{
		{"missing inputs", "Transaction rejected: missing inputs", true},
		{"missing inputs lowercase", "missing inputs for tx abc123", true},
		{"missing inputs uppercase", "MISSING INPUTS", true},
		{"mempool conflict", "txn-mempool-conflict", true},
		{"mempool conflict in message", "error: txn-mempool-conflict detected", true},
		{"mempool conflict mixed case", "TXN-MEMPOOL-CONFLICT", true},
		{"failed to validate", "PROCESSING (4): [ProcessTransaction][txid] failed to validate transaction", true},
		{"failed to validate lowercase", "failed to validate transaction", true},
		{"permanent error", "transaction is invalid: bad-txns-vin-empty", false},
		{"script error", "mandatory-script-verify-flag-failed", false},
		{"empty string", "", false},
		{"random error", "connection refused", false},
		{"insufficient fee", "insufficient fee", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsRetryableError(tt.errMsg)
			if got != tt.expected {
				t.Errorf("IsRetryableError(%q) = %v, want %v", tt.errMsg, got, tt.expected)
			}
		})
	}
}
