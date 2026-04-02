package bump

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
)

var httpClient = &http.Client{Timeout: 5 * time.Minute}

// blockJSONResponse is the JSON format returned by the DataHub block endpoint.
type blockJSONResponse struct {
	Subtrees     []string `json:"subtrees"`
	CoinbaseBump string   `json:"coinbase_bump"`
}

// FetchBlockDataForBUMP fetches subtree hashes and coinbase BUMP for a block,
// trying all DataHub URLs. Prefers the JSON endpoint (which includes coinbase_bump),
// falling back to the binary endpoint (no coinbase BUMP).
func FetchBlockDataForBUMP(ctx context.Context, datahubURLs []string, blockHash string) (subtreeHashes []chainhash.Hash, coinbaseBUMP []byte, err error) {
	for _, dataHubURL := range datahubURLs {
		// Try JSON endpoint first (includes coinbase_bump)
		resp, jsonErr := fetchBlockJSON(ctx, dataHubURL, blockHash)
		if jsonErr == nil {
			hashes, coinbase, parseErr := parseBlockJSONResponse(resp)
			if parseErr == nil {
				return hashes, coinbase, nil
			}
		}

		// Fall back to binary endpoint (no coinbase BUMP)
		hashes, binErr := fetchBlockBinarySubtrees(ctx, dataHubURL, blockHash)
		if binErr == nil {
			return hashes, nil, nil
		}
	}
	return nil, nil, fmt.Errorf("all DataHub URLs failed for block %s", blockHash)
}

func fetchBlockJSON(ctx context.Context, baseURL, blockHash string) (*blockJSONResponse, error) {
	url := fmt.Sprintf("%s/block/%s?format=json", baseURL, blockHash)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result blockJSONResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

func fetchBlockBinarySubtrees(ctx context.Context, baseURL, blockHash string) ([]chainhash.Hash, error) {
	url := fmt.Sprintf("%s/block/%s/subtrees", baseURL, blockHash)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Binary format: concatenated 32-byte hashes
	if len(body)%32 != 0 {
		return nil, fmt.Errorf("invalid binary subtree data: %d bytes", len(body))
	}

	hashes := make([]chainhash.Hash, len(body)/32)
	for i := range hashes {
		copy(hashes[i][:], body[i*32:(i+1)*32])
	}

	return hashes, nil
}

// parseBlockJSONResponse converts a blockJSONResponse into typed data.
func parseBlockJSONResponse(resp *blockJSONResponse) ([]chainhash.Hash, []byte, error) {
	hashes := make([]chainhash.Hash, 0, len(resp.Subtrees))
	for _, hexStr := range resp.Subtrees {
		h, err := chainhash.NewHashFromHex(hexStr)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse subtree hash %q: %w", hexStr, err)
		}
		hashes = append(hashes, *h)
	}

	var coinbaseBUMP []byte
	if resp.CoinbaseBump != "" {
		var err error
		coinbaseBUMP, err = hex.DecodeString(resp.CoinbaseBump)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to decode coinbase_bump: %w", err)
		}
	}

	return hashes, coinbaseBUMP, nil
}
