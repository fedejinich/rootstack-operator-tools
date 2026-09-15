package rsktrie

import (
	"bytes"
	"testing"
)

func TestBytesToKey(t *testing.T) {
	// Java: TrieKeySlice.fromKey(new byte[]{(byte) 0xaa}).encode()
	// 0xaa = 10101010
	// PathEncoder.encode should match expected bytes.

	key := []byte{0xaa}
	slice := TrieKeySliceFromKey(key)
	encoded := slice.Encode()

	// Java expectation: 0x01, 0x00, 0x01, 0x00, 0x01, 0x00, 0x01, 0x00
	// expected := []byte{1, 0, 1, 0, 1, 0, 1, 0}

	// My PathEncoder logic:
	// 0xaa = 10101010
	// k=0 (bit 7): 1 -> 0x80 >> 0 = 0x80? No.
	// Java Loop:
	// offset = k % 8. k=0 -> offset=0.
	// if k>0 && offset==0 nbyte++.
	// path[k] == 1 ?
	// encoded[nbyte] |= 0x80 >> offset.
	//
	// Wait, PathEncoder in Go I implemented calls `PathEncoderEncode`.
	// What did `path` input look like?
	// `TrieKeySliceFromKey`: `expandedKey := PathEncoderDecode(key, len(key)*8)`
	// `PathEncoderDecode`: decodes bytes to bits (0/1 array).
	// `0xaa` -> 1,0,1,0,1,0,1,0.
	//
	// Then `slice.Encode()` calls `PathEncoderEncode(slice)`.
	// `PathEncoderEncode`: takes bits arrays [1,0,1,0...].
	// AND produces the PACKED bytes.
	//
	// `Trie.java` `PathEncoder.encode` takes `byte[] path` (bits).
	// `TrieKeySliceTest.java` says:
	// `PathEncoder.encode(new byte[] { 0x01, 0x00... })`
	// So `PathEncoder.encode` is expected to return PACKED bytes from BIT bytes?
	//
	// Let's re-read Java `PathEncoder.java`.
	// `encodeBinaryPath`: inputs `path` (bits). Ouputs packed `encoded`.
	//
	// `TrieKeySliceTest`:
	// Assertions.assertArrayEquals(
	//    PathEncoder.encode(new byte[] { 0x01, 0x00, 0x01, 0x00, 0x01, 0x00, 0x01, 0x00 }),
	//    TrieKeySlice.fromKey(new byte[]{(byte) 0xaa}).encode()
	// );
	//
	// Left side: `PathEncoder.encode` of explicit bits.
	// Right side: `fromKey(0xaa).encode()`.
	// `fromKey(0xaa)` decodes `0xaa` to bits [1,0,1,0...].
	// `encode()` then encodes those bits back to PACKED.
	//
	// So `fromKey(0xaa).encode()` returns PACKED bytes.
	// Does `PathEncoder.encode` return packed bytes? Yes.
	//
	// So TestBytesToKey asserts that Pack([1,0,1,0...]) == Pack(Unpack(0xaa)).
	// This implies Unpack(0xaa) == [1,0,1,0...].
	//
	// However, the test code provided in Java `TrieKeySliceTest` compares output of `PathEncoder.encode(...)`.
	// It does NOT compare against raw bytes `0xaa`.
	// Wait, `PathEncoder.encode([1,0,1,0...])` should pack to `0xaa`?
	//
	// Let's look at `PathEncoder.java` logic again.
	// nbyte=0. k=0. bit=1. `encoded[0] |= 0x80 >> 0`. -> 0x80.
	// k=1. bit=0.
	// ...
	// Result is indeed `0xaa`.
	//
	// BUT the Test calls `PathEncoder.encode( ... )`.
	// So both sides run `encode`.
	//
	// Go test:
	// `slice := TrieKeySliceFromKey([]byte{0xaa})` -> internal `expandedKey` = [1,0,1,0,1,0,1,0]
	// `encoded := slice.Encode()` -> returns packed bytes, i.e., `0xaa`.
	//
	// So `encoded` should be `[]byte{0xaa}`.
	//
	// Java test:
	// `PathEncoder.encode({1,0,1...})` -> returns `0xaa`.
	// `TrieKeySlice.fromKey(0xaa).encode()` -> returns `0xaa`.
	// They are equal.
	//
	// So in my Go test, I should assert `encoded` equals `[]byte{0xaa}`.

	if !bytes.Equal(encoded, key) {
		t.Errorf("Expected %x, got %x", key, encoded)
	}

	// Slice test
	// slice(2, 8).
	// Original bits: 1,0,1,0,1,0,1,0.
	// Slice 2..8:    1,0,1,0,1,0 (indices 2 to 7).
	// Encode([1,0,1,0,1,0]) -> 101010xx.
	// 6 bits.
	// k=0->1(0x80), k=1->0, k=2->1(0x20), k=3->0, k=4->1(0x08), k=5->0.
	// Total: 0x80 | 0x20 | 0x08 = 0xA8.
	// Length 6 bits -> 1 byte.

	// TrieKeySlice.java: `newLimit = offset + to`.
	// `length() = limit - offset`.
	// `slice(from, to)` -> `offset+from`, `offset+to`.
	// So length is `to - from`.
	// Java test: `slice(2, 8)`. Length = 6.
	s2 := slice.Slice(2, 8)
	encoded2 := s2.Encode()
	expected2 := []byte{0xA8}
	if !bytes.Equal(encoded2, expected2) {
		// Wait, let's verify logic.
		// common.encode of 6 bits?
		// 101010 -> 0xA8. Correct.
		t.Errorf("Expected %x, got %x", expected2, encoded2)
	}

}

func TestLeftPad(t *testing.T) {
	// Java: leftPad(8)
	// initialKey = 0xff -> 1,1,1,1,1,1,1,1
	// padded -> 0,0,0,0,0,0,0,0, 1,1,1,1,1,1,1,1
	// encode -> 0x00, 0xff.

	key := []byte{0xff}
	initial := TrieKeySliceFromKey(key)
	padded := initial.LeftPad(8)

	if padded.Length() != initial.Length()+8 {
		t.Errorf("Length mismatch")
	}

	encoded := padded.Encode()
	expected := []byte{0x00, 0xff}

	if !bytes.Equal(encoded, expected) {
		t.Errorf("Expected %x, got %x", expected, encoded)
	}
}
