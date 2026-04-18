package bump

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

var httpClient = &http.Client{Timeout: 5 * time.Minute}

// FetchBlockDataForBUMP fetches subtree hashes, coinbase BUMP, and the block's
// header merkle root from the binary block endpoint, trying all DataHub URLs.
//
// Binary format: header (80) | txCount (varint) | sizeBytes (varint) |
// subtreeCount (varint) | subtreeHashes (N×32) | coinbaseTx (variable) |
// blockHeight (varint) | coinbaseBUMPLen (varint) | coinbaseBUMP (variable)
func FetchBlockDataForBUMP(ctx context.Context, datahubURLs []string, blockHash string) (subtreeHashes []chainhash.Hash, coinbaseBUMP []byte, headerMerkleRoot *chainhash.Hash, err error) {
	var urlErrors []string
	for i, dataHubURL := range datahubURLs {
		hashes, cbBUMP, root, fetchErr := fetchBlockBinary(ctx, dataHubURL, blockHash)
		if fetchErr == nil {
			return hashes, cbBUMP, root, nil
		}
		urlErrors = append(urlErrors, fmt.Sprintf("url[%d] %q: %v", i, dataHubURL, fetchErr))
	}
	return nil, nil, nil, fmt.Errorf("all DataHub URLs failed for block %s:\n  %s", blockHash, strings.Join(urlErrors, "\n  "))
}

// fetchBlockBinary fetches a block from the binary endpoint and parses
// subtree hashes, coinbase BUMP, and the header merkle root from the response.
func fetchBlockBinary(ctx context.Context, baseURL, blockHash string) ([]chainhash.Hash, []byte, *chainhash.Hash, error) {
	url := fmt.Sprintf("%s/block/%s", baseURL, blockHash)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, nil, nil, fmt.Errorf("GET %s: status %d (body: %s)", url, resp.StatusCode, string(body))
	}

	// Read entire response into memory so we can use NewTransactionFromStream
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return parseBlockBinary(data)
}

// parseBlockBinary parses the binary block format:
// header (80) | txCount (varint) | sizeBytes (varint) |
// subtreeCount (varint) | subtreeHashes (N×32) | coinbaseTx (variable) |
// blockHeight (varint) | coinbaseBUMPLen (varint) | coinbaseBUMP (variable)
//
// The 80-byte header layout is: version (4) | prevBlockHash (32) |
// merkleRoot (32) | time (4) | bits (4) | nonce (4).
func parseBlockBinary(data []byte) ([]chainhash.Hash, []byte, *chainhash.Hash, error) {
	if len(data) < 80 {
		return nil, nil, nil, fmt.Errorf("block data too short for header: %d bytes", len(data))
	}

	headerMerkleRoot, err := chainhash.NewHash(data[36:68])
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to parse header merkle root: %w", err)
	}

	r := bytes.NewReader(data[80:]) // skip block header

	// Read transaction count (varint)
	var txCount transaction.VarInt
	if _, err := txCount.ReadFrom(r); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to read transaction count: %w", err)
	}

	// Read size in bytes (varint)
	var sizeBytes transaction.VarInt
	if _, err := sizeBytes.ReadFrom(r); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to read size in bytes: %w", err)
	}

	// Read subtree count (varint)
	var subtreeCount transaction.VarInt
	if _, err := subtreeCount.ReadFrom(r); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to read subtree count: %w", err)
	}

	// Read subtree hashes (32 bytes each)
	hashes := make([]chainhash.Hash, 0, uint64(subtreeCount))
	hashBuf := make([]byte, 32)
	for i := uint64(0); i < uint64(subtreeCount); i++ {
		if _, err := io.ReadFull(r, hashBuf); err != nil {
			return nil, nil, nil, fmt.Errorf("failed to read subtree hash %d: %w", i, err)
		}
		hash, err := chainhash.NewHash(hashBuf)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to create hash: %w", err)
		}
		hashes = append(hashes, *hash)
	}

	// Parse coinbase transaction (variable length) to skip past it
	remaining := data[len(data)-r.Len():]
	_, txBytesUsed, err := transaction.NewTransactionFromStream(remaining)
	if err != nil {
		// Coinbase tx parsing failed — return subtree hashes without coinbase BUMP
		return hashes, nil, headerMerkleRoot, nil
	}

	r = bytes.NewReader(remaining[txBytesUsed:])

	// Read block height (varint)
	var blockHeight transaction.VarInt
	if _, err := blockHeight.ReadFrom(r); err != nil {
		return hashes, nil, headerMerkleRoot, nil
	}

	// Read coinbase BUMP length (varint)
	var cbBUMPLen transaction.VarInt
	if _, err := cbBUMPLen.ReadFrom(r); err != nil {
		return hashes, nil, headerMerkleRoot, nil
	}

	if uint64(cbBUMPLen) == 0 {
		return hashes, nil, headerMerkleRoot, nil
	}

	// Read coinbase BUMP data
	coinbaseBUMP := make([]byte, uint64(cbBUMPLen))
	if _, err := io.ReadFull(r, coinbaseBUMP); err != nil {
		return hashes, nil, headerMerkleRoot, nil
	}

	return hashes, coinbaseBUMP, headerMerkleRoot, nil
}
