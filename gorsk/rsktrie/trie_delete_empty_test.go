package rsktrie

import "testing"

// TestTrie_DeleteNonExistentOnEmpty_IsNoOp guards against Case 3
// constructing a residual node when Put is called with value=nil on an
// already-empty trie. Previously InternalPut hit the "IsEmptyTrie" branch
// regardless of the value, returning a fresh leaf with the deleted key's
// sharedPath and valueLength=0 — observationally non-empty in sharedPath
// even though IsEmptyTrie() still reported true.
func TestTrie_DeleteNonExistentOnEmpty_IsNoOp(t *testing.T) {
	trie := NewTrie(NewMemTrieStore())
	after := trie.Delete([]byte{0xAA, 0xBB, 0xCC})

	if !after.IsEmptyTrie() {
		t.Fatalf("delete on empty trie produced non-empty trie")
	}
	if got := after.GetSharedPath().Length(); got != 0 {
		t.Fatalf("delete on empty trie left residual sharedPath: %d bits", got)
	}
}
