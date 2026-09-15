// Package rskblocks provides helpers for working with RSK blocks and state proofs.
//
// # RSK Proof Verification
//
// RSK uses a binary trie (unlike Ethereum's hexary MPT) and a unified trie
// for accounts, contracts, and storage. This means:
//   - Account proofs verify directly against the state root
//   - Storage proofs also verify against the same state root (not a separate storage trie)
//   - The trie key for storage includes the account key as a prefix
//
// # Key Structure
//
// Account key: DomainPrefix(0x00) + SecureKeyPrefix(keccak256(address)[:10]) + address
// Storage key: AccountKey + StoragePrefix(0x00) + SecureKeyPrefix(keccak256(slot)[:10]) + stripLeadingZeros(slot)
// Code key: AccountKey + CodePrefix(0x80)
//
// # Usage Example
//
//	verifier := rskblocks.NewProofVerifier()
//	result, err := verifier.VerifyAccountProof(stateRoot, address, proofNodes)
//	if result.Valid {
//	    fmt.Println("Account exists with value:", result.Value)
//	}
package rskblocks

import (
	"bytes"
	"fmt"

	"github.com/fedejinich/rootstack-operator-tools/gorsk/rsktrie"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
)

// ProofVerifier verifies Merkle proofs from eth_getProof for RSK's binary trie
type ProofVerifier struct {
	keyMapper *rsktrie.TrieKeyMapper
}

// NewProofVerifier creates a new proof verifier for RSK state proofs
func NewProofVerifier() *ProofVerifier {
	return &ProofVerifier{
		keyMapper: rsktrie.NewTrieKeyMapper(),
	}
}

// AccountProofResult contains the result of account proof verification
type AccountProofResult struct {
	Valid   bool           // Whether the proof is valid
	Address common.Address // The verified address
	Value   []byte         // RLP-encoded account state (nonce, balance)
	Error   error          // Error if verification failed
}

// StorageProofResult contains the result of storage proof verification
type StorageProofResult struct {
	Valid      bool        // Whether the proof is valid
	StorageKey common.Hash // The verified storage key
	Value      []byte      // The storage value
	Error      error       // Error if verification failed
}

// VerifyAccountProof verifies an account proof against a state root.
//
// Parameters:
//   - stateRoot: The state root from the block header
//   - address: The account address to verify
//   - proofNodes: RLP-encoded trie nodes from eth_getProof accountProof field
//
// Returns AccountProofResult with Valid=true if the proof is valid.
// The Value field contains the RLP-encoded account state if the account exists.
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

// VerifyStorageProof verifies a storage proof for a contract.
//
// In RSK, storage is in the same unified trie as accounts.
// The storage key includes the account key as a prefix.
//
// Parameters:
//   - stateRoot: The state root from the block header
//   - address: The contract address
//   - storageKey: The storage slot (32 bytes)
//   - proofNodes: RLP-encoded trie nodes from eth_getProof storageProof[].proofs field
//
// Returns StorageProofResult with Valid=true if the proof is valid.
func (v *ProofVerifier) VerifyStorageProof(
	stateRoot common.Hash,
	address common.Address,
	storageKey common.Hash,
	proofNodes [][]byte,
) (*StorageProofResult, error) {
	// In RSK, storage is in the unified trie
	// Key: accountKey + storagePrefix + secureKey(slot) + slot
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

// VerifyStorageValue verifies a storage proof and checks the expected value
func (v *ProofVerifier) VerifyStorageValue(
	stateRoot common.Hash,
	address common.Address,
	storageKey common.Hash,
	expectedValue []byte,
	proofNodes [][]byte,
) (bool, error) {
	result, err := v.VerifyStorageProof(stateRoot, address, storageKey, proofNodes)
	if err != nil {
		return false, err
	}
	if !result.Valid {
		return false, result.Error
	}
	return bytes.Equal(result.Value, expectedValue), nil
}

// verifyProof walks through the proof nodes and verifies the path
func (v *ProofVerifier) verifyProof(expectedHash []byte, key []byte, proofNodes [][]byte) ([]byte, error) {
	if len(proofNodes) == 0 {
		return nil, fmt.Errorf("empty proof")
	}

	// RSK proof nodes are RLP-encoded. The hash is Keccak256 of the serialized (not RLP) content.
	// Proof order is leaf-to-root (last node is root).
	type nodeEntry struct {
		node           *rsktrie.Trie
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
		nodeHash := rsktrie.Keccak256(serializedNode)

		// Parse the node
		node, err := rsktrie.FromMessage(serializedNode, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to parse proof node %d: %w", i, err)
		}

		nodeMap[string(nodeHash)] = nodeEntry{node: node, serializedHash: nodeHash}
	}

	// Convert key to bit representation for traversal
	keySlice := rsktrie.TrieKeySliceFromKey(key)

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
		sharedPath := currentNode.GetSharedPath()
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

		var childRef *rsktrie.NodeReference
		if nextBit == 0 {
			childRef = currentNode.GetLeft()
		} else {
			childRef = currentNode.GetRight()
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

// DecodeRLPProofNodes decodes hex-encoded RLP proof nodes from eth_getProof response
func DecodeRLPProofNodes(hexNodes []string) ([][]byte, error) {
	nodes := make([][]byte, len(hexNodes))
	for i, hexNode := range hexNodes {
		// Remove 0x prefix if present
		if len(hexNode) >= 2 && hexNode[:2] == "0x" {
			hexNode = hexNode[2:]
		}
		node, err := hexDecode(hexNode)
		if err != nil {
			return nil, fmt.Errorf("decode proof node %d: %w", i, err)
		}
		nodes[i] = node
	}
	return nodes, nil
}

// hexDecode decodes a hex string to bytes
func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		s = "0" + s
	}
	result := make([]byte, len(s)/2)
	for i := 0; i < len(result); i++ {
		b, err := hexByte(s[i*2 : i*2+2])
		if err != nil {
			return nil, err
		}
		result[i] = b
	}
	return result, nil
}

func hexByte(s string) (byte, error) {
	var result byte
	for _, c := range s {
		result <<= 4
		switch {
		case c >= '0' && c <= '9':
			result |= byte(c - '0')
		case c >= 'a' && c <= 'f':
			result |= byte(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			result |= byte(c - 'A' + 10)
		default:
			return 0, fmt.Errorf("invalid hex character: %c", c)
		}
	}
	return result, nil
}
