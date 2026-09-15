package trie

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// TestConvertReceipt_PostStateMapping covers the EIP-658 mapping rule:
// post-Byzantium successful receipts must populate BOTH PostState (=0x01)
// and Status (=0x01) for RSK's encoding; failed receipts leave both empty;
// pre-Byzantium receipts pass PostState through unchanged.
func TestConvertReceipt_PostStateMapping(t *testing.T) {
	rootBytes := common.HexToHash("0xdeadbeef").Bytes()

	tests := []struct {
		name          string
		in            *types.Receipt
		wantPostState []byte
		wantStatus    []byte
	}{
		{
			name: "post-byzantium success",
			in: &types.Receipt{
				Status:            types.ReceiptStatusSuccessful,
				CumulativeGasUsed: 21000,
			},
			wantPostState: []byte{0x01},
			wantStatus:    []byte{0x01},
		},
		{
			name: "post-byzantium failure",
			in: &types.Receipt{
				Status:            types.ReceiptStatusFailed,
				CumulativeGasUsed: 21000,
			},
			wantPostState: nil,
			wantStatus:    nil,
		},
		{
			name: "pre-byzantium passes PostState through",
			in: &types.Receipt{
				PostState:         rootBytes,
				CumulativeGasUsed: 21000,
			},
			wantPostState: rootBytes,
			wantStatus:    nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ConvertReceipt(tc.in)
			if got == nil {
				t.Fatal("ConvertReceipt returned nil")
			}
			if !bytesEq(got.PostState, tc.wantPostState) {
				t.Errorf("PostState = %x, want %x", got.PostState, tc.wantPostState)
			}
			if !bytesEq(got.Status, tc.wantStatus) {
				t.Errorf("Status = %x, want %x", got.Status, tc.wantStatus)
			}
			if got.CumulativeGasUsed != tc.in.CumulativeGasUsed {
				t.Errorf("CumulativeGasUsed = %d, want %d", got.CumulativeGasUsed, tc.in.CumulativeGasUsed)
			}
		})
	}
}

func TestConvertReceipt_Nil(t *testing.T) {
	if ConvertReceipt(nil) != nil {
		t.Errorf("ConvertReceipt(nil) should return nil")
	}
}

func TestCalculateReceiptsRoot_EmptyDeterministic(t *testing.T) {
	root1, err := CalculateReceiptsRoot(types.Receipts{})
	if err != nil {
		t.Fatalf("CalculateReceiptsRoot: %v", err)
	}
	root2, err := CalculateReceiptsRoot(types.Receipts{})
	if err != nil {
		t.Fatalf("CalculateReceiptsRoot: %v", err)
	}
	if root1 != root2 {
		t.Errorf("empty-receipts root not deterministic: %x vs %x", root1, root2)
	}
}

func bytesEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
