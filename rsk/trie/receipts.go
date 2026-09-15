package trie

import (
	"github.com/fedejinich/rootstack-operator-tools/gorsk/rskblocks"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// CalculateReceiptsRoot calculates the RSK receipt trie root using gorsk's
// Unitrie implementation. Use instead of types.DeriveSha for RSK chains.
func CalculateReceiptsRoot(receipts types.Receipts) (common.Hash, error) {
	rskReceipts := make([]*rskblocks.TransactionReceipt, len(receipts))
	for i, r := range receipts {
		rskReceipts[i] = ConvertReceipt(r)
	}
	root, err := rskblocks.CalculateReceiptsTrieRoot(rskReceipts)
	if err != nil {
		return common.Hash{}, err
	}
	return common.BytesToHash(root), nil
}

// ConvertReceipt converts a go-ethereum Receipt to an RSK TransactionReceipt.
// RSK receipt encoding differs from Ethereum:
//   - Gas values are big-endian byte arrays, not uint64
//   - Field order: [postTxState, cumulativeGas, bloom, logs, gasUsed, status]
//   - Pre-Byzantium PostState is preserved; post-Byzantium maps Status into
//     both PostState and Status fields (RSK convention).
func ConvertReceipt(r *types.Receipt) *rskblocks.TransactionReceipt {
	if r == nil {
		return nil
	}

	logs := make([]*rskblocks.Log, len(r.Logs))
	for i, log := range r.Logs {
		logs[i] = &rskblocks.Log{
			Address: log.Address,
			Topics:  log.Topics,
			Data:    log.Data,
		}
	}

	var postState, status []byte
	if len(r.PostState) > 0 {
		postState = r.PostState
	} else if r.Status == types.ReceiptStatusSuccessful {
		postState = []byte{0x01}
	}
	if r.Status == types.ReceiptStatusSuccessful {
		status = []byte{0x01}
	}

	return &rskblocks.TransactionReceipt{
		PostState:         postState,
		CumulativeGasUsed: r.CumulativeGasUsed,
		Bloom:             r.Bloom,
		Logs:              logs,
		TxHash:            r.TxHash,
		ContractAddress:   r.ContractAddress,
		GasUsed:           r.GasUsed,
		Status:            status,
	}
}
