package rskblocks

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// Test vectors from RSK regtest node block 1
// These were verified against the actual RSK Java implementation (V2 headers with RSKIP-535)
func TestComputeBlockHashBlock1(t *testing.T) {
	// Block 1 data from RSK regtest (fresh node with V2 headers)
	input := &BlockHeaderInput{
		ParentHash:               common.HexToHash("0x8ea789fabef0dd4946ed53f001e7b6f8a8d0c22a612a6099fc7f93c990af68fe"),
		UnclesHash:               common.HexToHash("0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347"),
		Coinbase:                 common.HexToAddress("0xec4ddeb4380ad69b3e509baad9f158cdf4e4681d"),
		StateRoot:                common.HexToHash("0xf276a3a8c9c4eb4dcbbfb9bf6965f36dc611b815614c0d7cd06e15b8890c272c"),
		TxTrieRoot:               common.HexToHash("0x8c9664a30670ddc67aa13992fdd8751b7b797bbe172506ffd5cda10ebbf97952"),
		ReceiptTrieRoot:          common.HexToHash("0x66cfdb731f620cd96e2c2cb0f7d3c3a2879c29b40014aa27efbbf3cf9cd3b0f6"),
		Difficulty:               big.NewInt(1),
		Number:                   big.NewInt(1),
		GasLimit:                 big.NewInt(10000000), // 0x989680
		GasUsed:                  big.NewInt(0),
		Timestamp:                big.NewInt(0x69824213),
		ExtraData:                hexToBytes("d40192534e415053484f542d343031373966623937"),
		PaidFees:                 big.NewInt(0),
		MinimumGasPrice:          big.NewInt(0),
		UncleCount:               0,
		TxExecutionSublistsEdges: []int16{}, // Empty but not nil
		// No btcHeader for this test block
	}

	// LogsBloom is all zeros for block 1
	// (REMASC transaction doesn't generate logs)

	config := DefaultRegtestConfig() // V2 with IncludeUmmRoot=true

	// Expected hash from RSK node (V2 encoding)
	expectedHash := common.HexToHash("0x90299cad077d0759beee6c9625be98114874d9ae65ede6979752a97112043b63")

	// Compute hash
	computedHash := ComputeBlockHash(input, config)

	if computedHash != expectedHash {
		t.Errorf("Block hash mismatch\n  Expected: %s\n  Computed: %s", expectedHash.Hex(), computedHash.Hex())
	}
}

// Test the RLP encoding matches Java's output exactly (V2 headers)
func TestBlockHeaderEncodingBlock1(t *testing.T) {
	input := &BlockHeaderInput{
		ParentHash:               common.HexToHash("0x8ea789fabef0dd4946ed53f001e7b6f8a8d0c22a612a6099fc7f93c990af68fe"),
		UnclesHash:               common.HexToHash("0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347"),
		Coinbase:                 common.HexToAddress("0xec4ddeb4380ad69b3e509baad9f158cdf4e4681d"),
		StateRoot:                common.HexToHash("0xf276a3a8c9c4eb4dcbbfb9bf6965f36dc611b815614c0d7cd06e15b8890c272c"),
		TxTrieRoot:               common.HexToHash("0x8c9664a30670ddc67aa13992fdd8751b7b797bbe172506ffd5cda10ebbf97952"),
		ReceiptTrieRoot:          common.HexToHash("0x66cfdb731f620cd96e2c2cb0f7d3c3a2879c29b40014aa27efbbf3cf9cd3b0f6"),
		Difficulty:               big.NewInt(1),
		Number:                   big.NewInt(1),
		GasLimit:                 big.NewInt(10000000),
		GasUsed:                  big.NewInt(0),
		Timestamp:                big.NewInt(0x69824213),
		ExtraData:                hexToBytes("d40192534e415053484f542d343031373966623937"),
		PaidFees:                 big.NewInt(0),
		MinimumGasPrice:          big.NewInt(0),
		UncleCount:               0,
		TxExecutionSublistsEdges: []int16{},
		// No btcHeader for this test
	}

	config := DefaultRegtestConfig() // V2

	// Expected encoding from Java BlockHeader.getEncodedForHash() with V2 headers
	expectedEncoding := "f90105a08ea789fabef0dd4946ed53f001e7b6f8a8d0c22a612a6099fc7f93c990af68fea01dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d4934794ec4ddeb4380ad69b3e509baad9f158cdf4e4681da0f276a3a8c9c4eb4dcbbfb9bf6965f36dc611b815614c0d7cd06e15b8890c272ca08c9664a30670ddc67aa13992fdd8751b7b797bbe172506ffd5cda10ebbf97952a066cfdb731f620cd96e2c2cb0f7d3c3a2879c29b40014aa27efbbf3cf9cd3b0f6a3e202a09aca8469839f117b1a26abbdea244a32cb0833e387cf6af9dc13eb336094d3c20101840098968080846982421395d40192534e415053484f542d34303137396662393780008080"

	encoded := GetEncodedBlockHeader(input, config)
	encodedHex := hex.EncodeToString(encoded)

	if encodedHex != expectedEncoding {
		t.Errorf("Encoding mismatch\n  Expected length: %d\n  Computed length: %d\n  Expected: %s\n  Computed: %s",
			len(expectedEncoding)/2, len(encoded), expectedEncoding, encodedHex)
	}
}

// Test extension hash computation for V2 headers (RSKIP-535)
func TestExtensionHashComputationV2(t *testing.T) {
	input := &BlockHeaderInput{
		ParentHash:               common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		UnclesHash:               common.HexToHash("0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347"),
		StateRoot:                common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		TxTrieRoot:               common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		ReceiptTrieRoot:          common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		Difficulty:               big.NewInt(1),
		Number:                   big.NewInt(1),
		GasLimit:                 big.NewInt(10000000),
		GasUsed:                  big.NewInt(0),
		Timestamp:                big.NewInt(1000000),
		PaidFees:                 big.NewInt(0),
		MinimumGasPrice:          big.NewInt(0),
		TxExecutionSublistsEdges: []int16{}, // Empty edges
		BaseEvent:                nil,       // No baseEvent
	}
	// LogsBloom is all zeros (default)

	config := BlockHashConfig{
		UseRskip92Encoding: true,
		Version:            2,
		IncludeUmmRoot:     true,
	}

	header := InputToBlockHeader(input, config)

	// The V2 extension hash for logsBloom=zeros, baseEvent=nil, edges=[] should be:
	// Keccak256(RLP([Keccak256(logsBloom), emptyBaseEvent, edgesBytes]))
	// = Keccak256(RLP([d397b3b043d87fcd6fad1291ff0bfd16401c274896d8c63a923727f077b8e0b5, 0x80, 0x80]))
	// = 9aca8469839f117b1a26abbdea244a32cb0833e387cf6af9dc13eb336094d3c2

	// The extensionData should be RLP([version=2, extensionHash])
	expectedExtensionHash := "9aca8469839f117b1a26abbdea244a32cb0833e387cf6af9dc13eb336094d3c2"

	// Get the encoded header and check it contains the expected extension hash
	encoded := header.GetEncodedForHash()
	encodedHex := hex.EncodeToString(encoded)

	if !contains(encodedHex, expectedExtensionHash) {
		t.Errorf("V2 Extension hash not found in encoded header\n  Expected to contain: %s\n  Encoded: %s",
			expectedExtensionHash, encodedHex)
	}
}

// Test configuration for different networks
func TestConfigForBlockNumber(t *testing.T) {
	tests := []struct {
		name     string
		blockNum int64
		network  string
		expected BlockHashConfig
	}{
		{
			// Block 1, not 0: regtest.conf pins rskip144/351/535 there, so
			// genesis is V0 and refused. See
			// TestConfigForBlockNumberRegtestGenesisFailsClosed.
			name:     "regtest block 1 (first versioned header)",
			blockNum: 1,
			network:  "regtest",
			expected: BlockHashConfig{UseRskip92Encoding: true, Version: 2, IncludeUmmRoot: true, Use4ByteGasLimit: true},
		},
		{
			name:     "regtest block 100",
			blockNum: 100,
			network:  "regtest",
			expected: BlockHashConfig{UseRskip92Encoding: true, Version: 2, IncludeUmmRoot: true, Use4ByteGasLimit: true},
		},
		{
			name:     "mainnet post-UMM (V0)",
			blockNum: 5000000,
			network:  "mainnet",
			expected: BlockHashConfig{UseRskip92Encoding: true, Version: 0, IncludeUmmRoot: true, Use4ByteGasLimit: false},
		},
		{
			name:     "mainnet current (V0)",
			blockNum: 8000000,
			network:  "mainnet",
			expected: BlockHashConfig{UseRskip92Encoding: true, Version: 0, IncludeUmmRoot: true, Use4ByteGasLimit: false},
		},
		{
			name:     "mainnet pre-UMM",
			blockNum: 2000000,
			network:  "mainnet",
			expected: BlockHashConfig{UseRskip92Encoding: true, Version: 0, IncludeUmmRoot: false, Use4ByteGasLimit: false},
		},
		{
			name:     "testnet V1",
			blockNum: 7200000,
			network:  "testnet",
			expected: BlockHashConfig{UseRskip92Encoding: true, Version: 1, IncludeUmmRoot: true, Use4ByteGasLimit: false},
		},
		{
			// testnet.conf overrides rskip535 = 7604200 explicitly, at the
			// same height as vetiver900, so testnet IS V2 from there on.
			name:     "testnet V2 (rskip535 = 7604200)",
			blockNum: 7604200,
			network:  "testnet",
			expected: BlockHashConfig{UseRskip92Encoding: true, Version: 2, IncludeUmmRoot: true, Use4ByteGasLimit: false},
		},
		{
			name:     "testnet last V1 block",
			blockNum: 7604199,
			network:  "testnet",
			expected: BlockHashConfig{UseRskip92Encoding: true, Version: 1, IncludeUmmRoot: true, Use4ByteGasLimit: false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := ConfigForBlockNumber(tt.blockNum, tt.network)
			if err != nil {
				t.Fatalf("ConfigForBlockNumber(%d, %q): unexpected error: %v", tt.blockNum, tt.network, err)
			}
			if config.UseRskip92Encoding != tt.expected.UseRskip92Encoding {
				t.Errorf("UseRskip92Encoding: expected %v, got %v", tt.expected.UseRskip92Encoding, config.UseRskip92Encoding)
			}
			if config.Version != tt.expected.Version {
				t.Errorf("Version: expected %d, got %d", tt.expected.Version, config.Version)
			}
			if config.IncludeUmmRoot != tt.expected.IncludeUmmRoot {
				t.Errorf("IncludeUmmRoot: expected %v, got %v", tt.expected.IncludeUmmRoot, config.IncludeUmmRoot)
			}
			if config.Use4ByteGasLimit != tt.expected.Use4ByteGasLimit {
				t.Errorf("Use4ByteGasLimit: expected %v, got %v", tt.expected.Use4ByteGasLimit, config.Use4ByteGasLimit)
			}
		})
	}
}

func TestInputToBlockHeaderGasLimitUsesJavaBigIntegerEncoding(t *testing.T) {
	tests := []struct {
		name     string
		gasLimit *big.Int
		want     []byte
	}{
		{name: "nil", want: []byte{}},
		{name: "zero", gasLimit: big.NewInt(0), want: []byte{0x00}},
		{name: "high bit clear", gasLimit: big.NewInt(0x7fffff), want: []byte{0x7f, 0xff, 0xff}},
		{name: "high bit set", gasLimit: big.NewInt(0x800000), want: []byte{0x00, 0x80, 0x00, 0x00}},
		{name: "regtest ten million", gasLimit: big.NewInt(0x989680), want: []byte{0x00, 0x98, 0x96, 0x80}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, use4ByteGasLimit := range []bool{false, true} {
				header := InputToBlockHeader(
					&BlockHeaderInput{GasLimit: tt.gasLimit},
					BlockHashConfig{Use4ByteGasLimit: use4ByteGasLimit},
				)
				if !bytes.Equal(header.GasLimit, tt.want) {
					t.Errorf(
						"Use4ByteGasLimit=%v: GasLimit = %x, want %x",
						use4ByteGasLimit,
						header.GasLimit,
						tt.want,
					)
				}
			}
		})
	}
}

func TestInputToBlockHeaderPreservesExplicitGasLimitBytes(t *testing.T) {
	input := &BlockHeaderInput{
		GasLimit:      big.NewInt(0x800000),
		GasLimitBytes: []byte{0x00, 0x00, 0x80, 0x00, 0x00},
	}
	header := InputToBlockHeader(input, BlockHashConfig{})
	if !bytes.Equal(header.GasLimit, input.GasLimitBytes) {
		t.Errorf("GasLimit = %x, want explicit bytes %x", header.GasLimit, input.GasLimitBytes)
	}
}

// Test V1/V2 header always has edges (even if empty)
func TestV1V2HeaderAlwaysHasEdges(t *testing.T) {
	for _, version := range []byte{1, 2} {
		input := &BlockHeaderInput{
			Number:                   big.NewInt(1),
			TxExecutionSublistsEdges: nil, // Input has nil edges
		}

		config := BlockHashConfig{
			Version: version,
		}

		header := InputToBlockHeader(input, config)

		// V1/V2 header should have empty edges, not nil
		if header.TxExecutionSublistsEdges == nil {
			t.Errorf("V%d header should have non-nil edges (empty slice)", version)
		}

		if len(header.TxExecutionSublistsEdges) != 0 {
			t.Errorf("V%d header should have empty edges, got %d edges", version, len(header.TxExecutionSublistsEdges))
		}
	}
}

// Test V0 header doesn't add edges if input has none
func TestV0HeaderNoEdgesIfNone(t *testing.T) {
	input := &BlockHeaderInput{
		Number:                   big.NewInt(1),
		TxExecutionSublistsEdges: nil,
	}

	config := BlockHashConfig{
		Version: 0,
	}

	header := InputToBlockHeader(input, config)

	// V0 header should keep nil edges
	if header.TxExecutionSublistsEdges != nil {
		t.Error("V0 header should have nil edges when input has nil")
	}
}

// RSKj emits an ummRoot field in active eras, but JSON-RPC omits it. Preserve
// the present-but-empty fallback while allowing callers to pass a derived root.
func TestUmmRootNilIsSubstitutedWithEmpty(t *testing.T) {
	base := func() *BlockHeaderInput {
		return &BlockHeaderInput{
			Number:     big.NewInt(1),
			Difficulty: big.NewInt(1),
			GasLimit:   big.NewInt(1),
		}
	}

	h := InputToBlockHeader(base(), BlockHashConfig{UseRskip92Encoding: true, IncludeUmmRoot: true})
	if h.UmmRoot == nil {
		t.Fatal("IncludeUmmRoot with a nil input.UmmRoot left ummRoot absent: the substitution " +
			"this test pins is gone, and with it the block hash of every in-scope era")
	}
	if len(*h.UmmRoot) != 0 {
		t.Errorf("substituted ummRoot = %x, want present-but-empty", *h.UmmRoot)
	}

	if h := InputToBlockHeader(base(), BlockHashConfig{UseRskip92Encoding: true}); h.UmmRoot != nil {
		t.Errorf("IncludeUmmRoot=false with a nil input.UmmRoot: ummRoot = %x, want absent", *h.UmmRoot)
	}

	with := GetEncodedBlockHeader(base(), BlockHashConfig{UseRskip92Encoding: true, IncludeUmmRoot: true})
	without := GetEncodedBlockHeader(base(), BlockHashConfig{UseRskip92Encoding: true})
	if bytes.Equal(with, without) {
		t.Error("the substituted empty ummRoot does not change the header preimage")
	}
}

// Helper function
func hexToBytes(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr))
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
