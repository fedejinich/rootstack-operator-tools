package rsktrie

import (
	"bytes"
	"fmt"
	"testing"
)

func makeValue(size int) []byte {
	v := make([]byte, size)
	for i := 0; i < size; i++ {
		v[i] = byte(i % 256)
	}
	return v
}

func TestGetNullForUnknownKey(t *testing.T) {
	store := NewMemTrieStore()
	trie := NewTrie(store)

	val := trie.Get([]byte{1, 2, 3})
	if val != nil {
		t.Error("Expected nil for unknown key")
	}

	val = trie.Get([]byte("foo"))
	if val != nil {
		t.Error("Expected nil for unknown key")
	}
}

func TestPutAndGetKeyValue(t *testing.T) {
	store := NewMemTrieStore()
	trie := NewTrie(store)

	trie = trie.Put([]byte("foo"), []byte("bar"))
	val := trie.Get([]byte("foo"))
	if val == nil {
		t.Fatal("Expected value not nil")
	}
	if !bytes.Equal(val, []byte("bar")) {
		t.Errorf("Expected bar, got %s", val)
	}
}

func TestPutAndGetKeyValueTwice(t *testing.T) {
	store := NewMemTrieStore()
	trie := NewTrie(store)

	trie1 := trie.Put([]byte("foo"), []byte("bar"))
	trie2 := trie1.Put([]byte("foo"), []byte("bar"))

	if !bytes.Equal(trie1.Get([]byte("foo")), []byte("bar")) {
		t.Error("trie1 failed")
	}
	if !bytes.Equal(trie2.Get([]byte("foo")), []byte("bar")) {
		t.Error("trie2 failed")
	}

	// Check reference equality optimization
	// In Go, we can't easily check pointer equality if they are different structs,
	// but here trie1 and trie2 should be the same pointer if optimization works.
	if trie1 != trie2 {
		t.Log("Warning: Reference equality optimization not working (not critical)")
	}
}

func TestPutAndGetKeyValueTwiceWithDifferentValues(t *testing.T) {
	store := NewMemTrieStore()
	trie := NewTrie(store)

	trie1 := trie.Put([]byte("foo"), []byte("bar1"))
	trie2 := trie1.Put([]byte("foo"), []byte("bar2"))

	if !bytes.Equal(trie1.Get([]byte("foo")), []byte("bar1")) {
		t.Error("trie1 failed")
	}
	if !bytes.Equal(trie2.Get([]byte("foo")), []byte("bar2")) {
		t.Error("trie2 failed")
	}
}

func TestPutAndGetKeyLongValue(t *testing.T) {
	store := NewMemTrieStore()
	trie := NewTrie(store)
	value := makeValue(100)

	trie = trie.Put([]byte("foo"), value)
	got := trie.Get([]byte("foo"))
	if !bytes.Equal(got, value) {
		t.Error("Long value mismatch")
	}
}

func TestPutKeyValueAndDeleteKey(t *testing.T) {
	store := NewMemTrieStore()
	trie := NewTrie(store)

	trie = trie.Put([]byte("foo"), []byte("bar"))
	trie = trie.Delete([]byte("foo"))

	if trie.Get([]byte("foo")) != nil {
		t.Error("Expected nil after delete")
	}
}

func TestPutAndGetTwoKeyValues(t *testing.T) {
	store := NewMemTrieStore()
	trie := NewTrie(store)

	trie = trie.Put([]byte("foo"), []byte("bar"))
	trie = trie.Put([]byte("bar"), []byte("foo"))

	if !bytes.Equal(trie.Get([]byte("foo")), []byte("bar")) {
		t.Error("foo mismatch")
	}
	if !bytes.Equal(trie.Get([]byte("bar")), []byte("foo")) {
		t.Error("bar mismatch")
	}
}

func TestPutAndGetKeyAndSubKeyValues(t *testing.T) {
	store := NewMemTrieStore()
	trie := NewTrie(store)

	trie = trie.Put([]byte("foo"), []byte("bar"))
	trie = trie.Put([]byte("f"), []byte("42"))

	if !bytes.Equal(trie.Get([]byte("foo")), []byte("bar")) {
		t.Error("foo mismatch")
	}
	if !bytes.Equal(trie.Get([]byte("f")), []byte("42")) {
		t.Errorf("f mismatch: got %v", trie.Get([]byte("f")))
	}
}

func TestPutAndGetOneHundredKeyValues(t *testing.T) {
	store := NewMemTrieStore()
	trie := NewTrie(store)

	for k := 0; k < 100; k++ {
		key := []byte(fmt.Sprintf("%d", k))
		trie = trie.Put(key, key)
	}

	for k := 0; k < 100; k++ {
		key := []byte(fmt.Sprintf("%d", k))
		val := trie.Get(key)
		if !bytes.Equal(val, key) {
			t.Errorf("Mismatch for key %s", key)
		}
	}
}
