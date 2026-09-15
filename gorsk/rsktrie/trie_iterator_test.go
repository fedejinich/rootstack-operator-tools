package rsktrie

import (
	"encoding/hex"
	"testing"
)

func TestIterationElement(t *testing.T) {
	expandedKey := []byte{0, 1, 1, 0, 1, 1, 0, 0, 0, 1}
	// Java: new TrieKeySlice(expandedKey, 1, 7). Offset=1, Limit=7. Length=6.
	// NewTrieKeySlice takes the same (offset, limit) pair.
	slice := NewTrieKeySlice(expandedKey, 1, 7)

	ie := NewIterationElement(slice, nil)
	dump := ie.String()

	if dump != "110110" {
		t.Errorf("Expected 110110, got %s", dump)
	}
}

func buildTestTrie() *Trie {
	t := NewTrie(NewMemTrieStore())
	t = t.Put(decodeHex("0a"), []byte{0x06})
	t = t.Put(decodeHex("0a00"), []byte{0x02})
	t = t.Put(decodeHex("0a80"), []byte{0x07})
	t = t.Put(decodeHex("0a0000"), []byte{0x01})
	t = t.Put(decodeHex("0a0080"), []byte{0x04})
	t = t.Put(decodeHex("0a008000"), []byte{0x03})
	t = t.Put(decodeHex("0a008080"), []byte{0x05})
	t = t.Put(decodeHex("0a8080"), []byte{0x08})
	t = t.Put(decodeHex("0a808000"), []byte{0x09})
	return t
}

func decodeHex(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

func TestInOrderIterator(t *testing.T) {
	trie := buildTestTrie()
	expected := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x09, 0x08}

	it := trie.GetInOrderIterator()
	idx := 0
	for it.HasNext() {
		el := it.Next()
		val := el.node.GetValue()
		if len(val) != 1 {
			t.Errorf("Unexpected val len %d", len(val))
		}
		if val[0] != expected[idx] {
			t.Errorf("Idx %d: expected %x got %x", idx, expected[idx], val[0])
		}
		idx++
	}
	if idx != len(expected) {
		t.Errorf("Count mismatch")
	}
}

func TestPreOrderIterator(t *testing.T) {
	trie := buildTestTrie()
	expected := []byte{0x06, 0x02, 0x01, 0x04, 0x03, 0x05, 0x07, 0x08, 0x09}

	it := trie.GetPreOrderIterator()
	idx := 0
	for it.HasNext() {
		el := it.Next()
		val := el.node.GetValue()
		if len(val) != 1 {
			t.Errorf("Unexpected val len %d", len(val))
		}
		if val[0] != expected[idx] {
			t.Errorf("Idx %d: expected %x got %x", idx, expected[idx], val[0])
		}
		idx++
	}
	if idx != len(expected) {
		t.Errorf("Count mismatch")
	}
}

func TestPostOrderIterator(t *testing.T) {
	trie := buildTestTrie()
	expected := []byte{0x01, 0x03, 0x05, 0x04, 0x02, 0x09, 0x08, 0x07, 0x06}

	it := trie.GetPostOrderIterator()
	idx := 0
	for it.HasNext() {
		el := it.Next()
		val := el.node.GetValue()
		if len(val) != 1 {
			t.Errorf("Unexpected val len %d", len(val))
		}
		if val[0] != expected[idx] {
			t.Errorf("Idx %d: expected %x got %x", idx, expected[idx], val[0])
		}
		idx++
	}
	if idx != len(expected) {
		t.Errorf("Count mismatch")
	}
}
