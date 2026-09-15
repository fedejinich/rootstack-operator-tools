package rsktrie

import (
	"bytes"
	"testing"
)

// TestNodeReference_GetHash_ReturnsIsolatedCopy mirrors the Trie.GetHash
// isolation guard for NodeReference.GetHash, which also handed out the
// cached slice directly. A caller mutating the returned hash corrupted
// the NodeReference's lazyHash for every subsequent read — silently
// invalidating proofs and serializations derived from that reference.
func TestNodeReference_GetHash_ReturnsIsolatedCopy(t *testing.T) {
	trie := NewTrie(NewMemTrieStore())
	trie = trie.Put([]byte("k"), []byte("v"))

	ref := NewNodeReference(nil, trie, nil)
	h1 := ref.GetHash()
	original := append([]byte(nil), h1...)

	for i := range h1 {
		h1[i] ^= 0xff
	}

	h2 := ref.GetHash()
	if !bytes.Equal(h2, original) {
		t.Fatalf("NodeReference.GetHash leaked cached slice: want %x, got %x", original, h2)
	}
}

// TestTrie_GetValueHash_ReturnsIsolatedCopy covers the same mutation
// hazard on Trie.GetValueHash. The cached valueHash feeds long-value
// serialization and proof construction, so a corrupted slice produces
// wrong trie roots downstream.
func TestTrie_GetValueHash_ReturnsIsolatedCopy(t *testing.T) {
	longVal := makeValue(64)
	trie := NewTrieFull(
		NewMemTrieStore(),
		TrieKeySliceEmpty(),
		longVal,
		NodeReferenceEmpty(),
		NodeReferenceEmpty(),
		Uint24(len(longVal)),
		nil,
		&VarInt{Value: 0, Size: 1},
	)

	h1 := trie.GetValueHash()
	original := append([]byte(nil), h1...)

	for i := range h1 {
		h1[i] ^= 0xff
	}

	h2 := trie.GetValueHash()
	if !bytes.Equal(h2, original) {
		t.Fatalf("Trie.GetValueHash leaked cached slice: want %x, got %x", original, h2)
	}
}
