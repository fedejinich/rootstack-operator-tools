package rskblocks

import (
	"math/big"
	"testing"
)

// TestNewBlockHeaderInputFromHex_RejectsWrongLogsBloom ensures the
// helper rejects a logsBloom of the wrong length instead of silently
// zero-filling the field. Previously a non-256-byte input (e.g.
// leading-zero-stripped from an RPC) was discarded and the returned
// input carried an all-zero LogsBloom, producing a block hash that
// did not match the real RSK block — with no signal to the caller.
func TestNewBlockHeaderInputFromHex_RejectsWrongLogsBloom(t *testing.T) {
	args := func(bloom []byte) (*BlockHeaderInput, error) {
		return NewBlockHeaderInputFromHex(
			"0x", "0x", "0x", "0x", "0x", "0x",
			bloom,
			big.NewInt(0), big.NewInt(0), big.NewInt(0), big.NewInt(0), big.NewInt(0),
			nil,
			big.NewInt(0), big.NewInt(0),
			0,
			nil, nil, nil,
			nil,
		)
	}

	if _, err := args(make([]byte, 255)); err == nil {
		t.Fatalf("accepted 255-byte logsBloom, want error")
	}
	if _, err := args(make([]byte, 257)); err == nil {
		t.Fatalf("accepted 257-byte logsBloom, want error")
	}
	if _, err := args(make([]byte, 256)); err != nil {
		t.Fatalf("rejected valid 256-byte logsBloom: %v", err)
	}
}
