package main

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

func TestSystemConfigGasLimitABIUnpack(t *testing.T) {
	parsed, err := abi.JSON(strings.NewReader(getGasLimitABI))
	if err != nil {
		t.Fatal(err)
	}
	data, err := parsed.Pack("gasLimit")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 4 {
		t.Fatalf("packed calldata too short: %x", data)
	}

	// ABI-encoded uint64 return value is 32-byte word, right-aligned.
	want := uint64(60_000_000)
	word := big.NewInt(0).SetUint64(want).Bytes()
	padded := make([]byte, 32)
	copy(padded[32-len(word):], word)

	vals, err := parsed.Unpack("gasLimit", padded)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 1 {
		t.Fatalf("expected 1 value, got %d", len(vals))
	}
	got, ok := vals[0].(uint64)
	if !ok {
		t.Fatalf("expected uint64, got %T", vals[0])
	}
	if got != want {
		t.Fatalf("got %d want %d", got, want)
	}
}

func TestSetGasLimitABIPack(t *testing.T) {
	parsed, err := abi.JSON(strings.NewReader(setGasLimitABI))
	if err != nil {
		t.Fatal(err)
	}
	limit := uint64(30_000_000)
	data, err := parsed.Pack("setGasLimit", limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 4+32 {
		t.Fatalf("unexpected calldata length %d", len(data))
	}
}
