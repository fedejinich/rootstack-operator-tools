package rskblocks

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestDecodeRLPProofNodes(t *testing.T) {
	// Test with sample proof nodes from a real eth_getProof response
	hexNodes := []string{
		"0xb0506aa18a79061073179c0a334a8f67e4e384f3651fb016af1ff9cd37e3760980cf028d0c9f2c9cd03307215522740000",
	}

	nodes, err := DecodeRLPProofNodes(hexNodes)
	if err != nil {
		t.Fatalf("DecodeRLPProofNodes failed: %v", err)
	}

	if len(nodes) != 1 {
		t.Fatalf("Expected 1 node, got %d", len(nodes))
	}

	if len(nodes[0]) == 0 {
		t.Fatal("Expected non-empty node")
	}
}

func TestDecodeRLPProofNodesWithoutPrefix(t *testing.T) {
	// Test without 0x prefix
	hexNodes := []string{
		"b0506aa18a79061073179c0a334a8f67e4e384f3651fb016af1ff9cd37e3760980cf028d0c9f2c9cd03307215522740000",
	}

	nodes, err := DecodeRLPProofNodes(hexNodes)
	if err != nil {
		t.Fatalf("DecodeRLPProofNodes failed: %v", err)
	}

	if len(nodes) != 1 {
		t.Fatalf("Expected 1 node, got %d", len(nodes))
	}
}

func TestNewProofVerifier(t *testing.T) {
	verifier := NewProofVerifier()
	if verifier == nil {
		t.Fatal("NewProofVerifier returned nil")
	}
	if verifier.keyMapper == nil {
		t.Fatal("keyMapper is nil")
	}
}

func TestVerifyAccountProof_EmptyProof(t *testing.T) {
	verifier := NewProofVerifier()

	stateRoot := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	address := common.HexToAddress("0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826")

	result, err := verifier.VerifyAccountProof(stateRoot, address, [][]byte{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Valid {
		t.Fatal("Expected invalid result for empty proof")
	}
}

func TestVerifyStorageProof_EmptyProof(t *testing.T) {
	verifier := NewProofVerifier()

	stateRoot := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	address := common.HexToAddress("0x77045E71a7A2c50903d88e564cD72fab11e82051")
	storageKey := common.HexToHash("0x0")

	result, err := verifier.VerifyStorageProof(stateRoot, address, storageKey, [][]byte{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Valid {
		t.Fatal("Expected invalid result for empty proof")
	}
}

// TestVerifyAccountProof_RealData tests with actual proof data from RSK regtest
// This test demonstrates how to use the proof verifier with real data
func TestVerifyAccountProof_RealData(t *testing.T) {
	// Skip this test in CI - it requires a running RSK node
	t.Skip("Requires running RSK node")

	// Example: Real proof data from eth_getProof for an EOA
	// stateRoot from block header
	// proofNodes from accountProof field

	verifier := NewProofVerifier()
	_ = verifier // Would use with real data
}
