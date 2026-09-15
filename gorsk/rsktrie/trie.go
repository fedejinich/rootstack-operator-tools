package rsktrie

import (
	"bytes"

	"golang.org/x/crypto/sha3"
)

func Keccak256(data []byte) []byte {
	hash := sha3.NewLegacyKeccak256()
	hash.Write(data)
	return hash.Sum(nil)
}

type Trie struct {
	value        []byte
	left         *NodeReference
	right        *NodeReference
	store        TrieStore
	sharedPath   *TrieKeySlice
	valueLength  Uint24
	valueHash    []byte
	childrenSize *VarInt

	hash    []byte
	encoded []byte
	saved   bool
}

func NewTrie(store TrieStore) *Trie {
	return NewTrieFull(store, TrieKeySliceEmpty(), nil, NodeReferenceEmpty(), NodeReferenceEmpty(), 0, nil, &VarInt{Value: 0, Size: 1})
}

func NewTrieFull(store TrieStore, sharedPath *TrieKeySlice, value []byte, left, right *NodeReference, valueLength Uint24, valueHash []byte, childrenSize *VarInt) *Trie {
	t := &Trie{
		store:        store,
		sharedPath:   sharedPath,
		value:        value,
		left:         left,
		right:        right,
		valueLength:  valueLength,
		valueHash:    valueHash,
		childrenSize: childrenSize,
	}
	if value != nil {
		t.valueLength = Uint24(len(value))
	}
	return t
}

func (t *Trie) IsEmptyTrie() bool {
	return t.valueLength == 0 && t.left.IsEmpty() && t.right.IsEmpty()
}

func (t *Trie) Get(key []byte) []byte {
	return t.GetByKeySlice(TrieKeySliceFromKey(key))
}

func (t *Trie) GetByKeySlice(key *TrieKeySlice) []byte {
	node := t.Find(key)
	if node == nil {
		return nil
	}
	return node.GetValue()
}

func (t *Trie) GetValue() []byte {
	if t.value == nil && t.valueLength > 0 && t.valueHash != nil && t.store != nil {
		// Long value: bytes are stored externally keyed by valueHash. Cache
		// the result on the node so repeated reads don't re-hit the store.
		if v := t.store.RetrieveValue(t.valueHash); v != nil {
			t.value = v
		}
	}
	if t.value == nil {
		return nil
	}
	val := make([]byte, len(t.value))
	copy(val, t.value)
	return val
}

func (t *Trie) Find(key *TrieKeySlice) *Trie {
	if t.sharedPath.Length() > key.Length() {
		return nil
	}

	common := key.CommonPath(t.sharedPath)
	if common.Length() < t.sharedPath.Length() {
		return nil
	}

	if common.Length() == key.Length() {
		return t
	}

	// Implicit byte check
	implicitByte := key.Get(common.Length())
	node := t.RetrieveNode(implicitByte)
	if node == nil {
		return nil
	}

	return node.Find(key.Slice(common.Length()+1, key.Length()))
}

func (t *Trie) RetrieveNode(implicitByte byte) *Trie {
	if implicitByte == 0 {
		return t.left.GetNode()
	}
	return t.right.GetNode()
}

func (t *Trie) Put(key []byte, value []byte) *Trie {
	return t.PutKeySlice(TrieKeySliceFromKey(key), value)
}

func (t *Trie) PutKeySlice(key *TrieKeySlice, value []byte) *Trie {
	// Treat empty value as delete
	if value != nil && len(value) == 0 {
		value = nil
	}

	newTrie := t.InternalPut(key, value)
	if newTrie == nil {
		return NewTrie(t.store)
	}
	return newTrie
}

func (t *Trie) InternalPut(key *TrieKeySlice, value []byte) *Trie {
	commonPath := key.CommonPath(t.sharedPath)

	// Case 1: Shared path mismatch (split needed)
	if commonPath.Length() < t.sharedPath.Length() {
		if value == nil {
			return t // Deleting non-existent key
		}
		return t.Split(commonPath).PutKeySlice(key, value)
	}

	// Case 2: Exact match or sub-key
	if t.sharedPath.Length() >= key.Length() {
		// Exact match
		// Check for equality optimization
		if t.valueLength == Uint24(len(value)) && bytes.Equal(t.GetValue(), value) {
			return t
		}
		if value == nil {
			// Deleting the value at this exact node. If the node had no
			// children either, the whole node is gone — return nil so Put
			// substitutes a fresh empty trie (not a residual carrying the
			// deleted key's sharedPath, which would change downstream Put
			// behavior). Interior nodes with surviving children keep their
			// sharedPath but lose the value; deeper coalescing is left for
			// when a non-terminal delete strategy is needed.
			if t.left.IsEmpty() && t.right.IsEmpty() {
				return nil
			}
			return NewTrieFull(t.store, t.sharedPath, nil, t.left, t.right, 0, nil, t.childrenSize)
		}

		return NewTrieFull(t.store, t.sharedPath, value, t.left, t.right, Uint24(len(value)), nil, t.childrenSize)
	}

	// Case 3: Recursive put in children
	if t.IsEmptyTrie() {
		// Deleting a non-existent key from an empty trie is a no-op —
		// don't fabricate a residual node carrying the absent key's
		// sharedPath (which would surface via GetSharedPath even though
		// IsEmptyTrie still reports true).
		if value == nil {
			return t
		}
		return NewTrieFull(t.store, key, value, NodeReferenceEmpty(), NodeReferenceEmpty(), Uint24(len(value)), nil, nil)
	}

	pos := key.Get(t.sharedPath.Length())
	node := t.RetrieveNode(pos)
	if node == nil {
		node = NewTrie(t.store)
	}

	subKey := key.Slice(t.sharedPath.Length()+1, key.Length())
	newNode := node.PutKeySlice(subKey, value)

	if newNode == node {
		return t // No change
	}

	newLeft := t.left
	newRight := t.right
	newNodeRef := NewNodeReference(t.store, newNode, nil)

	if pos == 0 {
		newLeft = newNodeRef
	} else {
		newRight = newNodeRef
	}

	// Children just changed; pass nil so GetChildrenSize recomputes from
	// the new left/right references instead of reusing the parent's cached
	// VarInt (which would serialize a stale size and yield a wrong hash).
	return NewTrieFull(t.store, t.sharedPath, t.value, newLeft, newRight, t.valueLength, t.valueHash, nil)
}

func (t *Trie) Split(commonPath *TrieKeySlice) *Trie {
	commonLen := commonPath.Length()
	newChildSharedPath := t.sharedPath.Slice(commonLen+1, t.sharedPath.Length())

	newChildTrie := NewTrieFull(t.store, newChildSharedPath, t.value, t.left, t.right, t.valueLength, t.valueHash, t.childrenSize)
	newChildRef := NewNodeReference(t.store, newChildTrie, nil)

	pos := t.sharedPath.Get(commonLen)
	var newLeft, newRight *NodeReference
	if pos == 0 {
		newLeft = newChildRef
		newRight = NodeReferenceEmpty()
	} else {
		newLeft = NodeReferenceEmpty()
		newRight = newChildRef
	}

	return NewTrieFull(t.store, commonPath, nil, newLeft, newRight, 0, nil, nil) // ChildrenSize needs recalc
}

func (t *Trie) Delete(key []byte) *Trie {
	return t.Put(key, nil)
}

const (
	MaxEmbeddedNodeSizeInBytes = 44
)

// EmptyHash is the Keccak256 hash of the RLP encoding of an empty byte array.
// RLP([]) -> 0x80. Keccak(0x80) -> ...
var EmptyHash = Keccak256([]byte{0x80})

// TODO: Actually RLP encode element empty byte array gives 0x80.
// Java logic: makeEmptyHash() -> keccak(RLP.encodeElement(EMPTY_BYTE_ARRAY))
// EMPTY_BYTE_ARRAY is byte[0].
// RLP(byte[0]) -> 0x80.
// So EmptyHash is Keccak(0x80).

func (t *Trie) GetHash() []byte {
	if t.IsEmptyTrie() {
		val := make([]byte, 32)
		copy(val, EmptyHash)
		return val
	}
	if t.hash == nil {
		msg := t.ToMessage()
		t.hash = Keccak256(msg)
	}
	// Always return a copy so callers can't mutate the cached value.
	out := make([]byte, len(t.hash))
	copy(out, t.hash)
	return out
}

func (t *Trie) ToMessage() []byte {
	if t.encoded == nil {
		t.InternalToMessage()
	}
	// Return copy
	cp := make([]byte, len(t.encoded))
	copy(cp, t.encoded)
	return cp
}

func (t *Trie) GetMessageLength() int {
	if t.encoded == nil {
		t.InternalToMessage()
	}
	return len(t.encoded)
}

func (t *Trie) InternalToMessage() {
	// Logic from internalToMessage
	lvalue := t.valueLength
	hasLongVal := t.HasLongValue()

	sps := NewSharedPathSerializer(t.sharedPath)
	childrenSize := t.GetChildrenSize()

	buf := new(bytes.Buffer)

	// Flags
	// 01000000
	var flags byte = 0b01000000
	if hasLongVal {
		flags |= 0b00100000
	}
	if sps.IsPresent() {
		flags |= 0b00010000
	}
	if !t.left.IsEmpty() {
		flags |= 0b00001000
	}
	if !t.right.IsEmpty() {
		flags |= 0b00000100
	}
	if t.left.IsEmbeddable() {
		flags |= 0b00000010
	}
	if t.right.IsEmbeddable() {
		flags |= 0b00000001
	}

	buf.WriteByte(flags)
	sps.SerializeInto(buf)

	t.left.SerializeInto(buf)
	t.right.SerializeInto(buf)

	if !t.IsTerminal() {
		buf.Write(childrenSize.Encode())
	}

	if hasLongVal {
		buf.Write(t.GetValueHash())
		buf.Write(lvalue.Encode())
	} else if lvalue > 0 {
		buf.Write(t.GetValue())
	}

	t.encoded = buf.Bytes()
}

func (t *Trie) GetValueHash() []byte {
	if t.valueHash == nil && t.valueLength > 0 {
		t.valueHash = Keccak256(t.GetValue())
	}
	if t.valueHash == nil {
		return nil
	}
	// Return a copy so callers can't mutate the cached value hash.
	out := make([]byte, len(t.valueHash))
	copy(out, t.valueHash)
	return out
}

func (t *Trie) HasLongValue() bool {
	return t.valueLength > 32
}

func (t *Trie) IsTerminal() bool {
	return t.left.IsEmpty() && t.right.IsEmpty()
}

func (t *Trie) IsEmbeddable() bool {
	// Should only be called during save/serialization context usually
	return t.IsTerminal() && t.GetMessageLength() <= MaxEmbeddedNodeSizeInBytes
}

func (t *Trie) GetChildrenSize() *VarInt {
	if t.childrenSize == nil {
		if t.IsTerminal() {
			t.childrenSize = &VarInt{Value: 0}
		} else {
			// left.referenceSize() + right.referenceSize()
			// Go NodeReference ReferenceSize calls getNode().TrieSize()?
			// No, Java ReferenceSize logic:
			// trie.getChildrenSize().value + externalValueLength + trie.getMessageLength()
			// So it's the SERIALIZED size + children size.
			// "Size of this node along with its children"

			ls := t.left.ReferenceSize()
			rs := t.right.ReferenceSize()
			t.childrenSize = &VarInt{Value: uint64(ls + rs)}
		}
	}
	return t.childrenSize
}

func (t *Trie) TrieSize() int {
	s := 1
	if l := t.left.GetNode(); l != nil {
		s += l.TrieSize()
	}
	if r := t.right.GetNode(); r != nil {
		s += r.TrieSize()
	}
	return s
}

func (t *Trie) GetInOrderIterator() *InOrderIterator {
	return NewInOrderIterator(t)
}

func (t *Trie) GetPreOrderIterator() *PreOrderIterator {
	return NewPreOrderIterator(t)
}

func (t *Trie) GetPostOrderIterator() *PostOrderIterator {
	return NewPostOrderIterator(t)
}

// GetSharedPath returns the shared path of this trie node
func (t *Trie) GetSharedPath() *TrieKeySlice {
	return t.sharedPath
}

// GetLeft returns the left child reference
func (t *Trie) GetLeft() *NodeReference {
	return t.left
}

// GetRight returns the right child reference
func (t *Trie) GetRight() *NodeReference {
	return t.right
}
