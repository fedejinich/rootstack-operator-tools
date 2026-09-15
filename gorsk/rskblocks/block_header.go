package rskblocks

import (
	"bytes"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
	"golang.org/x/crypto/sha3"
)

// mustRLPEncode wraps rlp.Encode for consensus-critical block-hash code paths.
// Inputs here are fixed Go types with controlled shapes — an encode error
// would mean a programming bug, not a recoverable runtime condition. Panic
// rather than silently emit a truncated buffer that would produce a wrong
// block hash with no signal.
func mustRLPEncode(w *bytes.Buffer, val interface{}) {
	if err := rlp.Encode(w, val); err != nil {
		panic(fmt.Errorf("block_header: rlp encode: %w", err))
	}
}

// BlockHeader represents an RSK block header.
// This is a minimal implementation focused on computing the block hash.
type BlockHeader struct {
	ParentHash      common.Hash    // SHA3 256-bit hash of the parent block
	UnclesHash      common.Hash    // SHA3 256-bit hash of the uncles list
	Coinbase        common.Address // 160-bit address (miner)
	StateRoot       common.Hash    // SHA3 256-bit hash of the state trie root
	TxTrieRoot      common.Hash    // SHA3 256-bit hash of the transactions trie root
	ReceiptTrieRoot common.Hash    // SHA3 256-bit hash of the receipts trie root
	LogsBloom       [256]byte      // 256-byte bloom filter
	Difficulty      *big.Int       // Block difficulty
	Number          *big.Int       // Block number
	GasLimit        []byte         // Gas limit - stored as minimal raw bytes (no leading zeros)
	GasUsed         *big.Int       // Gas used
	Timestamp       *big.Int       // Unix timestamp
	ExtraData       []byte         // Extra data (max 32 bytes)
	PaidFees        *big.Int       // Total fees paid in this block
	MinimumGasPrice *big.Int       // Minimum gas price for transactions
	UncleCount      int            // Number of uncles

	// Bitcoin merged mining fields
	BitcoinMergedMiningHeader              []byte
	BitcoinMergedMiningMerkleProof         []byte
	BitcoinMergedMiningCoinbaseTransaction []byte

	// Optional fields - use pointer to distinguish nil from empty
	UmmRoot                  *[]byte // UMM root (nil = not present, empty = present but empty)
	TxExecutionSublistsEdges []int16 // RSKIP-144 parallel transaction execution edges
	BaseEvent                []byte  // RSKIP-535 base event (V2 headers)

	// RSKIP-92 encoding flag
	UseRskip92Encoding bool

	// RSKIP-351/535: Header version (0 for V0, 1 for V1, 2 for V2)
	// V1/V2 headers use extensionData instead of raw logsBloom in encoding
	// V2 adds baseEvent to extensionHash computation
	Version byte
}

// Hash computes the block header hash using Keccak256 of the RLP-encoded header.
func (h *BlockHeader) Hash() common.Hash {
	encoded := h.GetEncodedForHash()
	return keccak256Hash(encoded)
}

// GetEncodedForHash returns the RLP encoding used for computing the block hash.
// This uses compressed encoding with merged mining fields but without
// merkle proof and coinbase transaction (for RSKIP-92 enabled blocks).
func (h *BlockHeader) GetEncodedForHash() []byte {
	return h.getEncoded(true, !h.UseRskip92Encoding, true)
}

// GetFullEncoded returns the full RLP encoding including all fields.
func (h *BlockHeader) GetFullEncoded() []byte {
	return h.getEncoded(true, true, false)
}

// GetEncodedBaseForMergedMining returns the RLP encoding that RSKj hashes
// before adding merged-mining fields.
func (h *BlockHeader) GetEncodedBaseForMergedMining() []byte {
	return h.getEncoded(false, false, true)
}

// getEncoded returns the RLP-encoded block header.
// - withMergedMiningFields: include bitcoin merged mining header
// - withMerkleProofAndCoinbase: include merkle proof and coinbase transaction
// - compressed: use compressed encoding (extensionData instead of logsBloom for V1)
func (h *BlockHeader) getEncoded(withMergedMiningFields, withMerkleProofAndCoinbase, compressed bool) []byte {
	fields := make([]interface{}, 0, 20)

	// Core header fields in order
	fields = append(fields, h.ParentHash.Bytes())
	fields = append(fields, h.UnclesHash.Bytes())
	fields = append(fields, encodeRskAddress(h.Coinbase))
	fields = append(fields, h.StateRoot.Bytes())
	fields = append(fields, h.TxTrieRoot.Bytes())
	fields = append(fields, h.ReceiptTrieRoot.Bytes())

	// RSKIP-351/535: For V1/V2 headers in compressed mode, use extensionData
	// instead of raw logsBloom
	if (h.Version == 1 || h.Version == 2) && compressed {
		// extensionData = RLP([version, extensionHash])
		// V1: extensionHash = Keccak256(RLP([logsBloomHash, edges]))
		// V2: extensionHash = Keccak256(RLP([logsBloomHash, baseEvent, edges]))
		extensionData := h.computeExtensionData()
		fields = append(fields, extensionData)
	} else {
		fields = append(fields, h.LogsBloom[:])
	}

	fields = append(fields, encodeBlockDifficulty(h.Difficulty))
	fields = append(fields, encodeBigInteger(h.Number))
	fields = append(fields, h.GasLimit) // gasLimit stored as raw bytes to preserve encoding
	fields = append(fields, encodeBigInteger(h.GasUsed))
	fields = append(fields, encodeBigInteger(h.Timestamp))
	fields = append(fields, h.ExtraData)
	fields = append(fields, encodeCoin(h.PaidFees))
	fields = append(fields, encodeSignedCoinNonNullZero(h.MinimumGasPrice))
	fields = append(fields, encodeBigInteger(big.NewInt(int64(h.UncleCount))))

	// UMM root if present (nil = not included, non-nil = included even if empty)
	if h.UmmRoot != nil {
		fields = append(fields, *h.UmmRoot)
	}

	// For V0 headers or non-compressed V1, add extra fields
	if h.Version == 0 {
		// V0: add edges if present (including empty edges [] which encodes to 0x80)
		// nil means edges field doesn't exist; [] means it exists but is empty
		if h.TxExecutionSublistsEdges != nil {
			fields = append(fields, encodeShortsToRLP(h.TxExecutionSublistsEdges))
		}
	} else if h.Version == 1 && !compressed {
		// V1 non-compressed: add version and edges
		fields = append(fields, []byte{h.Version})
		if h.TxExecutionSublistsEdges != nil {
			fields = append(fields, encodeShortsToRLP(h.TxExecutionSublistsEdges))
		}
	}
	// V1 compressed: don't add version or edges (they're in extensionData)

	// Merged mining fields
	if withMergedMiningFields && h.hasMiningFields() {
		fields = append(fields, h.BitcoinMergedMiningHeader)
		if withMerkleProofAndCoinbase {
			fields = append(fields, h.BitcoinMergedMiningMerkleProof)
			fields = append(fields, h.BitcoinMergedMiningCoinbaseTransaction)
		}
	}

	var buf bytes.Buffer
	mustRLPEncode(&buf, fields)
	return buf.Bytes()
}

// computeExtensionData computes the extensionData for V1/V2 headers.
// extensionData = RLP([version, extensionHash])
// V1: extensionHash = Keccak256(RLP([Keccak256(logsBloom), edgesBytes]))
// V2: extensionHash = Keccak256(RLP([Keccak256(logsBloom), baseEvent, edgesBytes]))
// Note: logsBloom is HASHED before being included in the extension content!
func (h *BlockHeader) computeExtensionData() []byte {
	// First, hash the logsBloom (Java: HashUtil.keccak256(this.getLogsBloom()))
	logsBloomHash := keccak256Hash(h.LogsBloom[:])

	// Convert edges to bytes (little-endian, 2 bytes per short)
	// Empty edges [] is different from null - empty means include 0x80
	var edgesBytes []byte
	if h.TxExecutionSublistsEdges != nil {
		edgesBytes = make([]byte, len(h.TxExecutionSublistsEdges)*2)
		for i, edge := range h.TxExecutionSublistsEdges {
			// Little-endian encoding
			edgesBytes[i*2] = byte(edge)
			edgesBytes[i*2+1] = byte(edge >> 8)
		}
	}
	// Note: if edges is nil, edgesBytes stays nil and won't be included

	// Build extension content based on version
	var extContent bytes.Buffer
	if h.Version == 2 {
		// V2: [logsBloomHash, baseEvent, edgesBytes]
		// baseEvent is included even if empty (encodes as 0x80)
		baseEvent := h.BaseEvent
		if baseEvent == nil {
			baseEvent = []byte{}
		}
		if edgesBytes != nil {
			mustRLPEncode(&extContent, []interface{}{logsBloomHash.Bytes(), baseEvent, edgesBytes})
		} else {
			mustRLPEncode(&extContent, []interface{}{logsBloomHash.Bytes(), baseEvent})
		}
	} else {
		// V1: [logsBloomHash, edgesBytes]
		if edgesBytes != nil {
			mustRLPEncode(&extContent, []interface{}{logsBloomHash.Bytes(), edgesBytes})
		} else {
			mustRLPEncode(&extContent, []interface{}{logsBloomHash.Bytes()})
		}
	}

	// Hash the extension content to get extensionHash
	extensionHash := keccak256Hash(extContent.Bytes())

	// Encode extensionData: [version, extensionHash]
	var extData bytes.Buffer
	mustRLPEncode(&extData, []interface{}{[]byte{h.Version}, extensionHash.Bytes()})
	return extData.Bytes()
}

// hasMiningFields returns true if this header has bitcoin merged mining data.
func (h *BlockHeader) hasMiningFields() bool {
	return len(h.BitcoinMergedMiningCoinbaseTransaction) > 0 ||
		len(h.BitcoinMergedMiningHeader) > 0 ||
		len(h.BitcoinMergedMiningMerkleProof) > 0
}

// Helper functions for RSK-specific RLP encoding
// These return values that the Go RLP encoder will encode correctly.
// In RLP:
// - Empty []byte{} encodes to 0x80 (empty string, represents integer 0)
// - []byte{0} encodes to 0x00 (single zero byte)
// - []byte{0x01} encodes to 0x01 (single byte value)

// encodeRskAddress encodes an RSK address.
// Null/zero address is encoded as empty element (0x80).
func encodeRskAddress(addr common.Address) []byte {
	if addr == (common.Address{}) {
		return []byte{} // Empty -> 0x80
	}
	return addr.Bytes()
}

// encodeBigInteger encodes a BigInteger value.
// Zero is encoded as empty (0x80), matching Java's encodeBigInteger behavior.
func encodeBigInteger(val *big.Int) []byte {
	if val == nil || val.Sign() == 0 {
		return []byte{} // Empty -> 0x80 (RLP encoding of integer 0)
	}
	return val.Bytes()
}

// encodeBlockDifficulty uses the raw bytes returned by RSKj's
// BlockDifficulty.getBytes(). Nil encodes as an empty RLP element.
func encodeBlockDifficulty(difficulty *big.Int) []byte {
	if difficulty == nil {
		return []byte{} // Empty -> 0x80
	}
	return javaBigIntegerBytes(difficulty)
}

// javaBigIntegerBytes matches BigInteger.toByteArray for non-negative values.
// The sign-preserving leading zero is consensus for gasLimit and difficulty.
func javaBigIntegerBytes(v *big.Int) []byte {
	if v == nil {
		return []byte{}
	}
	switch v.Sign() {
	case -1:
		panic("rskblocks: javaBigIntegerBytes requires a non-negative value")
	case 0:
		return []byte{0x00} // BigInteger.ZERO.toByteArray() is {0x00}.
	}
	b := v.Bytes()
	if b[0]&0x80 != 0 {
		return append([]byte{0x00}, b...)
	}
	return b
}

// encodeCoin encodes a Coin value (like paidFees).
// Null or zero coin is encoded as 0x80 (empty, representing integer 0).
func encodeCoin(coin *big.Int) []byte {
	if coin == nil || coin.Sign() == 0 {
		return []byte{} // Empty -> 0x80
	}
	return coin.Bytes()
}

// encodeSignedCoinNonNullZero encodes a signed coin value.
// Null is encoded as empty element (0x80).
// Zero is encoded as single byte 0 (0x00) - different from encodeBigInteger!
func encodeSignedCoinNonNullZero(coin *big.Int) []byte {
	if coin == nil {
		return []byte{} // Empty -> 0x80
	}
	if coin.Sign() == 0 {
		return []byte{0} // Single zero byte -> 0x00
	}
	return coin.Bytes()
}

// encodeShortsToRLP encodes an array of shorts for RLP.
func encodeShortsToRLP(shorts []int16) []byte {
	if len(shorts) == 0 {
		return []byte{}
	}
	// Encode as list of integers
	items := make([]interface{}, len(shorts))
	for i, s := range shorts {
		items[i] = big.NewInt(int64(s))
	}
	var buf bytes.Buffer
	mustRLPEncode(&buf, items)
	return buf.Bytes()
}

// keccak256Hash computes the Keccak256 hash of the input.
func keccak256Hash(data []byte) common.Hash {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	var hash common.Hash
	h.Sum(hash[:0])
	return hash
}
