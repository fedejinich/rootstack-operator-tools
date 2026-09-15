// Package rskblocks provides an RPC client for RSK's eth_getProof endpoint.
//
// This client fetches Merkle proofs from RSKj and verifies them using the
// ProofVerifier. RSK uses a unified binary trie, so both account and storage
// proofs verify against the same state root.
//
// # Usage
//
//	client, err := rskblocks.NewProofClient("http://localhost:4444")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer client.Close()
//
//	// Fetch and verify an account proof
//	result, err := client.GetAndVerifyAccountProof(ctx, stateRoot, address, "latest")
//	if result.Valid {
//	    fmt.Println("Account verified:", result.Value)
//	}
package rskblocks

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

// ProofResponse represents the eth_getProof RPC response from RSKj.
// This matches the format returned by both eth_getProof and rsk_getProof endpoints.
type ProofResponse struct {
	Address      common.Address `json:"address"`
	AccountProof []string       `json:"accountProof"`
	Balance      *hexutil.Big   `json:"balance"`
	CodeHash     common.Hash    `json:"codeHash"`
	Nonce        hexutil.Uint64 `json:"nonce"`
	StorageHash  common.Hash    `json:"storageHash"`
	StorageProof []StorageProof `json:"storageProof"`
}

// StorageProof represents a single storage proof item in the response.
type StorageProof struct {
	Key    string   `json:"key"`
	Value  string   `json:"value"`
	Proofs []string `json:"proof"` // Ethereum standard field name
}

// ProofClient is an RPC client for fetching and verifying RSK state proofs.
// It wraps the RPC connection and provides methods to fetch proofs and
// verify them against a state root.
type ProofClient struct {
	rpc      *rpc.Client
	verifier *ProofVerifier
}

// NewProofClient creates a new ProofClient connected to the given RPC URL.
//
// Example:
//
//	client, err := NewProofClient("http://localhost:4444")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer client.Close()
func NewProofClient(rpcURL string) (*ProofClient, error) {
	client, err := rpc.Dial(rpcURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to RPC: %w", err)
	}

	return &ProofClient{
		rpc:      client,
		verifier: NewProofVerifier(),
	}, nil
}

// NewProofClientWithRPC creates a ProofClient from an existing RPC client.
// This is useful when you already have an established RPC connection.
func NewProofClientWithRPC(client *rpc.Client) *ProofClient {
	return &ProofClient{
		rpc:      client,
		verifier: NewProofVerifier(),
	}
}

// Close closes the underlying RPC connection.
func (c *ProofClient) Close() {
	if c.rpc != nil {
		c.rpc.Close()
	}
}

// GetProof calls eth_getProof on the RSKj node and returns the raw response.
//
// Parameters:
//   - ctx: Context for the RPC call
//   - address: The account address to get proof for
//   - storageKeys: Storage slots to include proofs for (can be empty for EOAs)
//   - blockRef: Block reference ("latest", "earliest", "pending", or hex block number)
//
// Returns the proof response from the RPC endpoint.
func (c *ProofClient) GetProof(
	ctx context.Context,
	address common.Address,
	storageKeys []common.Hash,
	blockRef string,
) (*ProofResponse, error) {
	// Convert storage keys to hex strings
	keys := make([]string, len(storageKeys))
	for i, key := range storageKeys {
		keys[i] = key.Hex()
	}

	var result ProofResponse
	err := c.rpc.CallContext(ctx, &result, "eth_getProof", address, keys, blockRef)
	if err != nil {
		return nil, fmt.Errorf("eth_getProof RPC call failed: %w", err)
	}

	return &result, nil
}

// GetRSKProof calls rsk_getProof on the RSKj node for RSK-native proof format.
// This endpoint returns proofs in RSK's native unified trie format.
func (c *ProofClient) GetRSKProof(
	ctx context.Context,
	address common.Address,
	storageKeys []common.Hash,
	blockRef string,
) (*ProofResponse, error) {
	keys := make([]string, len(storageKeys))
	for i, key := range storageKeys {
		keys[i] = key.Hex()
	}

	var result ProofResponse
	err := c.rpc.CallContext(ctx, &result, "rsk_getProof", address, keys, blockRef)
	if err != nil {
		return nil, fmt.Errorf("rsk_getProof RPC call failed: %w", err)
	}

	return &result, nil
}

// GetAndVerifyAccountProof fetches an account proof and verifies it against the state root.
//
// This is a convenience method that:
// 1. Calls eth_getProof to get the account proof
// 2. Decodes the RLP-encoded proof nodes
// 3. Verifies the proof against the provided state root
//
// Parameters:
//   - ctx: Context for the RPC call
//   - stateRoot: The state root to verify against (from block header)
//   - address: The account address to verify
//   - blockRef: Block reference for the proof request
//
// Returns AccountProofResult with Valid=true if verification succeeds.
func (c *ProofClient) GetAndVerifyAccountProof(
	ctx context.Context,
	stateRoot common.Hash,
	address common.Address,
	blockRef string,
) (*AccountProofResult, error) {
	// Fetch the proof
	proof, err := c.GetProof(ctx, address, nil, blockRef)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch proof: %w", err)
	}

	// Decode the proof nodes from hex
	proofNodes, err := DecodeRLPProofNodes(proof.AccountProof)
	if err != nil {
		return nil, fmt.Errorf("failed to decode proof nodes: %w", err)
	}

	// Verify the proof
	result, err := c.verifier.VerifyAccountProof(stateRoot, address, proofNodes)
	if err != nil {
		return nil, fmt.Errorf("proof verification error: %w", err)
	}

	return result, nil
}

// GetAndVerifyStorageProof fetches a storage proof and verifies it against the state root.
//
// In RSK, storage proofs verify against the same state root as account proofs
// (unified trie), not a separate storage trie root.
//
// Parameters:
//   - ctx: Context for the RPC call
//   - stateRoot: The state root to verify against (from block header)
//   - address: The contract address
//   - storageKey: The storage slot to verify
//   - blockRef: Block reference for the proof request
//
// Returns StorageProofResult with Valid=true if verification succeeds.
func (c *ProofClient) GetAndVerifyStorageProof(
	ctx context.Context,
	stateRoot common.Hash,
	address common.Address,
	storageKey common.Hash,
	blockRef string,
) (*StorageProofResult, error) {
	// Fetch the proof with the storage key
	proof, err := c.GetProof(ctx, address, []common.Hash{storageKey}, blockRef)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch proof: %w", err)
	}

	// Find the storage proof for our key
	var storageProof *StorageProof
	for i := range proof.StorageProof {
		sp := &proof.StorageProof[i]
		// Compare the key (handle both with and without leading zeros)
		keyHash := common.HexToHash(sp.Key)
		if keyHash == storageKey {
			storageProof = sp
			break
		}
	}

	if storageProof == nil {
		return nil, fmt.Errorf("storage key %s not found in proof response", storageKey.Hex())
	}

	// Decode the storage proof nodes from hex
	proofNodes, err := DecodeRLPProofNodes(storageProof.Proofs)
	if err != nil {
		return nil, fmt.Errorf("failed to decode storage proof nodes: %w", err)
	}

	// Verify the proof
	result, err := c.verifier.VerifyStorageProof(stateRoot, address, storageKey, proofNodes)
	if err != nil {
		return nil, fmt.Errorf("storage proof verification error: %w", err)
	}

	return result, nil
}

// VerifiedProofResult contains the complete result of a verified proof request,
// including both the raw RPC response and verification results.
type VerifiedProofResult struct {
	// Raw response from the RPC endpoint
	Response *ProofResponse

	// Account verification result
	AccountResult *AccountProofResult

	// Storage verification results (keyed by storage slot)
	StorageResults map[common.Hash]*StorageProofResult

	// Whether all proofs verified successfully
	AllValid bool
}

// GetAndVerifyFullProof fetches and verifies an account proof along with all
// requested storage proofs in a single call.
//
// This is the most comprehensive verification method, useful when you need
// to verify both account state and storage values.
func (c *ProofClient) GetAndVerifyFullProof(
	ctx context.Context,
	stateRoot common.Hash,
	address common.Address,
	storageKeys []common.Hash,
	blockRef string,
) (*VerifiedProofResult, error) {
	// Fetch the proof with all storage keys
	proof, err := c.GetProof(ctx, address, storageKeys, blockRef)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch proof: %w", err)
	}

	result := &VerifiedProofResult{
		Response:       proof,
		StorageResults: make(map[common.Hash]*StorageProofResult),
		AllValid:       true,
	}

	// Verify account proof
	accountProofNodes, err := DecodeRLPProofNodes(proof.AccountProof)
	if err != nil {
		return nil, fmt.Errorf("failed to decode account proof nodes: %w", err)
	}

	accountResult, err := c.verifier.VerifyAccountProof(stateRoot, address, accountProofNodes)
	if err != nil {
		return nil, fmt.Errorf("account proof verification error: %w", err)
	}
	result.AccountResult = accountResult
	if !accountResult.Valid {
		result.AllValid = false
	}

	// Verify each storage proof
	for _, sp := range proof.StorageProof {
		keyHash := common.HexToHash(sp.Key)

		proofNodes, err := DecodeRLPProofNodes(sp.Proofs)
		if err != nil {
			return nil, fmt.Errorf("failed to decode storage proof nodes for key %s: %w", sp.Key, err)
		}

		storageResult, err := c.verifier.VerifyStorageProof(stateRoot, address, keyHash, proofNodes)
		if err != nil {
			return nil, fmt.Errorf("storage proof verification error for key %s: %w", sp.Key, err)
		}

		result.StorageResults[keyHash] = storageResult
		if !storageResult.Valid {
			result.AllValid = false
		}
	}

	return result, nil
}

// GetBalance returns the account balance from a proof response.
func (p *ProofResponse) GetBalance() *big.Int {
	if p.Balance == nil {
		return big.NewInt(0)
	}
	return p.Balance.ToInt()
}

// GetNonce returns the account nonce from a proof response.
func (p *ProofResponse) GetNonce() uint64 {
	return uint64(p.Nonce)
}

// IsContract returns true if the account has code (is a contract).
// An empty code hash (keccak256 of empty) indicates an EOA.
func (p *ProofResponse) IsContract() bool {
	emptyCodeHash := common.HexToHash("0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470")
	return p.CodeHash != emptyCodeHash
}

// GetStorageValue returns the storage value for a given key from the proof response.
// Returns nil if the key is not in the storage proofs.
func (p *ProofResponse) GetStorageValue(key common.Hash) *big.Int {
	for _, sp := range p.StorageProof {
		keyHash := common.HexToHash(sp.Key)
		if keyHash == key {
			// Parse the hex value. Base 0 auto-detects (e.g. "0x..." → hex).
			// Surface parse failure as nil so callers don't conflate a malformed
			// proof entry with a legitimately stored zero.
			value, ok := new(big.Int).SetString(sp.Value, 0)
			if !ok {
				return nil
			}
			return value
		}
	}
	return nil
}
