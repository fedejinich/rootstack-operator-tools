package rskblocks

import (
	"bytes"
	"fmt"
	"io"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// TransactionReceipt represents the results of a transaction.
// RSK receipt RLP encoding order: [postTxState, cumulativeGas, bloom, logs, gasUsed, status]
type TransactionReceipt struct {
	// Consensus fields
	PostState         []byte      `json:"root"`
	CumulativeGasUsed uint64      `json:"cumulativeGasUsed"`
	Bloom             types.Bloom `json:"logsBloom"`
	Logs              []*Log      `json:"logs"`

	// Implementation fields
	TxHash          common.Hash    `json:"transactionHash"`
	ContractAddress common.Address `json:"contractAddress"`
	GasUsed         uint64         `json:"gasUsed"`

	// RSK specific - transaction status (0x01 for success, empty for failure)
	Status []byte
}

// receiptRLP is the RLP encoding structure for RSK receipts.
// RSK encodes gas values as byte arrays (big-endian), not as uint64.
// Order: [postTxState, cumulativeGas, bloom, logs, gasUsed, status]
type receiptRLP struct {
	PostState         []byte
	CumulativeGasUsed []byte
	Bloom             types.Bloom
	Logs              []*Log
	GasUsed           []byte
	Status            []byte
}

// Log represents a contract log event.
type Log struct {
	Address common.Address `json:"address" gencodec:"required"`
	Topics  []common.Hash  `json:"topics" gencodec:"required"`
	Data    []byte         `json:"data" gencodec:"required"`
}

func (r *TransactionReceipt) EncodeRLP(w io.Writer) error {
	// Convert uint64 gas values to byte arrays (trimmed big-endian)
	cumulativeGasBytes := uint64ToBytes(r.CumulativeGasUsed)
	gasUsedBytes := uint64ToBytes(r.GasUsed)

	return rlp.Encode(w, &receiptRLP{
		PostState:         r.PostState,
		CumulativeGasUsed: cumulativeGasBytes,
		Bloom:             r.Bloom,
		Logs:              r.Logs,
		GasUsed:           gasUsedBytes,
		Status:            r.Status,
	})
}

func (r *TransactionReceipt) DecodeRLP(s *rlp.Stream) error {
	var dec receiptRLP
	if err := s.Decode(&dec); err != nil {
		return err
	}
	r.PostState = dec.PostState
	cumulative, err := bytesToUint64(dec.CumulativeGasUsed)
	if err != nil {
		return fmt.Errorf("decode cumulativeGasUsed: %w", err)
	}
	r.CumulativeGasUsed = cumulative
	r.Bloom = dec.Bloom
	r.Logs = dec.Logs
	gasUsed, err := bytesToUint64(dec.GasUsed)
	if err != nil {
		return fmt.Errorf("decode gasUsed: %w", err)
	}
	r.GasUsed = gasUsed
	r.Status = dec.Status
	return nil
}

// uint64ToBytes converts a uint64 to a trimmed big-endian byte array
func uint64ToBytes(val uint64) []byte {
	if val == 0 {
		return nil // RLP encodes 0 as empty bytes
	}
	return new(big.Int).SetUint64(val).Bytes()
}

// bytesToUint64 converts a big-endian byte array to uint64. Inputs longer
// than 8 bytes are rejected rather than silently truncated — a misbehaving
// RPC handing us a 9+ byte CumulativeGasUsed would otherwise produce a
// receipt with wrong gas values and a wrong receipts root with no signal.
func bytesToUint64(b []byte) (uint64, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if len(b) > 8 {
		return 0, fmt.Errorf("bytesToUint64: %d bytes exceeds uint64 width", len(b))
	}
	return new(big.Int).SetBytes(b).Uint64(), nil
}

// GetEncodedRLP returns the RLP encoded bytes of the receipt
func (r *TransactionReceipt) GetEncodedRLP() ([]byte, error) {
	var buf bytes.Buffer
	if err := r.EncodeRLP(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
