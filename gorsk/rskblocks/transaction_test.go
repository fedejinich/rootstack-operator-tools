package rskblocks

import (
	"encoding/hex"
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
)

// Vectors from TransactionTest.java
const (
	// test1
	TxRLPRawData = "a9e880872386f26fc1000085e8d4a510008203e89413978aee95f38490e9769c39b2773ed763d9cd5f80"

	// testTransactionFromSignedRLP
	RlpEncodedSignedTx = "f86b8085e8d4a510008227109413978aee95f38490e9769c39b2773ed763d9cd5f872386f26fc10000801ba0eab47c1a49bf2fe5d40e01d313900e19ca485867d462fe06e139e3a536c6d4f4a014a569d327dcda4b29f74f93c0e9729d2f49ad726e703f9cd90dbb0fbf6649f1"

	// testTransactionFromUnsignedRLP
	RlpEncodedUnsignedTx = "eb8085e8d4a510008227109413978aee95f38490e9769c39b2773ed763d9cd5f872386f26fc1000080808080"
)

func decodeHexTx(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestTransactionVectors(t *testing.T) {
	// 1. Test Decoding Signed TX
	signedBytes := decodeHexTx(RlpEncodedSignedTx)
	var tx Transaction
	err := rlp.DecodeBytes(signedBytes, &tx)
	if err != nil {
		t.Fatalf("Failed to decode signed tx: %v", err)
	}

	// Verify fields
	// Nonce: 0 (80)
	if tx.Nonce() != 0 {
		t.Errorf("Expected nonce 0, got %d", tx.Nonce())
	}
	// GasPrice: e8d4a51000 (1000000000000)
	expectedGasPrice := big.NewInt(1000000000000)
	if tx.GasPrice().Cmp(expectedGasPrice) != 0 {
		t.Errorf("GasPrice mismatch")
	}
	// GasLimit: 2710 (10000)
	if tx.Gas() != 10000 {
		t.Errorf("GasLimit mismatch. Got %d", tx.Gas())
	}
	// Recipient: 13978aee95f38490e9769c39b2773ed763d9cd5f
	expectedTo := common.HexToAddress("13978aee95f38490e9769c39b2773ed763d9cd5f")
	if *tx.To() != expectedTo {
		t.Errorf("Recipient mismatch")
	}

	// Verify Encoding Roundtrip
	encoded, err := rlp.EncodeToBytes(&tx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(encoded, signedBytes) {
		t.Errorf("Encoding mismatch.\nExpected: %x\nGot:      %x", signedBytes, encoded)
	}
}

func TestTransactionRLP(t *testing.T) {
	addr := common.HexToAddress("0xdaea98642337cd3c956116809f48703b4207f2")

	tx := NewTransaction(
		1,
		addr,
		big.NewInt(1000),
		21000,
		big.NewInt(1),
		[]byte("hello"),
	)

	// Set some signature values manually for testing full encoding
	tx.data.V = big.NewInt(27)
	tx.data.R = big.NewInt(1)
	tx.data.S = big.NewInt(2)

	encoded, err := rlp.EncodeToBytes(tx)
	if err != nil {
		t.Fatalf("Failed to encode: %v", err)
	}

	var decoded Transaction
	err = rlp.DecodeBytes(encoded, &decoded)
	if err != nil {
		t.Fatalf("Failed to decode: %v", err)
	}

	if decoded.Nonce() != tx.Nonce() {
		t.Errorf("Nonce mismatch")
	}
	if decoded.Value().Cmp(tx.Value()) != 0 {
		t.Errorf("Value mismatch")
	}
	if string(decoded.Data()) != "hello" {
		t.Errorf("Data mismatch")
	}
}

func TestTransactionHash(t *testing.T) {
	// Just verify it doesn't panic and returns consistent hash
	tx := NewTransaction(0, common.Address{}, big.NewInt(0), 0, big.NewInt(0), nil)
	h1 := tx.Hash()
	h2 := tx.Hash()
	if h1 != h2 {
		t.Fatal("Inconsistent hash")
	}
}

// TestRemascTransaction tests encoding of REMASC system transaction
// REMASC transactions have:
// - nonce = blockNumber - 1
// - from = null address (0x0000...0000)
// - to = REMASC contract (0x0000000000000000000000000000000001000008)
// - gasPrice, gasLimit, value = 0
// - data = empty
// - no signature (v=0, r=0, s=0)
func TestRemascTransaction(t *testing.T) {
	// Create REMASC transaction for block 2 (nonce = 1)
	remascAddr := common.HexToAddress("0x0000000000000000000000000000000001000008")

	// Using NewSignedTransaction with all zeros for signature
	tx := NewSignedTransaction(
		1,             // nonce = blockNumber - 1 = 2 - 1 = 1
		&remascAddr,   // to = REMASC contract
		big.NewInt(0), // value = 0
		0,             // gas = 0
		big.NewInt(0), // gasPrice = 0
		nil,           // data = empty
		big.NewInt(0), // v = 0
		big.NewInt(0), // r = 0
		big.NewInt(0), // s = 0
	)

	// Get the RLP encoded bytes
	encoded, err := tx.GetEncodedRLP()
	if err != nil {
		t.Fatalf("Failed to encode REMASC tx: %v", err)
	}

	t.Logf("REMASC tx RLP encoded: %x", encoded)
	t.Logf("REMASC tx hash: %s", tx.Hash().Hex())

	// The hash from RSK RPC for block 2 REMASC tx is:
	// 0x2508efeddbab2f46ce53e0fb5ed61df9ac1ce696311941207833d7365194dacd
	// We'll compare once we know the correct encoding
}
