package rsktrie

import (
	"bytes"
	"fmt"

	"github.com/ethereum/go-ethereum/rlp"
)

// FromMessage deserializes a Trie node from its serialized format (RSKIP-107 format).
// This is used to reconstruct trie nodes from proof data.
func FromMessage(message []byte, store TrieStore) (*Trie, error) {
	if len(message) == 0 {
		return nil, fmt.Errorf("empty message")
	}

	// Check if it's the old Orchid format (first byte == 2 means arity)
	if message[0] == 2 {
		return fromMessageOrchid(message, store)
	}

	return fromMessageRSKIP107(message, store)
}

// fromMessageRSKIP107 deserializes using the RSKIP-107 format
func fromMessageRSKIP107(message []byte, store TrieStore) (*Trie, error) {
	if len(message) < 1 {
		return nil, fmt.Errorf("message too short")
	}

	buf := bytes.NewReader(message)

	flags, err := buf.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("read flags: %w", err)
	}

	// Parse flags
	hasLongVal := (flags & 0b00100000) == 0b00100000
	sharedPrefixPresent := (flags & 0b00010000) == 0b00010000
	leftNodePresent := (flags & 0b00001000) == 0b00001000
	rightNodePresent := (flags & 0b00000100) == 0b00000100
	leftNodeEmbedded := (flags & 0b00000010) == 0b00000010
	rightNodeEmbedded := (flags & 0b00000001) == 0b00000001

	// Deserialize shared path
	sharedPath := TrieKeySliceEmpty()
	if sharedPrefixPresent {
		sp, err := deserializeSharedPath(buf)
		if err != nil {
			return nil, fmt.Errorf("deserialize shared path: %w", err)
		}
		sharedPath = sp
	}

	// Deserialize left node reference
	var left *NodeReference = NodeReferenceEmpty()
	if leftNodePresent {
		if leftNodeEmbedded {
			lengthByte, err := buf.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("read left embedded length: %w", err)
			}
			embeddedNode := make([]byte, lengthByte)
			if _, err := buf.Read(embeddedNode); err != nil {
				return nil, fmt.Errorf("read left embedded node: %w", err)
			}
			node, err := fromMessageRSKIP107(embeddedNode, store)
			if err != nil {
				return nil, fmt.Errorf("parse left embedded node: %w", err)
			}
			left = NewNodeReference(store, node, nil)
		} else {
			hash := make([]byte, 32)
			if _, err := buf.Read(hash); err != nil {
				return nil, fmt.Errorf("read left hash: %w", err)
			}
			left = NewNodeReference(store, nil, hash)
		}
	}

	// Deserialize right node reference
	var right *NodeReference = NodeReferenceEmpty()
	if rightNodePresent {
		if rightNodeEmbedded {
			lengthByte, err := buf.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("read right embedded length: %w", err)
			}
			embeddedNode := make([]byte, lengthByte)
			if _, err := buf.Read(embeddedNode); err != nil {
				return nil, fmt.Errorf("read right embedded node: %w", err)
			}
			node, err := fromMessageRSKIP107(embeddedNode, store)
			if err != nil {
				return nil, fmt.Errorf("parse right embedded node: %w", err)
			}
			right = NewNodeReference(store, node, nil)
		} else {
			hash := make([]byte, 32)
			if _, err := buf.Read(hash); err != nil {
				return nil, fmt.Errorf("read right hash: %w", err)
			}
			right = NewNodeReference(store, nil, hash)
		}
	}

	// Deserialize children size (if non-terminal)
	var childrenSize *VarInt
	if leftNodePresent || rightNodePresent {
		// Read remaining bytes to parse VarInt
		remaining := make([]byte, buf.Len())
		n, _ := buf.Read(remaining)
		remaining = remaining[:n]

		vi, err := ReadVarInt(remaining, 0)
		if err != nil {
			return nil, fmt.Errorf("read children size: %w", err)
		}
		childrenSize = &vi
		// Create new reader from remaining data after VarInt
		buf = bytes.NewReader(remaining[vi.Size:])
	}

	// Deserialize value
	var value []byte
	var valueLength Uint24
	var valueHash []byte

	if hasLongVal {
		valueHash = make([]byte, 32)
		if _, err := buf.Read(valueHash); err != nil {
			return nil, fmt.Errorf("read value hash: %w", err)
		}
		lvalueBytes := make([]byte, 3)
		if _, err := buf.Read(lvalueBytes); err != nil {
			return nil, fmt.Errorf("read value length: %w", err)
		}
		valueLength, err = DecodeUint24(lvalueBytes, 0)
		if err != nil {
			return nil, fmt.Errorf("decode value length: %w", err)
		}
		// Long value - would need to retrieve from store
		// value remains nil
	} else {
		remaining := buf.Len()
		if remaining > 0 {
			value = make([]byte, remaining)
			if _, err := buf.Read(value); err != nil {
				return nil, fmt.Errorf("read value: %w", err)
			}
			valueLength = Uint24(len(value))
		}
	}

	return NewTrieFull(store, sharedPath, value, left, right, valueLength, valueHash, childrenSize), nil
}

// fromMessageOrchid deserializes using the pre-RSKIP-107 format
func fromMessageOrchid(message []byte, store TrieStore) (*Trie, error) {
	if len(message) < 6 {
		return nil, fmt.Errorf("orchid message too short")
	}

	current := 0

	// Arity (should be 2)
	arity := message[current]
	current++
	if arity != 2 {
		return nil, fmt.Errorf("invalid arity: %d", arity)
	}

	// Flags
	flags := message[current]
	current++
	hasLongVal := (flags & 0x02) == 2

	// bhashes (2 bytes, big-endian for bit flags)
	bhashes := int(message[current])<<8 | int(message[current+1])
	current += 2

	// lshared (2 bytes, big-endian)
	lshared := int(message[current])<<8 | int(message[current+1])
	current += 2

	// Shared path
	sharedPath := TrieKeySliceEmpty()
	if lshared > 0 {
		lencoded := calculateEncodedLength(lshared)
		if len(message)-current < lencoded {
			return nil, fmt.Errorf("message too short for shared path")
		}
		sharedPath = TrieKeySliceFromEncoded(message, current, lshared, lencoded)
		current += lencoded
	}

	// Left and right nodes
	var left *NodeReference = NodeReferenceEmpty()
	var right *NodeReference = NodeReferenceEmpty()

	if (bhashes & 0b01) != 0 {
		if len(message)-current < 32 {
			return nil, fmt.Errorf("message too short for left hash")
		}
		hash := make([]byte, 32)
		copy(hash, message[current:current+32])
		left = NewNodeReference(store, nil, hash)
		current += 32
	}

	if (bhashes & 0b10) != 0 {
		if len(message)-current < 32 {
			return nil, fmt.Errorf("message too short for right hash")
		}
		hash := make([]byte, 32)
		copy(hash, message[current:current+32])
		right = NewNodeReference(store, nil, hash)
		current += 32
	}

	// Value
	var value []byte
	var valueLength Uint24
	var valueHash []byte

	if hasLongVal {
		if len(message)-current < 32 {
			return nil, fmt.Errorf("message too short for value hash")
		}
		valueHash = make([]byte, 32)
		copy(valueHash, message[current:current+32])
		// Need to retrieve value from store
		if store != nil {
			value = store.RetrieveValue(valueHash)
			if value != nil {
				valueLength = Uint24(len(value))
			}
		}
	} else {
		remaining := len(message) - current
		if remaining > 0 {
			value = make([]byte, remaining)
			copy(value, message[current:])
			valueLength = Uint24(remaining)
		}
	}

	return NewTrieFull(store, sharedPath, value, left, right, valueLength, valueHash, nil), nil
}

// deserializeSharedPath reads a shared path from a buffer
// Format from SharedPathSerializer.SerializeBytes:
// - If 1 <= lshared <= 32: byte = lshared - 1 (so byte 0-31 means length 1-32)
// - If 160 <= lshared <= 382: byte = lshared - 128 (so byte 32-254 means length 160-382)
// - If byte == 255: followed by VarInt
func deserializeSharedPath(buf *bytes.Reader) (*TrieKeySlice, error) {
	lengthByte, err := buf.ReadByte()
	if err != nil {
		return nil, err
	}

	var pathLen int

	if lengthByte <= 31 {
		// Range 1-32: byte = lshared - 1
		pathLen = int(lengthByte) + 1
	} else if lengthByte >= 32 && lengthByte <= 254 {
		// Range 160-382: byte = lshared - 128
		pathLen = int(lengthByte) + 128
	} else {
		// byte == 255: VarInt-prefixed length. Read just the VarInt from the
		// caller's reader (its first byte determines total size) so subsequent
		// fields — encoded path, child refs, value — stay available downstream.
		first, err := buf.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read varint discriminator: %w", err)
		}
		var viSize int
		switch {
		case first < 253:
			viSize = 1
		case first == 253:
			viSize = 3
		case first == 254:
			viSize = 5
		default:
			viSize = 9
		}
		viBytes := make([]byte, viSize)
		viBytes[0] = first
		if viSize > 1 {
			if _, err := buf.Read(viBytes[1:]); err != nil {
				return nil, fmt.Errorf("read varint body: %w", err)
			}
		}
		vi, err := ReadVarInt(viBytes, 0)
		if err != nil {
			return nil, fmt.Errorf("read varint for path length: %w", err)
		}
		pathLen = int(vi.Value)
	}

	encodedLen := calculateEncodedLength(pathLen)
	if encodedLen == 0 {
		return TrieKeySliceEmpty(), nil
	}

	encodedBytes := make([]byte, encodedLen)
	if _, err := buf.Read(encodedBytes); err != nil {
		return nil, fmt.Errorf("read encoded path: %w", err)
	}

	return TrieKeySliceFromEncodedFull(encodedBytes, pathLen), nil
}

// calculateEncodedLength returns the number of bytes needed to encode a path of given bit length
func calculateEncodedLength(bitLength int) int {
	if bitLength == 0 {
		return 0
	}
	return (bitLength + 7) / 8
}

// FromRLPProof decodes an RLP-encoded proof node (as returned by eth_getProof)
func FromRLPProof(rlpEncoded []byte, store TrieStore) (*Trie, error) {
	// The proof nodes from eth_getProof are RLP-encoded: RLP(serialized_node)
	// We need to RLP-decode first to get the raw serialized node
	var serializedNode []byte
	if err := rlp.DecodeBytes(rlpEncoded, &serializedNode); err != nil {
		return nil, fmt.Errorf("RLP decode proof: %w", err)
	}

	return FromMessage(serializedNode, store)
}
