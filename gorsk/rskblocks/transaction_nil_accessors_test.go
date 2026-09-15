package rskblocks

import (
	"math/big"
	"testing"
)

// TestTransaction_NilAccessors_DoNotPanic guards against nil-pointer
// dereferences in the public big.Int accessors when the underlying txdata
// fields decoded to nil (RLP empty/null). Previously GasPrice()/Value()
// did new(big.Int).Set(tx.data.Price/Amount), panicking on nil. RPC
// callers consuming proxied transactions (e.g. proof / receipt walks)
// could crash on a malformed tx.
func TestTransaction_NilAccessors_DoNotPanic(t *testing.T) {
	// Construct a Transaction whose data has nil Price / Amount — the same
	// state DecodeRLP can leave behind for RLP-encoded null big.Int fields.
	tx := &Transaction{data: txdata{}}

	if got := tx.GasPrice(); got == nil || got.Sign() != 0 {
		t.Fatalf("GasPrice() on nil Price: want zero *big.Int, got %v", got)
	}
	if got := tx.Value(); got == nil || got.Sign() != 0 {
		t.Fatalf("Value() on nil Amount: want zero *big.Int, got %v", got)
	}

	// Sanity: normal accessor path still returns the original value.
	tx2 := NewTransaction(0, [20]byte{}, big.NewInt(7), 0, big.NewInt(11), nil)
	if tx2.GasPrice().Int64() != 11 || tx2.Value().Int64() != 7 {
		t.Fatalf("accessors regressed on populated tx: gas=%v val=%v", tx2.GasPrice(), tx2.Value())
	}
}
