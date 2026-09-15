package rsktrie

import (
	"bytes"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
)

// ProofVerifier verifies Merkle proofs from eth_getProof for RSK's binary trie
type ProofVerifier struct {
	keyMapper *TrieKeyMapper
}

// NewProofVerifier creates a new proof verifier
func NewProofVerifier() *ProofVerifier {
	return &ProofVerifier{
		keyMapper: NewTrieKeyMapper(),
	}
}

// AccountProofResult contains the result of account proof verification
type AccountProofResult struct {
	Valid   bool
	Address common.Address
	Value   []byte // RLP-encoded account state
	Error   error
}

// StorageProofResult contains the result of storage proof verification
type StorageProofResult struct {
	Valid      bool
	StorageKey common.Hash
	Value      []byte
	Error      error
}

// VerifyAccountProof verifies an account proof against a state root
// proofNodes should be the RLP-encoded trie nodes from accountProof
func (v *ProofVerifier) VerifyAccountProof(
	stateRoot common.Hash,
	address common.Address,
	proofNodes [][]byte,
) (*AccountProofResult, error) {

	// Generate the trie key for this account
	trieKey := v.keyMapper.GetAccountKey(address)

	// Verify the proof path
	value, err := v.verifyProof(stateRoot[:], trieKey, proofNodes)
	if err != nil {
		return &AccountProofResult{
			Valid:   false,
			Address: address,
			Error:   err,
		}, nil
	}

	return &AccountProofResult{
		Valid:   true,
		Address: address,
		Value:   value,
	}, nil
}

// VerifyStorageProof verifies a storage proof for a contract
// proofNodes should be the RLP-encoded trie nodes from storageProof[].proofs
func (v *ProofVerifier) VerifyStorageProof(
	stateRoot common.Hash,
	address common.Address,
	storageKey common.Hash,
	proofNodes [][]byte,
) (*StorageProofResult, error) {

	// In RSK, storage is in the same unified trie
	// The key is: accountStorageKey = accountKey + storagePrefix + secureKey(slot) + slot
	trieKey := v.keyMapper.GetAccountStorageKey(address, storageKey)

	// Verify the proof path
	value, err := v.verifyProof(stateRoot[:], trieKey, proofNodes)
	if err != nil {
		return &StorageProofResult{
			Valid:      false,
			StorageKey: storageKey,
			Error:      err,
		}, nil
	}

	return &StorageProofResult{
		Valid:      true,
		StorageKey: storageKey,
		Value:      value,
	}, nil
}

// verifyProof walks through the proof nodes and verifies the path
func (v *ProofVerifier) verifyProof(expectedHash []byte, key []byte, proofNodes [][]byte) ([]byte, error) {
	if len(proofNodes) == 0 {
		return nil, fmt.Errorf("empty proof")
	}

	// RSK proof nodes are RLP-encoded. The hash is Keccak256 of the serialized (not RLP) content.
	// Proof order is leaf-to-root (last node is root).
	// Build map using hash of the RLP-decoded (serialized) content

	type nodeEntry struct {
		node           *Trie
		serializedHash []byte
	}
	nodeMap := make(map[string]nodeEntry)

	for i, rlpNode := range proofNodes {
		// RLP decode to get serialized node
		var serializedNode []byte
		if err := rlp.DecodeBytes(rlpNode, &serializedNode); err != nil {
			return nil, fmt.Errorf("failed to RLP decode proof node %d: %w", i, err)
		}

		// Hash of serialized content
		nodeHash := Keccak256(serializedNode)

		// Parse the node
		node, err := FromMessage(serializedNode, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to parse proof node %d: %w", i, err)
		}

		nodeMap[string(nodeHash)] = nodeEntry{node: node, serializedHash: nodeHash}
	}

	// Convert key to bit representation for traversal
	keySlice := TrieKeySliceFromKey(key)

	// Find the root node (should match expectedHash)
	rootEntry, ok := nodeMap[string(expectedHash)]
	if !ok {
		return nil, fmt.Errorf("root hash %x not found in proof nodes", expectedHash)
	}
	currentNode := rootEntry.node

	// Walk the path
	keyPos := 0
	for {
		// Check shared path
		sharedPath := currentNode.sharedPath
		if sharedPath.Length() > 0 {
			// Verify shared path matches
			remaining := keySlice.Length() - keyPos
			if remaining < sharedPath.Length() {
				return nil, fmt.Errorf("key too short for shared path at position %d", keyPos)
			}

			for i := 0; i < sharedPath.Length(); i++ {
				keyBit := keySlice.Get(keyPos + i)
				pathBit := sharedPath.Get(i)
				if keyBit != pathBit {
					// Key diverges from path - value doesn't exist
					return nil, nil
				}
			}
			keyPos += sharedPath.Length()
		}

		// Check if we've consumed the entire key
		if keyPos >= keySlice.Length() {
			// Found the node - return its value
			return currentNode.GetValue(), nil
		}

		// Get next bit and follow child
		nextBit := keySlice.Get(keyPos)
		keyPos++

		var childRef *NodeReference
		if nextBit == 0 {
			childRef = currentNode.left
		} else {
			childRef = currentNode.right
		}

		if childRef.IsEmpty() {
			// No child - value doesn't exist
			return nil, nil
		}

		// Get child hash
		childHash := childRef.GetHash()
		if childHash == nil {
			// Embedded node - get directly
			childNode := childRef.GetNode()
			if childNode == nil {
				return nil, fmt.Errorf("missing embedded child node")
			}
			currentNode = childNode
			continue
		}

		// Look up child in proof nodes
		childEntry, ok := nodeMap[string(childHash)]
		if !ok {
			return nil, fmt.Errorf("missing proof node for hash %x", childHash)
		}
		currentNode = childEntry.node
	}
}

// VerifyProofValue is a convenience function that verifies a proof and checks the expected value
func (v *ProofVerifier) VerifyProofValue(
	stateRoot common.Hash,
	key []byte,
	expectedValue []byte,
	proofNodes [][]byte,
) (bool, error) {

	value, err := v.verifyProof(stateRoot[:], key, proofNodes)
	if err != nil {
		return false, err
	}

	return bytes.Equal(value, expectedValue), nil
}
