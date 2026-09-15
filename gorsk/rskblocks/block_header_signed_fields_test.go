package rskblocks

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// RSKj RLP-encodes gasLimit and difficulty as BigInteger.toByteArray output.
// Values with a high sign bit therefore need a leading zero byte.
func TestJavaSignedByteEncoding(t *testing.T) {
	cases := []struct {
		name string
		val  int64
		want string
	}{
		// High bit clear: identical to big.Int.Bytes().
		{"mainnet gasLimit 0x67c280", 0x67c280, "67c280"},
		{"mainnet gasLimit at the boundary 0x7ff3a8", 0x7ff3a8, "7ff3a8"},
		{"testnet difficulty 0x47a40f2d", 0x47a40f2d, "47a40f2d"},
		// High bit set: one extra 0x00 byte.
		{"regtest gasLimit 0x989680", 0x989680, "00989680"},
		{"mainnet gasLimit past the boundary 0x8013a4", 0x8013a4, "008013a4"},
		{"testnet gasLimit 0xb71b00", 0xb71b00, "00b71b00"},
		{"testnet difficulty 0xa6917e80 (block 7319445)", 0xa6917e80, "00a6917e80"},
		{"testnet difficulty 0xff51f86b (block 7484302)", 0xff51f86b, "00ff51f86b"},
		{"testnet difficulty 0xfff56014 (block 7484303)", 0xfff56014, "00fff56014"},
		{"testnet difficulty 0xf8fa34e4 (block 7529264)", 0xf8fa34e4, "00f8fa34e4"},
		{"testnet difficulty 0x100993051 (block 7484304)", 0x100993051, "0100993051"},
		// Single byte either side of 0x80.
		{"0x7f", 0x7f, "7f"},
		{"0x80", 0x80, "0080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hex.EncodeToString(javaBigIntegerBytes(big.NewInt(tc.val)))
			if got != tc.want {
				t.Errorf("javaBigIntegerBytes(%#x) = %s, want %s", tc.val, got, tc.want)
			}
			if g := hex.EncodeToString(EncodeGasLimitBytes(big.NewInt(tc.val))); g != tc.want {
				t.Errorf("EncodeGasLimitBytes(%#x) = %s, want %s", tc.val, g, tc.want)
			}
		})
	}

	if got := javaBigIntegerBytes(big.NewInt(0)); !bytes.Equal(got, []byte{0x00}) {
		t.Errorf("javaBigIntegerBytes(0) = %x, want 00", got)
	}
	if got := javaBigIntegerBytes(nil); len(got) != 0 {
		t.Errorf("javaBigIntegerBytes(nil) = %x, want empty", got)
	}
}

func TestJavaBigIntegerBytesRejectsNegativeValues(t *testing.T) {
	tests := []struct {
		name   string
		encode func(*big.Int) []byte
	}{
		{name: "internal helper", encode: javaBigIntegerBytes},
		{name: "exported gas limit helper", encode: EncodeGasLimitBytes},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("negative value did not panic")
				}
			}()
			tt.encode(big.NewInt(-1))
		})
	}
}

func TestDifficultyHighBitWidensThePreimage(t *testing.T) {
	withHighBit := big.NewInt(0xfff56014)
	oneBlockLater := big.NewInt(0x100993051)

	input := func(d *big.Int) *BlockHeaderInput {
		return &BlockHeaderInput{
			ParentHash:               common.HexToHash("0x01"),
			UnclesHash:               common.HexToHash("0x02"),
			StateRoot:                common.HexToHash("0x03"),
			TxTrieRoot:               common.HexToHash("0x04"),
			ReceiptTrieRoot:          common.HexToHash("0x05"),
			Difficulty:               d,
			Number:                   big.NewInt(7484303),
			GasLimit:                 big.NewInt(0x67c280),
			GasUsed:                  big.NewInt(0),
			Timestamp:                big.NewInt(1),
			ExtraData:                []byte{},
			PaidFees:                 big.NewInt(0),
			MinimumGasPrice:          big.NewInt(0),
			TxExecutionSublistsEdges: []int16{},
		}
	}
	cfg := BlockHashConfig{UseRskip92Encoding: true, Version: 1, IncludeUmmRoot: true}

	a := GetEncodedBlockHeader(input(withHighBit), cfg)
	b := GetEncodedBlockHeader(input(oneBlockLater), cfg)
	if len(a) != len(b) {
		t.Errorf("preimage lengths differ (%d vs %d): the sign byte is missing from the high-bit difficulty", len(a), len(b))
	}
	if !bytes.Contains(a, []byte{0x85, 0x00, 0xff, 0xf5, 0x60, 0x14}) {
		t.Errorf("preimage does not carry difficulty as the RLP element 85 00fff56014:\n%x", a)
	}
	if bytes.Contains(a, []byte{0x84, 0xff, 0xf5, 0x60, 0x14}) {
		t.Errorf("preimage still carries the unsigned 84 fff56014 encoding:\n%x", a)
	}
}
