package rsktrie

import (
	"bytes"
	"testing"
)

func TestGetNotNullHashOnEmptyTrie(t *testing.T) {
	trie := NewTrie(NewMemTrieStore())
	if trie.GetHash() == nil {
		t.Error("Hash should not be nil")
	}
}

func TestGetHashAs32BytesOnEmptyTrie(t *testing.T) {
	trie := NewTrie(NewMemTrieStore())
	if len(trie.GetHash()) != 32 {
		t.Errorf("Expected 32 bytes, got %d", len(trie.GetHash()))
	}
}

func TestEmptyTriesHasTheSameHash(t *testing.T) {
	trie1 := NewTrie(NewMemTrieStore())
	trie2 := NewTrie(NewMemTrieStore())
	trie3 := NewTrie(NewMemTrieStore())

	if !bytes.Equal(trie1.GetHash(), trie1.GetHash()) {
		t.Error("trie1 self hash mismatch")
	}
	if !bytes.Equal(trie1.GetHash(), trie2.GetHash()) {
		t.Error("trie1 and trie2 hash mismatch")
	}
	if !bytes.Equal(trie3.GetHash(), trie2.GetHash()) {
		t.Error("trie3 and trie2 hash mismatch")
	}
}

func TestEmptyHashForEmptyTrie(t *testing.T) {
	trie := NewTrie(NewMemTrieStore())
	if !bytes.Equal(trie.GetHash(), EmptyHash) {
		t.Error("Empty trie hash mismatch")
	}
}

func TestNonEmptyHashForNonEmptyTrie(t *testing.T) {
	trie := NewTrie(NewMemTrieStore())
	trie = trie.Put([]byte("foo"), []byte("bar"))

	if bytes.Equal(trie.GetHash(), EmptyHash) {
		t.Error("Non-empty trie should not have empty hash")
	}
}

func TestNonEmptyHashForNonEmptyTrieWithLongValue(t *testing.T) {
	trie := NewTrie(NewMemTrieStore())
	trie = trie.Put([]byte("foo"), makeValue(100))

	if bytes.Equal(trie.GetHash(), EmptyHash) {
		t.Error("Non-empty trie should not have empty hash")
	}
}

func TestTriesWithSameKeyValuesHaveSameHash(t *testing.T) {
	trie1 := NewTrie(NewMemTrieStore()).Put([]byte("foo"), []byte("bar")).Put([]byte("bar"), []byte("baz"))
	trie2 := NewTrie(NewMemTrieStore()).Put([]byte("foo"), []byte("bar")).Put([]byte("bar"), []byte("baz"))

	if !bytes.Equal(trie1.GetHash(), trie2.GetHash()) {
		t.Error("Hashes should be equal")
	}
}

func TestTriesWithSameKeyLongValuesHaveSameHash(t *testing.T) {
	trie1 := NewTrie(NewMemTrieStore()).Put([]byte("foo"), []byte("bar")).Put([]byte("bar"), makeValue(100))
	trie2 := NewTrie(NewMemTrieStore()).Put([]byte("foo"), []byte("bar")).Put([]byte("bar"), makeValue(100))

	if !bytes.Equal(trie1.GetHash(), trie2.GetHash()) {
		t.Error("Hashes should be equal")
	}
}

func TestTriesWithSameKeyValuesInsertedInDifferentOrderHaveSameHash(t *testing.T) {
	trie1 := NewTrie(NewMemTrieStore()).Put([]byte("foo"), []byte("bar")).Put([]byte("bar"), []byte("baz"))
	trie2 := NewTrie(NewMemTrieStore()).Put([]byte("bar"), []byte("baz")).Put([]byte("foo"), []byte("bar"))

	if !bytes.Equal(trie1.GetHash(), trie2.GetHash()) {
		t.Error("Hashes should be equal")
	}
}

func TestTriesWithSameKeyLongValuesInsertedInDifferentOrderHaveSameHash(t *testing.T) {
	trie1 := NewTrie(NewMemTrieStore()).Put([]byte("foo"), makeValue(100)).Put([]byte("bar"), makeValue(200))
	trie2 := NewTrie(NewMemTrieStore()).Put([]byte("bar"), makeValue(200)).Put([]byte("foo"), makeValue(100))

	if !bytes.Equal(trie1.GetHash(), trie2.GetHash()) {
		t.Error("Hashes should be equal")
	}
}

func TestThreeTriesWithSameKeyValuesInsertedInDifferentOrderHaveSameHash(t *testing.T) {
	trie1 := NewTrie(NewMemTrieStore()).Put([]byte("foo"), []byte("bar")).Put([]byte("bar"), []byte("baz")).Put([]byte("baz"), []byte("foo"))
	trie2 := NewTrie(NewMemTrieStore()).Put([]byte("bar"), []byte("baz")).Put([]byte("baz"), []byte("foo")).Put([]byte("foo"), []byte("bar"))
	trie3 := NewTrie(NewMemTrieStore()).Put([]byte("baz"), []byte("foo")).Put([]byte("bar"), []byte("baz")).Put([]byte("foo"), []byte("bar"))

	if !bytes.Equal(trie1.GetHash(), trie2.GetHash()) {
		t.Error("1 and 2 mismatch")
	}
	if !bytes.Equal(trie3.GetHash(), trie2.GetHash()) {
		t.Error("3 and 2 mismatch")
	}
}

func TestTriesWithDifferentKeyValuesHaveDifferentHashes(t *testing.T) {
	trie1 := NewTrie(NewMemTrieStore()).Put([]byte("foo"), []byte("bar")).Put([]byte("bar"), []byte("42"))
	trie2 := NewTrie(NewMemTrieStore()).Put([]byte("foo"), []byte("bar")).Put([]byte("bar"), []byte("baz"))

	if bytes.Equal(trie1.GetHash(), trie2.GetHash()) {
		t.Error("Hashes should be different")
	}
}
