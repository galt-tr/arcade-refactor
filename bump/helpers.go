package bump

import (
	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// addLeaf adds a PathElement to the given level of a MerklePath.
func addLeaf(mp *transaction.MerklePath, level int, elem *transaction.PathElement) {
	for level >= len(mp.Path) {
		mp.Path = append(mp.Path, nil)
	}
	mp.Path[level] = append(mp.Path[level], elem)
}

// findLeafByOffset finds a PathElement at the given level and offset.
func findLeafByOffset(mp *transaction.MerklePath, level int, offset uint64) *transaction.PathElement {
	if level >= len(mp.Path) {
		return nil
	}
	for _, elem := range mp.Path[level] {
		if elem.Offset == offset {
			return elem
		}
	}
	return nil
}

// computeMissingHashes fills in intermediate nodes of the merkle path by computing
// parent hashes from pairs of children.
func computeMissingHashes(mp *transaction.MerklePath) {
	for level := 0; level < len(mp.Path)-1; level++ {
		for _, elem := range mp.Path[level] {
			if elem.Hash == nil {
				continue
			}
			parentOffset := elem.Offset / 2
			// Check if parent already exists
			if findLeafByOffset(mp, level+1, parentOffset) != nil {
				continue
			}
			// Find the sibling
			siblingOffset := elem.Offset ^ 1
			sibling := findLeafByOffset(mp, level, siblingOffset)
			if sibling == nil || sibling.Hash == nil {
				continue
			}

			var parent *chainhash.Hash
			if elem.Offset%2 == 0 {
				parent = merkleTreeParent(elem.Hash, sibling.Hash)
			} else {
				parent = merkleTreeParent(sibling.Hash, elem.Hash)
			}

			addLeaf(mp, level+1, &transaction.PathElement{
				Offset: parentOffset,
				Hash:   parent,
			})
		}
	}
}

// merkleTreeParent computes the parent hash of two child nodes.
func merkleTreeParent(left, right *chainhash.Hash) *chainhash.Hash {
	return transaction.MerkleTreeParent(left, right)
}
