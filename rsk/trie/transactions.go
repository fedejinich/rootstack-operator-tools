package trie

import (
	"github.com/fedejinich/rootstack-operator-tools/gorsk/rskblocks"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// CalculateTxRoot calculates the RSK transaction trie root using gorsk's
// Unitrie implementation. Use instead of types.DeriveSha for RSK chains.
func CalculateTxRoot(txs types.Transactions) (common.Hash, error) {
	rskTxs := make([]*rskblocks.Transaction, len(txs))
	for i, tx := range txs {
		rskTxs[i] = ConvertTransaction(tx)
	}
	root, err := rskblocks.GetTxTrieRoot(rskTxs)
	if err != nil {
		return common.Hash{}, err
	}
	return common.BytesToHash(root), nil
}

// ConvertTransaction converts a go-ethereum Transaction to an RSK Transaction.
// RSK transactions have specific RLP encoding rules for internal transactions
// (REMASC) vs external signed transactions; gorsk handles the encoding via
// NewSignedTransaction.
func ConvertTransaction(tx *types.Transaction) *rskblocks.Transaction {
	if tx == nil {
		return nil
	}
	v, r, s := tx.RawSignatureValues()
	var to *common.Address
	if tx.To() != nil {
		addr := *tx.To()
		to = &addr
	}
	return rskblocks.NewSignedTransaction(
		tx.Nonce(), to, tx.Value(), tx.Gas(), tx.GasPrice(), tx.Data(),
		v, r, s,
	)
}
