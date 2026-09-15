package rskblocks

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/rlp"
)

// TestBytesToUint64_RejectsOversizedInput guards against silent truncation
// of >8-byte big-endian values in receipt RLP decoding. Previously
// bytesToUint64 ran the input through big.Int and called .Uint64(), which
// drops high bits with no error — a malformed feed could decode a receipt
// with wrong gas values and we'd silently compute a wrong receipts root.
func TestBytesToUint64_RejectsOversizedInput(t *testing.T) {
	if _, err := bytesToUint64(make([]byte, 9)); err == nil {
		t.Fatalf("bytesToUint64 accepted a 9-byte input, want error")
	}

	// Boundary: 8 bytes is fine; 0 bytes returns 0.
	if got, err := bytesToUint64([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}); err != nil || got != 0xffffffffffffffff {
		t.Fatalf("bytesToUint64(8B max): got %x err %v", got, err)
	}
	if got, err := bytesToUint64(nil); err != nil || got != 0 {
		t.Fatalf("bytesToUint64(nil): got %d err %v", got, err)
	}
}

// TestTransactionReceipt_DecodeRLP_RejectsOversizedGas exercises the same
// guard through the public DecodeRLP path: an RLP receipt whose
// CumulativeGasUsed field is encoded as 9 bytes must surface an error
// instead of silently producing a wrong receipts root.
func TestTransactionReceipt_DecodeRLP_RejectsOversizedGas(t *testing.T) {
	encoded, err := rlp.EncodeToBytes(&receiptRLP{
		CumulativeGasUsed: make([]byte, 9),
		Status:            []byte{0x01},
	})
	if err != nil {
		t.Fatalf("test setup: encode receiptRLP: %v", err)
	}

	var got TransactionReceipt
	if err := rlp.Decode(bytes.NewReader(encoded), &got); err == nil {
		t.Fatalf("DecodeRLP accepted 9-byte CumulativeGasUsed, want error")
	}
}
