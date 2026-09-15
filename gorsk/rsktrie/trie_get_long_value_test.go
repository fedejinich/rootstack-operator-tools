package rsktrie

import (
	"bytes"
	"testing"
)

// TestTrie_GetValue_LongValueFromStore covers the case where a Trie node has
// only a value hash (the value itself is stored externally because it exceeds
// the inline threshold). GetValue must consult the store to materialize the
// real bytes; previously it returned nil because long-value retrieval was a
// TODO, which silently dropped values for any deserialized proof/storage
// node referencing a long value.
func TestTrie_GetValue_LongValueFromStore(t *testing.T) {
	store := NewMemTrieStore()

	longVal := makeValue(64) // > 32 bytes, lives in the value store
	hash := Keccak256(longVal)
	store.AddValue(hash, longVal)

	trie := NewTrieFull(
		store,
		TrieKeySliceEmpty(),
		nil, // value not inlined
		NodeReferenceEmpty(),
		NodeReferenceEmpty(),
		Uint24(len(longVal)),
		hash,
		&VarInt{Value: 0, Size: 1},
	)

	got := trie.GetValue()
	if !bytes.Equal(got, longVal) {
		t.Fatalf("GetValue with long value in store: want %x, got %x", longVal, got)
	}
}
