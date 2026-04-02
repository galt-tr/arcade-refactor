// Package bump implements BUMP (Bitcoin Unified Merkle Path) construction from STUMPs.
// This is ported directly from the reference arcade implementation.
package bump

import (
	"fmt"
	"math"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/bsv-blockchain/arcade/models"
	"github.com/bsv-blockchain/arcade/store"
)

// AssembleBUMP constructs a minimal BUMP (block-level merkle proof) from a STUMP
// (subtree-level merkle path) and the block's subtree root hashes.
// The returned path contains only the nodes needed to verify the single tracked transaction.
//
// Parameters:
//   - stumpData: BRC-74 binary-encoded STUMP (subtree merkle path with local offsets)
//   - subtreeIndex: index of the subtree containing the tracked transaction
//   - subtreeHashes: root hashes for all subtrees in the block
//   - coinbaseBUMP: BRC-74 binary-encoded merkle path of the coinbase transaction; nil if unavailable.
//
// Returns the minimal merkle path for the tracked transaction (with global offsets) and the global tx offset.
func AssembleBUMP(stumpData []byte, subtreeIndex int, subtreeHashes []chainhash.Hash, coinbaseBUMP []byte) (*transaction.MerklePath, uint64, error) {
	fullPath, txOffset, err := assembleFullPath(stumpData, subtreeIndex, subtreeHashes, coinbaseBUMP)
	if err != nil {
		return nil, 0, err
	}
	minimalPath := ExtractMinimalPath(fullPath, txOffset)
	return minimalPath, txOffset, nil
}

// assembleFullPath constructs a full BUMP from a STUMP, retaining ALL level-0 hashes
// and intermediate nodes. This is used by BuildCompoundBUMP to preserve data for all
// tracked transactions, not just one.
func assembleFullPath(stumpData []byte, subtreeIndex int, subtreeHashes []chainhash.Hash, coinbaseBUMP []byte) (*transaction.MerklePath, uint64, error) {
	stumpPath, err := transaction.NewMerklePathFromBinary(stumpData)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to parse STUMP: %w", err)
	}

	internalHeight := len(stumpPath.Path)

	// Handle coinbase placeholder replacement for subtree 0.
	if subtreeIndex == 0 && len(coinbaseBUMP) > 0 {
		coinbaseTxID := extractCoinbaseTxID(coinbaseBUMP)
		if coinbaseTxID != nil {
			applyCoinbaseToSTUMP(stumpPath, coinbaseTxID, coinbaseBUMP)
		}
	}

	numSubtrees := len(subtreeHashes)
	if numSubtrees <= 1 {
		// Single subtree: STUMP IS the full BUMP.
		computeMissingHashes(stumpPath)
		var txOffset uint64
		if internalHeight > 0 {
			for _, leaf := range stumpPath.Path[0] {
				if leaf.Txid != nil && *leaf.Txid {
					txOffset = leaf.Offset
					break
				}
			}
		}
		return stumpPath, txOffset, nil
	}

	// Multi-subtree: shift STUMP offsets from local (within subtree) to global (within block).
	for level := 0; level < internalHeight; level++ {
		shift := uint64(subtreeIndex) << uint(internalHeight-level) //nolint:gosec // safe
		for _, elem := range stumpPath.Path[level] {
			elem.Offset += shift
		}
	}

	var txOffset uint64
	if internalHeight > 0 {
		for _, leaf := range stumpPath.Path[0] {
			if leaf.Txid != nil && *leaf.Txid {
				txOffset = leaf.Offset
				break
			}
		}
	}

	// Build the full path: STUMP levels (global offsets) + subtree root layer
	subtreeRootLayer := int(math.Ceil(math.Log2(float64(numSubtrees))))
	totalHeight := internalHeight + subtreeRootLayer
	fullPath := &transaction.MerklePath{
		BlockHeight: stumpPath.BlockHeight,
		Path:        make([][]*transaction.PathElement, totalHeight),
	}

	for level := 0; level < internalHeight; level++ {
		for _, elem := range stumpPath.Path[level] {
			addLeaf(fullPath,level, elem)
		}
	}

	// Add subtree root hashes at the subtree root layer.
	for i, subHash := range subtreeHashes {
		if i == subtreeIndex {
			continue // our subtree root will be computed from STUMP leaves
		}
		hashCopy := subHash
		addLeaf(fullPath,internalHeight, &transaction.PathElement{
			Offset: uint64(i),
			Hash:   &hashCopy,
		})
	}

	computeMissingHashes(fullPath)
	return fullPath, txOffset, nil
}

// extractCoinbaseTxID parses a BRC-74 coinbase BUMP and returns the real
// coinbase txid (the hash at level 0, offset 0).
func extractCoinbaseTxID(coinbaseBUMP []byte) *chainhash.Hash {
	if len(coinbaseBUMP) == 0 {
		return nil
	}
	cbPath, err := transaction.NewMerklePathFromBinary(coinbaseBUMP)
	if err != nil || len(cbPath.Path) == 0 {
		return nil
	}
	for _, leaf := range cbPath.Path[0] {
		if leaf.Offset == 0 && leaf.Hash != nil {
			return leaf.Hash
		}
	}
	return nil
}

// applyCoinbaseToSTUMP replaces the coinbase placeholder at level 0, offset 0
// with the real coinbase txid, and removes any stale pre-computed hashes at
// offset 0 for higher levels so ComputeMissingHashes can recompute them correctly.
//
// For full STUMPs (all level-0 leaves present): replaces level 0 offset 0 and
// clears stale higher-level offset-0 hashes, letting ComputeMissingHashes rebuild them.
//
// For minimal STUMPs (only tracked tx path): if level 0 doesn't include offset 0,
// walks the coinbase BUMP to replace stale higher-level offset-0 hashes directly.
func applyCoinbaseToSTUMP(stumpPath *transaction.MerklePath, coinbaseTxID *chainhash.Hash, coinbaseBUMP []byte) {
	if coinbaseTxID == nil || len(stumpPath.Path) == 0 {
		return
	}

	// Check if level 0 has offset 0 (full STUMP includes the coinbase slot).
	level0HasOffset0 := false
	for _, elem := range stumpPath.Path[0] {
		if elem.Offset == 0 {
			h := *coinbaseTxID
			elem.Hash = &h
			level0HasOffset0 = true
			break
		}
	}

	if level0HasOffset0 {
		// Full STUMP: clear stale higher-level offset-0 hashes.
		// ComputeMissingHashes will recompute them from the corrected level-0 data.
		for level := 1; level < len(stumpPath.Path); level++ {
			filtered := make([]*transaction.PathElement, 0, len(stumpPath.Path[level]))
			for _, elem := range stumpPath.Path[level] {
				if elem.Offset != 0 {
					filtered = append(filtered, elem)
				}
			}
			stumpPath.Path[level] = filtered
		}
		return
	}

	// Minimal STUMP: level 0 doesn't contain offset 0, so we can't recompute
	// higher-level offset-0 hashes from level-0 data. Instead, walk the coinbase
	// BUMP to compute correct intermediate hashes and replace stale ones.
	cbPath, err := transaction.NewMerklePathFromBinary(coinbaseBUMP)
	if err != nil || len(cbPath.Path) == 0 {
		return
	}
	currentHash := coinbaseTxID
	internalHeight := len(stumpPath.Path)
	for level := 0; level < internalHeight && level < len(cbPath.Path); level++ {
		for _, elem := range stumpPath.Path[level] {
			if elem.Offset == 0 && (elem.Txid == nil || !*elem.Txid) {
				h := *currentHash
				elem.Hash = &h
				break
			}
		}
		sibling := findLeafByOffset(cbPath,level, 1)
		if sibling == nil || sibling.Hash == nil {
			break
		}
		currentHash = transaction.MerkleTreeParent(currentHash, sibling.Hash)
	}
}

// computeCorrectedSubtreeRoot parses a subtree 0 STUMP, replaces the placeholder
// with the real coinbase txid, builds the full tree, and returns the corrected root.
func computeCorrectedSubtreeRoot(stumpData []byte, coinbaseTxID *chainhash.Hash) (*chainhash.Hash, error) {
	stumpPath, err := transaction.NewMerklePathFromBinary(stumpData)
	if err != nil {
		return nil, err
	}
	applyCoinbaseToSTUMP(stumpPath, coinbaseTxID, nil)

	// Build full tree from level 0 to get the root.
	height := len(stumpPath.Path)
	fullTree := &transaction.MerklePath{
		BlockHeight: stumpPath.BlockHeight,
		Path:        make([][]*transaction.PathElement, height),
	}
	// Only copy level 0 (all leaves) — let ComputeMissingHashes build the rest.
	for _, elem := range stumpPath.Path[0] {
		addLeaf(fullTree,0, elem)
	}
	computeMissingHashes(fullTree)

	// The root is the single element at the highest computed level.
	topLevel := fullTree.Path[height-1]
	if len(topLevel) > 0 && topLevel[0].Hash != nil {
		return topLevel[0].Hash, nil
	}
	return nil, fmt.Errorf("failed to compute corrected subtree root")
}

// ExtractMinimalPath extracts the minimal set of nodes needed to verify a single
// transaction at the given offset from a full merkle path.
func ExtractMinimalPath(fullPath *transaction.MerklePath, txOffset uint64) *transaction.MerklePath {
	mp := &transaction.MerklePath{
		BlockHeight: fullPath.BlockHeight,
		Path:        make([][]*transaction.PathElement, len(fullPath.Path)),
	}

	offset := txOffset
	for level := 0; level < len(fullPath.Path); level++ {
		if level == 0 {
			if leaf := findLeafByOffset(fullPath,level, offset); leaf != nil {
				addLeaf(mp,level, leaf)
			}
		}
		if sibling := findLeafByOffset(fullPath,level, offset^1); sibling != nil {
			addLeaf(mp,level, sibling)
		}
		offset = offset >> 1
	}

	return mp
}

// ExtractLevel0Hashes parses a BRC-74 STUMP binary and returns all level-0 hashes.
func ExtractLevel0Hashes(stumpData []byte) []chainhash.Hash {
	mp, err := transaction.NewMerklePathFromBinary(stumpData)
	if err != nil || len(mp.Path) == 0 {
		return nil
	}

	hashes := make([]chainhash.Hash, 0, len(mp.Path[0]))
	for _, leaf := range mp.Path[0] {
		if leaf.Hash != nil {
			hashes = append(hashes, *leaf.Hash)
		}
	}
	return hashes
}

// BuildCompoundBUMP merges multiple per-subtree BUMPs into a single compound MerklePath
// containing all tracked transactions at level 0. The tracker is used to discover
// which level-0 hashes are tracked transactions.
func BuildCompoundBUMP(stumps []*models.Stump, subtreeHashes []chainhash.Hash, coinbaseBUMP []byte, tracker *store.TxTracker) (*transaction.MerklePath, []string, error) {
	if len(stumps) == 0 {
		return nil, nil, fmt.Errorf("no stumps to build compound BUMP")
	}

	coinbaseTxID := extractCoinbaseTxID(coinbaseBUMP)

	// Correct subtreeHashes[0] if coinbase is available.
	// The original subtreeHashes[0] may be computed from the placeholder, not the real coinbase.
	if coinbaseTxID != nil && len(subtreeHashes) > 0 {
		for _, stump := range stumps {
			if stump.SubtreeIndex == 0 {
				if correctedRoot, err := computeCorrectedSubtreeRoot(stump.StumpData, coinbaseTxID); err == nil {
					subtreeHashes[0] = *correctedRoot
				}
				break
			}
		}
	}

	// Assemble each STUMP into a full path, collect all elements by level
	var blockHeight uint32
	var txids []string
	allPaths := make([]*transaction.MerklePath, 0, len(stumps))

	for _, stump := range stumps {
		fullPath, _, err := assembleFullPath(stump.StumpData, stump.SubtreeIndex, subtreeHashes, coinbaseBUMP)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to assemble BUMP for subtree %d: %w", stump.SubtreeIndex, err)
		}
		blockHeight = fullPath.BlockHeight

		// Discover tracked txids from level-0 hashes
		level0 := ExtractLevel0Hashes(stump.StumpData)
		if tracker != nil {
			tracked := tracker.FilterTrackedHashes(level0)
			for _, h := range tracked {
				txids = append(txids, h.String())
			}
		}

		allPaths = append(allPaths, fullPath)
	}

	// Determine total height from the first path
	totalHeight := len(allPaths[0].Path)

	compound := &transaction.MerklePath{
		BlockHeight: blockHeight,
		Path:        make([][]*transaction.PathElement, totalHeight),
	}

	// Merge all path elements, deduplicating by (level, offset)
	type key struct {
		level  int
		offset uint64
	}
	seen := make(map[key]bool)

	for _, mp := range allPaths {
		for level := 0; level < len(mp.Path); level++ {
			for _, elem := range mp.Path[level] {
				k := key{level, elem.Offset}
				if seen[k] {
					continue
				}
				seen[k] = true
				addLeaf(compound,level, elem)
			}
		}
	}

	return compound, txids, nil
}
