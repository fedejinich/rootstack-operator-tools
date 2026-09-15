package rskblocks

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/fedejinich/rootstack-operator-tools/gorsk/rsktrie"

	"github.com/ethereum/go-ethereum/common"
)

func TestCalculateReceiptsTrieRoot(t *testing.T) {
	// Create some dummy receipts
	r1 := &TransactionReceipt{
		Status:            []byte{1},
		CumulativeGasUsed: 1000,
		Bloom:             [256]byte{},
	}
	r2 := &TransactionReceipt{
		Status:            []byte{0},
		CumulativeGasUsed: 2000,
		Bloom:             [256]byte{},
	}
	receipts := []*TransactionReceipt{r1, r2}

	root, err := CalculateReceiptsTrieRoot(receipts)
	if err != nil {
		t.Fatalf("CalculateReceiptsTrieRoot: %v", err)
	}
	if len(root) != 32 {
		t.Errorf("Expected 32-byte root, got %d", len(root))
	}
}

func TestGetTxTrieRoot(t *testing.T) {
	tx1 := NewTransaction(0, common.Address{}, big.NewInt(0), 21000, big.NewInt(1), nil)
	tx2 := NewTransaction(1, common.Address{}, big.NewInt(100), 21000, big.NewInt(1), nil)
	txs := []*Transaction{tx1, tx2}

	root, err := GetTxTrieRoot(txs)
	if err != nil {
		t.Fatalf("GetTxTrieRoot: %v", err)
	}
	if len(root) != 32 {
		t.Errorf("Expected 32-byte root, got %d", len(root))
	}
}

func TestTxTrieRootEmpty(t *testing.T) {
	root, err := GetTxTrieRoot(nil)
	if err != nil {
		t.Fatalf("GetTxTrieRoot(nil): %v", err)
	}
	// Empty trie hash
	// Keccak256(RLP(Empty String "80"))? No, Empty Trie hash is often specific constant.
	// In our Trie implementation:
	trie := rsktrie.NewTrie(nil)
	expected := trie.GetHash()

	if !bytes.Equal(root, expected) {
		t.Errorf("Empty root mismatch. Got %x, want %x", root, expected)
	}
}
