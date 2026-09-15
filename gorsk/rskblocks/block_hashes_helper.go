package rskblocks

import (
	"fmt"

	"github.com/fedejinich/rootstack-operator-tools/gorsk/rsktrie"

	"github.com/ethereum/go-ethereum/rlp"
)

// BlockHashesHelper provides methods to calculate transaction and receipt trie roots.
// Ported from co.rsk.core.bc.BlockHashesHelper.java
type BlockHashesHelper struct{}

// CalculateReceiptsTrieRoot calculates the root hash of the receipts trie.
// Corresponds to calculateReceiptsTrieRoot(List<TransactionReceipt> receipts, boolean isRskip126Enabled)
// NOTE: We assume isRskip126Enabled is always TRUE, as requested.
func CalculateReceiptsTrieRoot(receipts []*TransactionReceipt) ([]byte, error) {
	trie, err := CalculateReceiptsTrieFor(receipts)
	if err != nil {
		return nil, err
	}
	return trie.GetHash(), nil
}

// CalculateReceiptsTrieFor builds a Trie containing the given receipts.
//
// RLP encoding of a uint64 index or a well-formed *TransactionReceipt is
// expected to always succeed in normal operation. If an encoder ever
// errors (malformed input from an RPC, future protocol change), surface
// the error so the caller can fail the surrounding operation rather than
// crash the process — these helpers run inside live validator hooks.
func CalculateReceiptsTrieFor(receipts []*TransactionReceipt) (*rsktrie.Trie, error) {
	receiptsTrie := rsktrie.NewTrie(nil)
	for i, receipt := range receipts {
		key, err := rlp.EncodeToBytes(uint64(i))
		if err != nil {
			return nil, fmt.Errorf("rlp encode receipt key %d: %w", i, err)
		}
		encodedReceipt, err := rlp.EncodeToBytes(receipt)
		if err != nil {
			return nil, fmt.Errorf("rlp encode receipt %d: %w", i, err)
		}
		receiptsTrie = receiptsTrie.Put(key, encodedReceipt)
	}
	return receiptsTrie, nil
}

// GetTxTrieRoot calculates the root hash of the transactions trie.
// Corresponds to getTxTrieRoot(List<Transaction> transactions, boolean isRskip126Enabled)
// NOTE: We assume isRskip126Enabled is always TRUE, as requested.
func GetTxTrieRoot(transactions []*Transaction) ([]byte, error) {
	trie, err := GetTxTrieFor(transactions)
	if err != nil {
		return nil, err
	}
	return trie.GetHash(), nil
}

// GetTxTrieFor builds a Trie containing the given transactions.
//
// See CalculateReceiptsTrieFor for the rationale on returning errors when
// RLP encoding fails.
func GetTxTrieFor(transactions []*Transaction) (*rsktrie.Trie, error) {
	txsState := rsktrie.NewTrie(nil)
	if transactions == nil {
		return txsState, nil
	}

	for i, tx := range transactions {
		key, err := rlp.EncodeToBytes(uint64(i))
		if err != nil {
			return nil, fmt.Errorf("rlp encode tx key %d: %w", i, err)
		}
		encodedTx, err := rlp.EncodeToBytes(tx)
		if err != nil {
			return nil, fmt.Errorf("rlp encode tx %d: %w", i, err)
		}
		txsState = txsState.Put(key, encodedTx)
	}

	return txsState, nil
}
