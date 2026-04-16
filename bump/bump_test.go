package bump

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/bsv-blockchain/arcade/models"
	"github.com/bsv-blockchain/arcade/store"
)

// --- Test Helpers ---

// generateTxHashes produces n deterministic transaction hashes (SHA256 of big-endian index).
func generateTxHashes(n int) []chainhash.Hash {
	hashes := make([]chainhash.Hash, n)
	for i := range n {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(i))
		h := sha256.Sum256(buf[:])
		hash, _ := chainhash.NewHash(h[:])
		hashes[i] = *hash
	}
	return hashes
}

// buildMerkleTree computes all levels of a merkle tree from leaves.
// Returns tree[level][offset] where tree[0] = leaves and tree[len-1] = [root].
func buildMerkleTree(leaves []chainhash.Hash) [][]chainhash.Hash {
	if len(leaves) == 0 {
		return nil
	}

	tree := [][]chainhash.Hash{leaves}
	current := leaves

	for len(current) > 1 {
		if len(current)%2 == 1 {
			current = append(current, current[len(current)-1])
		}
		var next []chainhash.Hash
		for i := 0; i < len(current); i += 2 {
			parent := transaction.MerkleTreeParent(&current[i], &current[i+1])
			next = append(next, *parent)
		}
		tree = append(tree, next)
		current = next
	}

	return tree
}

// computeMerkleRoot returns the root of a merkle tree from leaves.
func computeMerkleRootFromLeaves(leaves []chainhash.Hash) chainhash.Hash {
	tree := buildMerkleTree(leaves)
	return tree[len(tree)-1][0]
}

// buildSTUMP constructs a minimal STUMP (subtree-level merkle path) for a transaction
// at the given offset in a subtree, serialized to BRC-74 binary.
func buildSTUMP(leaves []chainhash.Hash, txOffset uint64, blockHeight uint32) []byte {
	tree := buildMerkleTree(leaves)
	numLevels := len(tree) - 1
	if numLevels < 1 {
		numLevels = 1
	}

	mp := &transaction.MerklePath{
		BlockHeight: blockHeight,
		Path:        make([][]*transaction.PathElement, numLevels),
	}

	offset := txOffset
	for level := 0; level < numLevels; level++ {
		if level == 0 {
			txHash := tree[0][offset]
			isTxid := true
			mp.Path[0] = append(mp.Path[0], &transaction.PathElement{
				Offset: offset,
				Hash:   &txHash,
				Txid:   &isTxid,
			})
		}

		sibOffset := offset ^ 1
		levelHashes := tree[level]
		if len(levelHashes)%2 == 1 {
			levelHashes = append(levelHashes, levelHashes[len(levelHashes)-1])
		}
		if sibOffset < uint64(len(levelHashes)) {
			h := levelHashes[sibOffset]
			mp.Path[level] = append(mp.Path[level], &transaction.PathElement{
				Offset: sibOffset,
				Hash:   &h,
			})
		}

		offset = offset >> 1
	}

	return mp.Bytes()
}

// computeBlockMerkleRoot computes the block-level merkle root from subtree roots.
func computeBlockMerkleRoot(subtreeLeaves [][]chainhash.Hash) chainhash.Hash {
	subtreeRoots := make([]chainhash.Hash, len(subtreeLeaves))
	for i, leaves := range subtreeLeaves {
		subtreeRoots[i] = computeMerkleRootFromLeaves(leaves)
	}
	if len(subtreeRoots) == 1 {
		return subtreeRoots[0]
	}
	return computeMerkleRootFromLeaves(subtreeRoots)
}

// buildCoinbaseBUMP constructs a coinbase BUMP for the coinbase transaction in subtree 0.
func buildCoinbaseBUMP(subtree0Leaves []chainhash.Hash, coinbaseTxID chainhash.Hash, blockHeight uint32) []byte {
	trueLeaves := make([]chainhash.Hash, len(subtree0Leaves))
	copy(trueLeaves, subtree0Leaves)
	trueLeaves[0] = coinbaseTxID
	return buildSTUMP(trueLeaves, 0, blockHeight)
}

// buildFullSTUMP constructs a STUMP containing ALL level-0 hashes for a subtree.
func buildFullSTUMP(leaves []chainhash.Hash, txOffset uint64, blockHeight uint32) []byte {
	tree := buildMerkleTree(leaves)
	numLevels := len(tree) - 1
	if numLevels < 1 {
		numLevels = 1
	}

	mp := &transaction.MerklePath{
		BlockHeight: blockHeight,
		Path:        make([][]*transaction.PathElement, numLevels),
	}

	for i, h := range leaves {
		hashCopy := h
		isTxid := (uint64(i) == txOffset)
		mp.Path[0] = append(mp.Path[0], &transaction.PathElement{
			Offset: uint64(i),
			Hash:   &hashCopy,
			Txid:   &isTxid,
		})
	}

	offset := txOffset
	for level := 1; level < numLevels; level++ {
		offset = offset >> 1
		sibOffset := offset ^ 1
		levelHashes := tree[level]
		if len(levelHashes)%2 == 1 {
			levelHashes = append(levelHashes, levelHashes[len(levelHashes)-1])
		}
		if sibOffset < uint64(len(levelHashes)) {
			h := levelHashes[sibOffset]
			mp.Path[level] = append(mp.Path[level], &transaction.PathElement{
				Offset: sibOffset,
				Hash:   &h,
			})
		}
	}

	return mp.Bytes()
}

// multiSubtreeTestSetup creates a block with numSubtrees subtrees of subtreeSize txs each.
func multiSubtreeTestSetup(numSubtrees, subtreeSize int) (allLeaves [][]chainhash.Hash, subtreeHashes []chainhash.Hash, blockRoot chainhash.Hash) {
	allLeaves = make([][]chainhash.Hash, numSubtrees)
	subtreeHashes = make([]chainhash.Hash, numSubtrees)

	offset := 0
	for s := range numSubtrees {
		leaves := make([]chainhash.Hash, subtreeSize)
		for i := range subtreeSize {
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], uint64(offset+i+1000*s))
			h := sha256.Sum256(buf[:])
			hash, _ := chainhash.NewHash(h[:])
			leaves[i] = *hash
		}
		allLeaves[s] = leaves
		subtreeHashes[s] = computeMerkleRootFromLeaves(leaves)
		offset += subtreeSize
	}

	blockRoot = computeMerkleRootFromLeaves(subtreeHashes)
	return
}

// setupCoinbaseBlock creates a block with a coinbase placeholder at subtree 0, offset 0.
func setupCoinbaseBlock(numSubtrees, subtreeSize int) (
	allLeaves [][]chainhash.Hash,
	trueAllLeaves [][]chainhash.Hash,
	subtreeHashes []chainhash.Hash,
	coinbaseTxID chainhash.Hash,
	trueBlockRoot chainhash.Hash,
) {
	placeholder := chainhash.Hash{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	coinbaseTxID = generateTxHashes(1)[0]

	allLeaves = make([][]chainhash.Hash, numSubtrees)
	trueAllLeaves = make([][]chainhash.Hash, numSubtrees)

	for s := range numSubtrees {
		allLeaves[s] = make([]chainhash.Hash, subtreeSize)
		trueAllLeaves[s] = make([]chainhash.Hash, subtreeSize)
		for i := range subtreeSize {
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], uint64(5000+s*100+i))
			h := sha256.Sum256(buf[:])
			hash, _ := chainhash.NewHash(h[:])
			allLeaves[s][i] = *hash
			trueAllLeaves[s][i] = *hash
		}
	}

	allLeaves[0][0] = placeholder
	trueAllLeaves[0][0] = coinbaseTxID

	subtreeHashes = make([]chainhash.Hash, numSubtrees)
	for s := range numSubtrees {
		subtreeHashes[s] = computeMerkleRootFromLeaves(allLeaves[s])
	}

	trueBlockRoot = computeBlockMerkleRoot(trueAllLeaves)
	return
}

// --- Sanity Tests for Helpers ---

func TestBuildMerkleTree_SanityCheck(t *testing.T) {
	leaves := generateTxHashes(4)
	ourRoot := computeMerkleRootFromLeaves(leaves)

	height := int(math.Ceil(math.Log2(float64(len(leaves)))))
	mp := &transaction.MerklePath{
		BlockHeight: 100,
		Path:        make([][]*transaction.PathElement, height),
	}
	for i, h := range leaves {
		hashCopy := h
		isTxid := true
		addLeaf(mp, 0, &transaction.PathElement{
			Offset: uint64(i),
			Hash:   &hashCopy,
			Txid:   &isTxid,
		})
	}
	computeMissingHashes(mp)
	sdkRoot, err := mp.ComputeRoot(&leaves[0])
	if err != nil {
		t.Fatalf("ComputeRoot failed: %v", err)
	}

	if ourRoot != *sdkRoot {
		t.Fatalf("buildMerkleTree root %s != go-sdk root %s", ourRoot, sdkRoot)
	}
}

func TestBuildMerkleTree_OddCount(t *testing.T) {
	leaves := generateTxHashes(3)
	ourRoot := computeMerkleRootFromLeaves(leaves)

	// For 3 leaves, our buildMerkleTree duplicates the last to get 4,
	// so height = log2(4) = 2. Verify root via a STUMP round-trip.
	stump := buildSTUMP(leaves, 0, 100)
	result, _, err := AssembleBUMP(stump, 0, []chainhash.Hash{ourRoot}, nil)
	if err != nil {
		t.Fatalf("AssembleBUMP failed: %v", err)
	}
	sdkRoot, err := result.ComputeRoot(&leaves[0])
	if err != nil {
		t.Fatalf("ComputeRoot failed: %v", err)
	}

	if ourRoot != *sdkRoot {
		t.Fatalf("buildMerkleTree root (odd) %s != assembled root %s", ourRoot, sdkRoot)
	}
}

// --- Single-Subtree Tests ---

func TestAssembleBUMP_SingleSubtree_2txs_Offset1(t *testing.T) {
	leaves := generateTxHashes(2)
	expectedRoot := computeMerkleRootFromLeaves(leaves)
	subtreeHashes := []chainhash.Hash{expectedRoot}

	stump := buildSTUMP(leaves, 1, 800000)
	result, _, err := AssembleBUMP(stump, 0, subtreeHashes, nil)
	if err != nil {
		t.Fatalf("AssembleBUMP failed: %v", err)
	}

	root, err := result.ComputeRoot(&leaves[1])
	if err != nil {
		t.Fatalf("ComputeRoot failed: %v", err)
	}
	if *root != expectedRoot {
		t.Fatalf("root mismatch: got %s, want %s", root, expectedRoot)
	}
}

func TestAssembleBUMP_SingleSubtree_4txs_Offset0(t *testing.T) {
	leaves := generateTxHashes(4)
	expectedRoot := computeMerkleRootFromLeaves(leaves)
	subtreeHashes := []chainhash.Hash{expectedRoot}

	stump := buildSTUMP(leaves, 0, 800001)
	result, _, err := AssembleBUMP(stump, 0, subtreeHashes, nil)
	if err != nil {
		t.Fatalf("AssembleBUMP failed: %v", err)
	}

	root, err := result.ComputeRoot(&leaves[0])
	if err != nil {
		t.Fatalf("ComputeRoot failed: %v", err)
	}
	if *root != expectedRoot {
		t.Fatalf("root mismatch: got %s, want %s", root, expectedRoot)
	}
}

func TestAssembleBUMP_SingleSubtree_8txs_LastOffset(t *testing.T) {
	leaves := generateTxHashes(8)
	expectedRoot := computeMerkleRootFromLeaves(leaves)
	subtreeHashes := []chainhash.Hash{expectedRoot}

	stump := buildSTUMP(leaves, 7, 800002)
	result, _, err := AssembleBUMP(stump, 0, subtreeHashes, nil)
	if err != nil {
		t.Fatalf("AssembleBUMP failed: %v", err)
	}

	root, err := result.ComputeRoot(&leaves[7])
	if err != nil {
		t.Fatalf("ComputeRoot failed: %v", err)
	}
	if *root != expectedRoot {
		t.Fatalf("root mismatch: got %s, want %s", root, expectedRoot)
	}
}

func TestAssembleBUMP_SingleSubtree_16txs_Middle(t *testing.T) {
	leaves := generateTxHashes(16)
	expectedRoot := computeMerkleRootFromLeaves(leaves)
	subtreeHashes := []chainhash.Hash{expectedRoot}

	stump := buildSTUMP(leaves, 5, 800003)
	result, _, err := AssembleBUMP(stump, 0, subtreeHashes, nil)
	if err != nil {
		t.Fatalf("AssembleBUMP failed: %v", err)
	}

	root, err := result.ComputeRoot(&leaves[5])
	if err != nil {
		t.Fatalf("ComputeRoot failed: %v", err)
	}
	if *root != expectedRoot {
		t.Fatalf("root mismatch: got %s, want %s", root, expectedRoot)
	}
}

// --- Multi-Subtree Tests ---

func TestAssembleBUMP_2Subtrees_TrackedInSubtree1(t *testing.T) {
	allLeaves, subtreeHashes, blockRoot := multiSubtreeTestSetup(2, 4)

	txOffset := uint64(2)
	stump := buildSTUMP(allLeaves[1], txOffset, 900000)

	result, _, err := AssembleBUMP(stump, 1, subtreeHashes, nil)
	if err != nil {
		t.Fatalf("AssembleBUMP failed: %v", err)
	}

	root, err := result.ComputeRoot(&allLeaves[1][txOffset])
	if err != nil {
		t.Fatalf("ComputeRoot failed: %v", err)
	}
	if *root != blockRoot {
		t.Fatalf("root mismatch: got %s, want %s", root, blockRoot)
	}
}

func TestAssembleBUMP_4Subtrees_TrackedInSubtree2(t *testing.T) {
	allLeaves, subtreeHashes, blockRoot := multiSubtreeTestSetup(4, 4)

	txOffset := uint64(1)
	stump := buildSTUMP(allLeaves[2], txOffset, 900001)

	result, _, err := AssembleBUMP(stump, 2, subtreeHashes, nil)
	if err != nil {
		t.Fatalf("AssembleBUMP failed: %v", err)
	}

	root, err := result.ComputeRoot(&allLeaves[2][txOffset])
	if err != nil {
		t.Fatalf("ComputeRoot failed: %v", err)
	}
	if *root != blockRoot {
		t.Fatalf("root mismatch: got %s, want %s", root, blockRoot)
	}
}

func TestAssembleBUMP_8Subtrees_TrackedInSubtree5(t *testing.T) {
	allLeaves, subtreeHashes, blockRoot := multiSubtreeTestSetup(8, 4)

	txOffset := uint64(3)
	stump := buildSTUMP(allLeaves[5], txOffset, 900002)

	subtreeRootLayer := int(math.Ceil(math.Log2(float64(len(subtreeHashes)))))
	if subtreeRootLayer != 3 {
		t.Fatalf("expected subtreeRootLayer=3, got %d", subtreeRootLayer)
	}

	result, _, err := AssembleBUMP(stump, 5, subtreeHashes, nil)
	if err != nil {
		t.Fatalf("AssembleBUMP failed: %v", err)
	}

	root, err := result.ComputeRoot(&allLeaves[5][txOffset])
	if err != nil {
		t.Fatalf("ComputeRoot failed: %v", err)
	}
	if *root != blockRoot {
		t.Fatalf("root mismatch: got %s, want %s", root, blockRoot)
	}
}

func TestAssembleBUMP_2Subtrees_DifferentSizes(t *testing.T) {
	leaves0 := generateTxHashes(8)
	leaves1 := make([]chainhash.Hash, 4)
	for i := range 4 {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(100+i))
		h := sha256.Sum256(buf[:])
		hash, _ := chainhash.NewHash(h[:])
		leaves1[i] = *hash
	}

	subtreeHashes := []chainhash.Hash{
		computeMerkleRootFromLeaves(leaves0),
		computeMerkleRootFromLeaves(leaves1),
	}
	blockRoot := computeMerkleRootFromLeaves(subtreeHashes)

	txOffset := uint64(2)
	stump := buildSTUMP(leaves1, txOffset, 900003)

	result, _, err := AssembleBUMP(stump, 1, subtreeHashes, nil)
	if err != nil {
		t.Fatalf("AssembleBUMP failed: %v", err)
	}

	root, err := result.ComputeRoot(&leaves1[txOffset])
	if err != nil {
		t.Fatalf("ComputeRoot failed: %v", err)
	}
	if *root != blockRoot {
		t.Fatalf("root mismatch: got %s, want %s", root, blockRoot)
	}
}

// --- Coinbase Placeholder Tests ---

func TestAssembleBUMP_Subtree0_CoinbaseReplacement(t *testing.T) {
	placeholder := chainhash.Hash{0xff, 0xff, 0xff, 0xff}
	coinbaseTxID := generateTxHashes(1)[0]

	subtree0Leaves := make([]chainhash.Hash, 4)
	subtree0Leaves[0] = placeholder
	for i := 1; i < 4; i++ {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(200+i))
		h := sha256.Sum256(buf[:])
		hash, _ := chainhash.NewHash(h[:])
		subtree0Leaves[i] = *hash
	}

	subtree1Leaves := make([]chainhash.Hash, 4)
	for i := range 4 {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(300+i))
		h := sha256.Sum256(buf[:])
		hash, _ := chainhash.NewHash(h[:])
		subtree1Leaves[i] = *hash
	}

	stump := buildSTUMP(subtree0Leaves, 1, 950000)

	trueSubtree0Leaves := make([]chainhash.Hash, 4)
	copy(trueSubtree0Leaves, subtree0Leaves)
	trueSubtree0Leaves[0] = coinbaseTxID

	subtreeHashes := []chainhash.Hash{
		computeMerkleRootFromLeaves(subtree0Leaves),
		computeMerkleRootFromLeaves(subtree1Leaves),
	}

	trueBlockRoot := computeBlockMerkleRoot([][]chainhash.Hash{trueSubtree0Leaves, subtree1Leaves})

	cbBUMP := buildCoinbaseBUMP(subtree0Leaves, coinbaseTxID, 950000)
	result, _, err := AssembleBUMP(stump, 0, subtreeHashes, cbBUMP)
	if err != nil {
		t.Fatalf("AssembleBUMP failed: %v", err)
	}

	root, err := result.ComputeRoot(&subtree0Leaves[1])
	if err != nil {
		t.Fatalf("ComputeRoot failed: %v", err)
	}
	if *root != trueBlockRoot {
		t.Fatalf("root mismatch with coinbase replacement: got %s, want %s", root, trueBlockRoot)
	}
}

func TestAssembleBUMP_Subtree0_CoinbaseReplacement_Offset3(t *testing.T) {
	placeholder := chainhash.Hash{0xff, 0xff, 0xff, 0xff}
	coinbaseTxID := generateTxHashes(1)[0]

	subtree0Leaves := make([]chainhash.Hash, 4)
	subtree0Leaves[0] = placeholder
	for i := 1; i < 4; i++ {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(400+i))
		h := sha256.Sum256(buf[:])
		hash, _ := chainhash.NewHash(h[:])
		subtree0Leaves[i] = *hash
	}

	subtree1Leaves := generateTxHashes(4)

	stump := buildSTUMP(subtree0Leaves, 3, 950001)

	trueSubtree0Leaves := make([]chainhash.Hash, 4)
	copy(trueSubtree0Leaves, subtree0Leaves)
	trueSubtree0Leaves[0] = coinbaseTxID

	subtreeHashes := []chainhash.Hash{
		computeMerkleRootFromLeaves(subtree0Leaves),
		computeMerkleRootFromLeaves(subtree1Leaves),
	}

	trueBlockRoot := computeBlockMerkleRoot([][]chainhash.Hash{trueSubtree0Leaves, subtree1Leaves})

	cbBUMP := buildCoinbaseBUMP(subtree0Leaves, coinbaseTxID, 950001)
	result, _, err := AssembleBUMP(stump, 0, subtreeHashes, cbBUMP)
	if err != nil {
		t.Fatalf("AssembleBUMP failed: %v", err)
	}

	root, err := result.ComputeRoot(&subtree0Leaves[3])
	if err != nil {
		t.Fatalf("ComputeRoot failed: %v", err)
	}
	if *root != trueBlockRoot {
		t.Fatalf("root mismatch: got %s, want %s", root, trueBlockRoot)
	}
}

func TestAssembleBUMP_Subtree0_NoCoinbase(t *testing.T) {
	placeholder := chainhash.Hash{0xff, 0xff, 0xff, 0xff}
	coinbaseTxID := generateTxHashes(1)[0]

	subtree0Leaves := make([]chainhash.Hash, 4)
	subtree0Leaves[0] = placeholder
	for i := 1; i < 4; i++ {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(500+i))
		h := sha256.Sum256(buf[:])
		hash, _ := chainhash.NewHash(h[:])
		subtree0Leaves[i] = *hash
	}
	subtree1Leaves := generateTxHashes(4)

	stump := buildSTUMP(subtree0Leaves, 1, 950002)

	trueSubtree0Leaves := make([]chainhash.Hash, 4)
	copy(trueSubtree0Leaves, subtree0Leaves)
	trueSubtree0Leaves[0] = coinbaseTxID

	subtreeHashes := []chainhash.Hash{
		computeMerkleRootFromLeaves(subtree0Leaves),
		computeMerkleRootFromLeaves(subtree1Leaves),
	}
	trueBlockRoot := computeBlockMerkleRoot([][]chainhash.Hash{trueSubtree0Leaves, subtree1Leaves})

	result, _, err := AssembleBUMP(stump, 0, subtreeHashes, nil)
	if err != nil {
		t.Fatalf("AssembleBUMP failed: %v", err)
	}

	root, err := result.ComputeRoot(&subtree0Leaves[1])
	if err != nil {
		t.Fatalf("ComputeRoot failed: %v", err)
	}
	if *root == trueBlockRoot {
		t.Fatal("expected root to NOT match true block root when coinbase is nil (placeholder differs)")
	}
}

func TestAssembleBUMP_4Subtrees_Subtree0_CoinbaseReplacement(t *testing.T) {
	placeholder := chainhash.Hash{0xff, 0xff, 0xff, 0xff}
	coinbaseTxID := generateTxHashes(1)[0]

	allLeaves := make([][]chainhash.Hash, 4)
	allLeaves[0] = make([]chainhash.Hash, 4)
	allLeaves[0][0] = placeholder
	for i := 1; i < 4; i++ {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(600+i))
		h := sha256.Sum256(buf[:])
		hash, _ := chainhash.NewHash(h[:])
		allLeaves[0][i] = *hash
	}
	for s := 1; s < 4; s++ {
		allLeaves[s] = make([]chainhash.Hash, 4)
		for i := range 4 {
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], uint64(600+s*100+i))
			h := sha256.Sum256(buf[:])
			hash, _ := chainhash.NewHash(h[:])
			allLeaves[s][i] = *hash
		}
	}

	subtreeHashes := make([]chainhash.Hash, 4)
	for s := range 4 {
		subtreeHashes[s] = computeMerkleRootFromLeaves(allLeaves[s])
	}

	stump := buildSTUMP(allLeaves[0], 2, 950003)

	trueAllLeaves := make([][]chainhash.Hash, 4)
	for s := range 4 {
		trueAllLeaves[s] = make([]chainhash.Hash, len(allLeaves[s]))
		copy(trueAllLeaves[s], allLeaves[s])
	}
	trueAllLeaves[0][0] = coinbaseTxID

	trueBlockRoot := computeBlockMerkleRoot(trueAllLeaves)

	cbBUMP := buildCoinbaseBUMP(allLeaves[0], coinbaseTxID, 950003)
	result, _, err := AssembleBUMP(stump, 0, subtreeHashes, cbBUMP)
	if err != nil {
		t.Fatalf("AssembleBUMP failed: %v", err)
	}

	root, err := result.ComputeRoot(&allLeaves[0][2])
	if err != nil {
		t.Fatalf("ComputeRoot failed: %v", err)
	}
	if *root != trueBlockRoot {
		t.Fatalf("root mismatch: got %s, want %s", root, trueBlockRoot)
	}
}

// --- Edge Cases ---

func TestAssembleBUMP_OddSubtreeSize(t *testing.T) {
	leaves := generateTxHashes(3)
	expectedRoot := computeMerkleRootFromLeaves(leaves)
	subtreeHashes := []chainhash.Hash{expectedRoot}

	stump := buildSTUMP(leaves, 1, 960000)

	result, _, err := AssembleBUMP(stump, 0, subtreeHashes, nil)
	if err != nil {
		t.Fatalf("AssembleBUMP failed: %v", err)
	}

	root, err := result.ComputeRoot(&leaves[1])
	if err != nil {
		t.Fatalf("ComputeRoot failed: %v", err)
	}
	if *root != expectedRoot {
		t.Fatalf("root mismatch: got %s, want %s", root, expectedRoot)
	}
}

func TestAssembleBUMP_TwoTxs_DifferentSubtrees_SameRoot(t *testing.T) {
	allLeaves, subtreeHashes, blockRoot := multiSubtreeTestSetup(4, 4)

	stump1 := buildSTUMP(allLeaves[1], 2, 970000)
	result1, _, err := AssembleBUMP(stump1, 1, subtreeHashes, nil)
	if err != nil {
		t.Fatalf("AssembleBUMP (subtree 1) failed: %v", err)
	}

	stump3 := buildSTUMP(allLeaves[3], 0, 970000)
	result3, _, err := AssembleBUMP(stump3, 3, subtreeHashes, nil)
	if err != nil {
		t.Fatalf("AssembleBUMP (subtree 3) failed: %v", err)
	}

	root1, err := result1.ComputeRoot(&allLeaves[1][2])
	if err != nil {
		t.Fatalf("ComputeRoot (subtree 1) failed: %v", err)
	}
	root3, err := result3.ComputeRoot(&allLeaves[3][0])
	if err != nil {
		t.Fatalf("ComputeRoot (subtree 3) failed: %v", err)
	}

	if *root1 != blockRoot {
		t.Fatalf("subtree 1 root mismatch: got %s, want %s", root1, blockRoot)
	}
	if *root3 != blockRoot {
		t.Fatalf("subtree 3 root mismatch: got %s, want %s", root3, blockRoot)
	}
	if *root1 != *root3 {
		t.Fatalf("roots should be equal: %s vs %s", root1, root3)
	}
}

func TestAssembleBUMP_TwoTxs_SameSubtree_SameRoot(t *testing.T) {
	allLeaves, subtreeHashes, blockRoot := multiSubtreeTestSetup(2, 4)

	stumpA := buildSTUMP(allLeaves[1], 0, 970001)
	resultA, _, err := AssembleBUMP(stumpA, 1, subtreeHashes, nil)
	if err != nil {
		t.Fatalf("AssembleBUMP (offset 0) failed: %v", err)
	}

	stumpB := buildSTUMP(allLeaves[1], 3, 970001)
	resultB, _, err := AssembleBUMP(stumpB, 1, subtreeHashes, nil)
	if err != nil {
		t.Fatalf("AssembleBUMP (offset 3) failed: %v", err)
	}

	rootA, err := resultA.ComputeRoot(&allLeaves[1][0])
	if err != nil {
		t.Fatalf("ComputeRoot (offset 0) failed: %v", err)
	}
	rootB, err := resultB.ComputeRoot(&allLeaves[1][3])
	if err != nil {
		t.Fatalf("ComputeRoot (offset 3) failed: %v", err)
	}

	if *rootA != blockRoot {
		t.Fatalf("offset 0 root mismatch: got %s, want %s", rootA, blockRoot)
	}
	if *rootB != blockRoot {
		t.Fatalf("offset 3 root mismatch: got %s, want %s", rootB, blockRoot)
	}
}

// --- Compound BUMP Tests ---

func TestBuildCompoundBUMP_AllTxsExtractable(t *testing.T) {
	allLeaves, subtreeHashes, blockRoot := multiSubtreeTestSetup(2, 4)

	stump0 := buildFullSTUMP(allLeaves[0], 0, 990000)
	stump1 := buildFullSTUMP(allLeaves[1], 0, 990000)

	stumps := []*models.Stump{
		{BlockHash: "blockhash", SubtreeIndex: 0, StumpData: stump0},
		{BlockHash: "blockhash", SubtreeIndex: 1, StumpData: stump1},
	}

	tracker := store.NewTxTracker()
	for s := range 2 {
		for i := range 4 {
			tracker.AddHash(allLeaves[s][i], models.StatusSeenOnNetwork)
		}
	}

	compound, txids, err := BuildCompoundBUMP(stumps, subtreeHashes, nil, tracker)
	if err != nil {
		t.Fatalf("BuildCompoundBUMP failed: %v", err)
	}

	if len(txids) != 8 {
		t.Fatalf("expected 8 tracked txids, got %d", len(txids))
	}

	bumpData := compound.Bytes()

	for s := range 2 {
		for i := range 4 {
			txHash := allLeaves[s][i]
			txid := txHash.String()

			parsed, parseErr := transaction.NewMerklePathFromBinary(bumpData)
			if parseErr != nil {
				t.Fatalf("failed to parse compound BUMP: %v", parseErr)
			}

			var txOffset uint64
			found := false
			for _, leaf := range parsed.Path[0] {
				if leaf.Hash != nil && *leaf.Hash == txHash {
					txOffset = leaf.Offset
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("subtree %d tx %d (%s) not found in compound BUMP level 0", s, i, txid)
			}

			minimal := ExtractMinimalPath(parsed, txOffset)
			root, rootErr := minimal.ComputeRoot(&txHash)
			if rootErr != nil {
				t.Fatalf("ComputeRoot failed for subtree %d tx %d: %v", s, i, rootErr)
			}
			if *root != blockRoot {
				t.Fatalf("root mismatch for subtree %d tx %d: got %s, want %s", s, i, root, blockRoot)
			}
		}
	}
}

func TestAssembleBUMP_LargeBlock_16Subtrees_32Txs(t *testing.T) {
	allLeaves, subtreeHashes, blockRoot := multiSubtreeTestSetup(16, 32)

	txOffset := uint64(17)
	stump := buildSTUMP(allLeaves[11], txOffset, 980000)

	result, _, err := AssembleBUMP(stump, 11, subtreeHashes, nil)
	if err != nil {
		t.Fatalf("AssembleBUMP failed: %v", err)
	}

	root, err := result.ComputeRoot(&allLeaves[11][txOffset])
	if err != nil {
		t.Fatalf("ComputeRoot failed: %v", err)
	}
	if *root != blockRoot {
		t.Fatalf("root mismatch: got %s, want %s", root, blockRoot)
	}
}

// --- Coinbase Replacement in Compound BUMP Tests ---

func TestBuildCompoundBUMP_CoinbaseReplacement(t *testing.T) {
	allLeaves, trueAllLeaves, subtreeHashes, coinbaseTxID, trueBlockRoot := setupCoinbaseBlock(2, 4)
	placeholder := allLeaves[0][0]

	stump0 := buildFullSTUMP(allLeaves[0], 1, 1000000)
	stump1 := buildFullSTUMP(allLeaves[1], 2, 1000000)

	stumps := []*models.Stump{
		{BlockHash: "block1", SubtreeIndex: 0, StumpData: stump0},
		{BlockHash: "block1", SubtreeIndex: 1, StumpData: stump1},
	}

	tracker := store.NewTxTracker()
	for s := range 2 {
		for i := range 4 {
			tracker.AddHash(allLeaves[s][i], models.StatusSeenOnNetwork)
		}
	}
	tracker.AddHash(coinbaseTxID, models.StatusSeenOnNetwork)

	cbBUMP := buildCoinbaseBUMP(allLeaves[0], coinbaseTxID, 1000000)

	compound, txids, err := BuildCompoundBUMP(stumps, subtreeHashes, cbBUMP, tracker)
	if err != nil {
		t.Fatalf("BuildCompoundBUMP failed: %v", err)
	}

	if len(txids) == 0 {
		t.Fatal("expected tracked txids, got none")
	}

	bumpData := compound.Bytes()
	parsed, err := transaction.NewMerklePathFromBinary(bumpData)
	if err != nil {
		t.Fatalf("failed to parse compound BUMP: %v", err)
	}
	for _, leaf := range parsed.Path[0] {
		if leaf.Offset == 0 {
			if leaf.Hash != nil && *leaf.Hash == placeholder {
				t.Fatal("placeholder still present at level 0 offset 0")
			}
			if leaf.Hash == nil || *leaf.Hash != coinbaseTxID {
				t.Fatalf("expected coinbase txid at level 0 offset 0, got %v", leaf.Hash)
			}
			break
		}
	}

	for s := range 2 {
		for i := range 4 {
			txHash := trueAllLeaves[s][i]
			parsed, err := transaction.NewMerklePathFromBinary(bumpData)
			if err != nil {
				t.Fatalf("failed to parse compound BUMP: %v", err)
			}
			var txOffset uint64
			found := false
			for _, leaf := range parsed.Path[0] {
				if leaf.Hash != nil && *leaf.Hash == txHash {
					txOffset = leaf.Offset
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("subtree %d tx %d not found in compound BUMP", s, i)
			}
			minimal := ExtractMinimalPath(parsed, txOffset)
			root, err := minimal.ComputeRoot(&txHash)
			if err != nil {
				t.Fatalf("ComputeRoot failed for subtree %d tx %d: %v", s, i, err)
			}
			if *root != trueBlockRoot {
				t.Fatalf("root mismatch for subtree %d tx %d: got %s, want %s", s, i, root, trueBlockRoot)
			}
		}
	}
}

func TestBuildCompoundBUMP_CoinbaseReplacement_SingleSubtree(t *testing.T) {
	allLeaves, trueAllLeaves, subtreeHashes, coinbaseTxID, trueBlockRoot := setupCoinbaseBlock(1, 4)
	placeholder := allLeaves[0][0]

	stump0 := buildFullSTUMP(allLeaves[0], 2, 1000001)

	stumps := []*models.Stump{
		{BlockHash: "block2", SubtreeIndex: 0, StumpData: stump0},
	}

	tracker := store.NewTxTracker()
	for i := range 4 {
		tracker.AddHash(allLeaves[0][i], models.StatusSeenOnNetwork)
	}
	tracker.AddHash(coinbaseTxID, models.StatusSeenOnNetwork)

	cbBUMP := buildCoinbaseBUMP(allLeaves[0], coinbaseTxID, 1000001)

	compound, _, err := BuildCompoundBUMP(stumps, subtreeHashes, cbBUMP, tracker)
	if err != nil {
		t.Fatalf("BuildCompoundBUMP failed: %v", err)
	}

	bumpData := compound.Bytes()
	parsed, err := transaction.NewMerklePathFromBinary(bumpData)
	if err != nil {
		t.Fatalf("failed to parse compound BUMP: %v", err)
	}

	for _, leaf := range parsed.Path[0] {
		if leaf.Offset == 0 {
			if leaf.Hash != nil && *leaf.Hash == placeholder {
				t.Fatal("placeholder still present at level 0 offset 0")
			}
			break
		}
	}

	for i := range 4 {
		txHash := trueAllLeaves[0][i]
		parsed, err := transaction.NewMerklePathFromBinary(bumpData)
		if err != nil {
			t.Fatalf("failed to parse compound BUMP: %v", err)
		}
		var txOffset uint64
		found := false
		for _, leaf := range parsed.Path[0] {
			if leaf.Hash != nil && *leaf.Hash == txHash {
				txOffset = leaf.Offset
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("tx %d not found in compound BUMP", i)
		}
		minimal := ExtractMinimalPath(parsed, txOffset)
		root, err := minimal.ComputeRoot(&txHash)
		if err != nil {
			t.Fatalf("ComputeRoot failed for tx %d: %v", i, err)
		}
		if *root != trueBlockRoot {
			t.Fatalf("root mismatch for tx %d: got %s, want %s", i, root, trueBlockRoot)
		}
	}
}

func TestApplyCoinbaseToSTUMP_ClearsStaleHashes(t *testing.T) {
	placeholder := chainhash.Hash{0xff, 0xff, 0xff, 0xff}
	coinbaseTxID := generateTxHashes(1)[0]

	leaves := make([]chainhash.Hash, 4)
	leaves[0] = placeholder
	for i := 1; i < 4; i++ {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(7000+i))
		h := sha256.Sum256(buf[:])
		hash, _ := chainhash.NewHash(h[:])
		leaves[i] = *hash
	}

	tree := buildMerkleTree(leaves)

	numLevels := len(tree) - 1
	mp := &transaction.MerklePath{
		BlockHeight: 100,
		Path:        make([][]*transaction.PathElement, numLevels),
	}

	for i, h := range leaves {
		hashCopy := h
		isTxid := (i == 1)
		mp.Path[0] = append(mp.Path[0], &transaction.PathElement{
			Offset: uint64(i),
			Hash:   &hashCopy,
			Txid:   &isTxid,
		})
	}

	for level := 1; level < numLevels; level++ {
		staleHash := tree[level][0]
		mp.Path[level] = append(mp.Path[level], &transaction.PathElement{
			Offset: 0,
			Hash:   &staleHash,
		})
	}

	applyCoinbaseToSTUMP(mp, &coinbaseTxID, nil)

	found := false
	for _, leaf := range mp.Path[0] {
		if leaf.Offset == 0 {
			if leaf.Hash == nil || *leaf.Hash != coinbaseTxID {
				t.Fatalf("expected coinbase at level 0 offset 0, got %v", leaf.Hash)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no element at level 0 offset 0")
	}

	for level := 1; level < numLevels; level++ {
		for _, elem := range mp.Path[level] {
			if elem.Offset == 0 {
				t.Fatalf("stale hash at level %d offset 0 was not removed", level)
			}
		}
	}
}

// --- ExtractMinimalPathForTx Tests ---

func TestExtractMinimalPathForTx_AllTxsExtractable(t *testing.T) {
	allLeaves, subtreeHashes, blockRoot := multiSubtreeTestSetup(2, 4)

	stump0 := buildFullSTUMP(allLeaves[0], 0, 990000)
	stump1 := buildFullSTUMP(allLeaves[1], 0, 990000)

	stumps := []*models.Stump{
		{BlockHash: "blockhash", SubtreeIndex: 0, StumpData: stump0},
		{BlockHash: "blockhash", SubtreeIndex: 1, StumpData: stump1},
	}

	tracker := store.NewTxTracker()
	for s := range 2 {
		for i := range 4 {
			tracker.AddHash(allLeaves[s][i], models.StatusSeenOnNetwork)
		}
	}

	compound, _, err := BuildCompoundBUMP(stumps, subtreeHashes, nil, tracker)
	if err != nil {
		t.Fatalf("BuildCompoundBUMP failed: %v", err)
	}

	bumpData := compound.Bytes()

	for s := range 2 {
		for i := range 4 {
			txHash := allLeaves[s][i]
			txid := txHash.String()

			minimalBytes := ExtractMinimalPathForTx(bumpData, txid)
			if minimalBytes == nil {
				t.Fatalf("ExtractMinimalPathForTx returned nil for subtree %d tx %d (%s)", s, i, txid)
			}

			minimal, err := transaction.NewMerklePathFromBinary(minimalBytes)
			if err != nil {
				t.Fatalf("failed to parse minimal path for subtree %d tx %d: %v", s, i, err)
			}

			root, err := minimal.ComputeRoot(&txHash)
			if err != nil {
				t.Fatalf("ComputeRoot failed for subtree %d tx %d: %v", s, i, err)
			}
			if *root != blockRoot {
				t.Fatalf("root mismatch for subtree %d tx %d: got %s, want %s", s, i, root, blockRoot)
			}
		}
	}
}

func TestExtractMinimalPathForTx_TxNotFound(t *testing.T) {
	allLeaves, subtreeHashes, _ := multiSubtreeTestSetup(2, 4)

	stump0 := buildFullSTUMP(allLeaves[0], 0, 990000)
	stumps := []*models.Stump{
		{BlockHash: "blockhash", SubtreeIndex: 0, StumpData: stump0},
	}

	tracker := store.NewTxTracker()
	for i := range 4 {
		tracker.AddHash(allLeaves[0][i], models.StatusSeenOnNetwork)
	}

	compound, _, err := BuildCompoundBUMP(stumps, subtreeHashes[:1], nil, tracker)
	if err != nil {
		t.Fatalf("BuildCompoundBUMP failed: %v", err)
	}

	bumpData := compound.Bytes()

	// Use a txid that is NOT in the compound BUMP
	fakeTxid := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	result := ExtractMinimalPathForTx(bumpData, fakeTxid)
	if result != nil {
		t.Fatalf("expected nil for unknown txid, got %d bytes", len(result))
	}
}

func TestExtractMinimalPathForTx_InvalidInput(t *testing.T) {
	// Invalid binary data
	result := ExtractMinimalPathForTx([]byte{0xff, 0xff, 0xff}, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if result != nil {
		t.Fatal("expected nil for invalid BUMP binary")
	}

	// Invalid txid (not hex)
	result = ExtractMinimalPathForTx([]byte{0x01, 0x01, 0x01, 0x00, 0x01}, "not-a-valid-hex-txid")
	if result != nil {
		t.Fatal("expected nil for invalid txid")
	}

	// Nil input
	result = ExtractMinimalPathForTx(nil, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if result != nil {
		t.Fatal("expected nil for nil BUMP data")
	}
}
