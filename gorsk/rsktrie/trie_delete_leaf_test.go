package rsktrie

import (
	"testing"
)

// TestTrie_DeleteLeaf_ResetsSharedPath asserts that deleting the only key
// from a trie yields a node that is observationally identical to a fresh
// empty trie — same IsEmptyTrie status AND a cleared sharedPath. The
// previous Delete path retained the deleted leaf's sharedPath, leaving a
// residual that could change downstream Put behavior via the Split path
// in subtle ways (or hide bugs behind incidental NodeReference cleanup).
func TestTrie_DeleteLeaf_ResetsSharedPath(t *testing.T) {
	key := []byte{0xAA, 0xBB, 0xCC}
	val := []byte("value")

	trie := NewTrie(NewMemTrieStore())
	trie = trie.Put(key, val)
	trie = trie.Delete(key)

	if !trie.IsEmptyTrie() {
		t.Fatalf("trie not empty after deleting only key")
	}
	if got := trie.GetSharedPath().Length(); got != 0 {
		t.Fatalf("sharedPath after deleting only key: want 0 bits, got %d bits", got)
	}
}
