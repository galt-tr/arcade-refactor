package propagation

import "strings"

// retryablePatterns contains error message substrings that indicate a transient
// broadcast failure eligible for retry (e.g., parent tx not yet visible to the network).
var retryablePatterns = []string{
	"missing inputs",
	"txn-mempool-conflict",
	"failed to validate", // teranode batch wraps missing-input errors this way
}

// IsRetryableError returns true if the error message indicates a transient
// broadcast failure that may succeed on retry.
func IsRetryableError(errMsg string) bool {
	if errMsg == "" {
		return false
	}
	lower := strings.ToLower(errMsg)
	for _, pattern := range retryablePatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}
