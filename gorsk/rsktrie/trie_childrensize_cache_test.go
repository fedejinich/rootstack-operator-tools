package rsktrie

import (
	"bytes"
	"testing"
)

// TestTrie_Put_RecomputesChildrenSizeAfterCache covers a caching bug in
// InternalPut: when a recursive Put replaces one child of an interior node,
// the new parent is constructed with the *old* parent's cached childrenSize
// VarInt. If GetHash / ToMessage had already populated that cache, the
// resulting node serializes with a stale children-size prefix and a wrong
// hash compared to the same content built without the intermediate cache.
//
// We verify the invariant via hash equivalence: building the same logical
// trie twice — once with an intermediate GetHash that caches childrenSize,
// once without — must yield the same root hash.
func TestTrie_Put_RecomputesChildrenSizeAfterCache(t *testing.T) {
	k1 := []byte{0x00, 0x11}
	k2 := []byte{0x80, 0x22}
	k3 := []byte{0x40, 0x33}

	v1 := []byte("v1")
	v2 := []byte("v2")
	v3 := []byte("v3")

	// Reference build: no intermediate hash so childrenSize is never cached
	// before the final Put.
	ref := NewTrie(NewMemTrieStore())
	ref = ref.Put(k1, v1)
	ref = ref.Put(k2, v2)
	ref = ref.Put(k3, v3)
	want := ref.GetHash()

	// Cached build: force childrenSize to cache on the interior node before
	// the third Put triggers the recursive Case 3 path in InternalPut.
	got := NewTrie(NewMemTrieStore())
	got = got.Put(k1, v1)
	got = got.Put(k2, v2)
	_ = got.GetHash() // populates childrenSize on the current root
	got = got.Put(k3, v3)
	have := got.GetHash()

	if !bytes.Equal(want, have) {
		t.Fatalf("hash mismatch after Put on cached interior node:\n  want=%x\n  got =%x", want, have)
	}
}
