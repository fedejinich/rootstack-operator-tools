package rskblocks

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

func TestProofResponseMarshaling(t *testing.T) {
	// Test that our types can properly unmarshal JSON from RSK
	jsonResponse := `{
		"address": "0xcd2a3d9f938e13cd947ec05abc7fe734df8dd826",
		"accountProof": [
			"0xb0506aa18a79061073179c0a334a8f67e4e384f3651fb016af1ff9cd37e3760980cf028d0c9f2c9cd03307215522740000"
		],
		"balance": "0xde0b6b3a7640000",
		"codeHash": "0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470",
		"nonce": "0x2",
		"storageHash": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
		"storageProof": [
			{
				"key": "0x0000000000000000000000000000000000000000000000000000000000000000",
				"value": "0x2a",
				"proof": ["0x8f50ff56a437b365522d8aa3580c002a"]
			}
		]
	}`

	var response ProofResponse
	err := json.Unmarshal([]byte(jsonResponse), &response)
	if err != nil {
		t.Fatalf("Failed to unmarshal ProofResponse: %v", err)
	}

	// Check address
	expectedAddr := common.HexToAddress("0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826")
	if response.Address != expectedAddr {
		t.Errorf("Address mismatch: got %s, want %s", response.Address.Hex(), expectedAddr.Hex())
	}

	// Check nonce
	if response.GetNonce() != 2 {
		t.Errorf("Nonce mismatch: got %d, want 2", response.GetNonce())
	}

	// Check storage proof has "proof" field
	if len(response.StorageProof) != 1 {
		t.Fatalf("Expected 1 storage proof, got %d", len(response.StorageProof))
	}
	if len(response.StorageProof[0].Proofs) != 1 {
		t.Errorf("Expected 1 proof node, got %d", len(response.StorageProof[0].Proofs))
	}
}

func TestProofResponse_IsContract(t *testing.T) {
	tests := []struct {
		name       string
		codeHash   string
		isContract bool
	}{
		{
			name:       "EOA (empty code hash)",
			codeHash:   "0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470",
			isContract: false,
		},
		{
			name:       "Contract (non-empty code hash)",
			codeHash:   "0xc10f4e2caad321ec73bc2f9fb53dc69f934417616ae7f04622fb43ecbd8a27b2",
			isContract: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := &ProofResponse{
				CodeHash: common.HexToHash(tt.codeHash),
			}
			if response.IsContract() != tt.isContract {
				t.Errorf("IsContract() = %v, want %v", response.IsContract(), tt.isContract)
			}
		})
	}
}

func TestProofResponse_GetStorageValue(t *testing.T) {
	response := &ProofResponse{
		StorageProof: []StorageProof{
			{
				Key:   "0x0000000000000000000000000000000000000000000000000000000000000000",
				Value: "0x2a", // 42 in hex
			},
			{
				Key:   "0x0000000000000000000000000000000000000000000000000000000000000001",
				Value: "0x64", // 100 in hex
			},
		},
	}

	// Test existing key
	slot0 := common.HexToHash("0x0")
	value := response.GetStorageValue(slot0)
	if value == nil {
		t.Fatal("Expected non-nil value for slot 0")
	}
	if value.Int64() != 42 {
		t.Errorf("Expected 42, got %d", value.Int64())
	}

	// Test another existing key
	slot1 := common.HexToHash("0x1")
	value = response.GetStorageValue(slot1)
	if value == nil {
		t.Fatal("Expected non-nil value for slot 1")
	}
	if value.Int64() != 100 {
		t.Errorf("Expected 100, got %d", value.Int64())
	}

	// Test non-existing key
	slot2 := common.HexToHash("0x2")
	value = response.GetStorageValue(slot2)
	if value != nil {
		t.Errorf("Expected nil for non-existing key, got %v", value)
	}
}

// TestProofResponse_GetStorageValue_MalformedValue covers the case where a
// proof's storage value is not parseable as a number. The previous
// implementation ignored the ok return from big.Int.SetString and returned
// a zero-valued *big.Int, indistinguishable from a legitimate stored 0.
// The function's documented "nil means not found" contract should extend to
// "nil means cannot be interpreted" rather than silently masking a parse
// failure as a real zero.
func TestProofResponse_GetStorageValue_MalformedValue(t *testing.T) {
	response := &ProofResponse{
		StorageProof: []StorageProof{
			{
				Key:   "0x0000000000000000000000000000000000000000000000000000000000000000",
				Value: "not-a-hex-number",
			},
		},
	}

	slot0 := common.HexToHash("0x0")
	if got := response.GetStorageValue(slot0); got != nil {
		t.Fatalf("Expected nil for malformed value, got %v", got)
	}
}

func TestProofResponse_GetBalance(t *testing.T) {
	t.Run("with balance", func(t *testing.T) {
		response := &ProofResponse{}
		// Simulate a balance of 1 ETH (1e18 wei)
		balance := new(big.Int)
		balance.SetString("1000000000000000000", 10)
		response.Balance = (*hexutil.Big)(balance)

		got := response.GetBalance()
		if got.Cmp(balance) != 0 {
			t.Errorf("GetBalance() = %v, want %v", got, balance)
		}
	})

	t.Run("nil balance", func(t *testing.T) {
		response := &ProofResponse{Balance: nil}
		got := response.GetBalance()
		if got.Cmp(big.NewInt(0)) != 0 {
			t.Errorf("GetBalance() = %v, want 0", got)
		}
	})
}

func TestStorageProof_ProofField(t *testing.T) {
	// Ensure our struct uses "proof" (singular) matching Ethereum standard
	sp := StorageProof{
		Key:    "0x0",
		Value:  "0x42",
		Proofs: []string{"0xabc", "0xdef"},
	}

	// Marshal to JSON and verify the field name
	data, err := json.Marshal(sp)
	if err != nil {
		t.Fatalf("Failed to marshal StorageProof: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Failed to unmarshal to map: %v", err)
	}

	// Check that "proof" field exists (Ethereum standard)
	if _, ok := raw["proof"]; !ok {
		t.Error("Expected 'proof' field in JSON output")
	}
}

// TestNewProofClientWithRPC tests creating a client from an existing RPC connection
func TestNewProofClientWithRPC(t *testing.T) {
	// Create a mock HTTP server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer server.Close()

	// Create RPC client
	rpcClient, err := rpc.Dial(server.URL)
	if err != nil {
		t.Fatalf("Failed to dial RPC: %v", err)
	}
	defer rpcClient.Close()

	// Create ProofClient from existing RPC client
	client := NewProofClientWithRPC(rpcClient)
	if client == nil {
		t.Fatal("NewProofClientWithRPC returned nil")
	}
	if client.verifier == nil {
		t.Error("ProofClient verifier is nil")
	}
	if client.rpc == nil {
		t.Error("ProofClient rpc is nil")
	}
}

// TestGetProof_MockServer tests the GetProof method with a mock RPC server
func TestGetProof_MockServer(t *testing.T) {
	// Create a mock JSON-RPC server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
			ID     int               `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("Failed to decode request: %v", err)
			return
		}

		if req.Method != "eth_getProof" {
			t.Errorf("Expected method eth_getProof, got %s", req.Method)
		}

		response := `{
			"jsonrpc": "2.0",
			"id": 1,
			"result": {
				"address": "0xcd2a3d9f938e13cd947ec05abc7fe734df8dd826",
				"accountProof": ["0xabc123"],
				"balance": "0xde0b6b3a7640000",
				"codeHash": "0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470",
				"nonce": "0x5",
				"storageHash": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
				"storageProof": []
			}
		}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()

	// Create client
	client, err := NewProofClient(server.URL)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer client.Close()

	// Call GetProof
	ctx := context.Background()
	address := common.HexToAddress("0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826")
	proof, err := client.GetProof(ctx, address, nil, "latest")
	if err != nil {
		t.Fatalf("GetProof failed: %v", err)
	}

	// Verify response
	if proof.GetNonce() != 5 {
		t.Errorf("Expected nonce 5, got %d", proof.GetNonce())
	}

	if len(proof.AccountProof) != 1 {
		t.Errorf("Expected 1 account proof node, got %d", len(proof.AccountProof))
	}
}

// TestGetProof_WithStorageKeys tests requesting specific storage keys
func TestGetProof_WithStorageKeys(t *testing.T) {
	var capturedParams []json.RawMessage

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
			ID     int               `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		capturedParams = req.Params

		response := `{
			"jsonrpc": "2.0",
			"id": 1,
			"result": {
				"address": "0xcd2a3d9f938e13cd947ec05abc7fe734df8dd826",
				"accountProof": [],
				"balance": "0x0",
				"codeHash": "0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470",
				"nonce": "0x0",
				"storageHash": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
				"storageProof": [
					{
						"key": "0x0000000000000000000000000000000000000000000000000000000000000000",
						"value": "0x2a",
						"proof": ["0xdef456"]
					}
				]
			}
		}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()

	client, err := NewProofClient(server.URL)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer client.Close()

	ctx := context.Background()
	address := common.HexToAddress("0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826")
	storageKeys := []common.Hash{common.HexToHash("0x0")}

	proof, err := client.GetProof(ctx, address, storageKeys, "latest")
	if err != nil {
		t.Fatalf("GetProof failed: %v", err)
	}

	// Verify storage proof was included
	if len(proof.StorageProof) != 1 {
		t.Fatalf("Expected 1 storage proof, got %d", len(proof.StorageProof))
	}

	// Verify the storage key was passed in params
	if len(capturedParams) < 2 {
		t.Fatal("Expected at least 2 params (address, keys)")
	}

	var keys []string
	if err := json.Unmarshal(capturedParams[1], &keys); err != nil {
		t.Fatalf("Failed to unmarshal keys param: %v", err)
	}
	if len(keys) != 1 {
		t.Errorf("Expected 1 key in params, got %d", len(keys))
	}
}

// TestGetRSKProof tests the rsk_getProof variant
func TestGetRSKProof(t *testing.T) {
	var capturedMethod string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		capturedMethod = req.Method

		response := `{
			"jsonrpc": "2.0",
			"id": 1,
			"result": {
				"address": "0xcd2a3d9f938e13cd947ec05abc7fe734df8dd826",
				"accountProof": [],
				"balance": "0x0",
				"codeHash": "0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470",
				"nonce": "0x0",
				"storageHash": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
				"storageProof": []
			}
		}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()

	client, err := NewProofClient(server.URL)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer client.Close()

	ctx := context.Background()
	address := common.HexToAddress("0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826")

	_, err = client.GetRSKProof(ctx, address, nil, "latest")
	if err != nil {
		t.Fatalf("GetRSKProof failed: %v", err)
	}

	if capturedMethod != "rsk_getProof" {
		t.Errorf("Expected method rsk_getProof, got %s", capturedMethod)
	}
}

// TestNewProofClient_InvalidURL tests error handling for invalid URLs
func TestNewProofClient_InvalidURL(t *testing.T) {
	_, err := NewProofClient("not-a-valid-url")
	if err == nil {
		t.Error("Expected error for invalid URL")
	}
}

// Integration test - skipped unless running against a real RSKj node
func TestIntegration_GetAndVerifyAccountProof(t *testing.T) {
	t.Skip("Integration test - requires running RSKj node at localhost:4444")

	client, err := NewProofClient("http://localhost:4444")
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}
	defer client.Close()

	ctx := context.Background()

	// First get a block to get the state root
	// This would require an additional RPC call in a real test

	// For now, just test that we can fetch a proof
	address := common.HexToAddress("0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826")
	proof, err := client.GetProof(ctx, address, nil, "latest")
	if err != nil {
		t.Fatalf("GetProof failed: %v", err)
	}

	t.Logf("Account: %s", proof.Address.Hex())
	t.Logf("Balance: %s", proof.GetBalance().String())
	t.Logf("Nonce: %d", proof.GetNonce())
	t.Logf("IsContract: %v", proof.IsContract())
	t.Logf("AccountProof nodes: %d", len(proof.AccountProof))
}
