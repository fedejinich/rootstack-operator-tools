package rsktrie

import (
	"bytes"
	"testing"
)

// TestTrie_GetHash_ReturnsIsolatedCopy guards against callers mutating the
// trie's cached hash through the returned slice. Previously the non-empty
// branch of GetHash returned t.hash directly while the empty-trie branch
// returned a copy, so a single `h := node.GetHash(); h[0] ^= 0xff` was
// enough to corrupt every subsequent GetHash call on that node — silently
// breaking proofs and receipts roots derived from the trie.
func TestTrie_GetHash_ReturnsIsolatedCopy(t *testing.T) {
	trie := NewTrie(NewMemTrieStore())
	trie = trie.Put([]byte("k"), []byte("v"))

	h1 := trie.GetHash()
	original := append([]byte(nil), h1...)

	// Mutate the returned slice; the trie's cached hash must not change.
	for i := range h1 {
		h1[i] ^= 0xff
	}

	h2 := trie.GetHash()
	if !bytes.Equal(h2, original) {
		t.Fatalf("GetHash leaked internal slice: want %x, got %x", original, h2)
	}
}
