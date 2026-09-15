package rsktrie

import (
	"bytes"
	"testing"
)

// TestFromMessage_LongSharedPath_RoundtripsValue exercises the
// `lengthByte == 255` branch in deserializeSharedPath (shared paths longer
// than 382 bits, encoded with a VarInt length prefix).
//
// The previous implementation drained the *caller's* bytes.Reader to parse
// the VarInt and then only reassigned the local copy, so any bytes after
// the encoded path (here: the inline value) were silently dropped. The
// roundtrip used to come back with an empty value.
func TestFromMessage_LongSharedPath_RoundtripsValue(t *testing.T) {
	// 50 bytes = 400 bits of shared path -> well above the 382-bit
	// threshold that forces the VarInt-prefixed length encoding.
	rawKey := make([]byte, 50)
	for i := range rawKey {
		rawKey[i] = byte(i + 1)
	}
	sharedPath := TrieKeySliceFromKey(rawKey)
	if sharedPath.Length() <= 382 {
		t.Fatalf("test setup: sharedPath length %d does not exceed 382 bits", sharedPath.Length())
	}

	wantValue := []byte("inline-value-after-long-path")

	original := NewTrieFull(
		NewMemTrieStore(),
		sharedPath,
		wantValue,
		NodeReferenceEmpty(),
		NodeReferenceEmpty(),
		Uint24(len(wantValue)),
		nil,
		&VarInt{Value: 0, Size: 1},
	)

	message := original.ToMessage()

	decoded, err := FromMessage(message, NewMemTrieStore())
	if err != nil {
		t.Fatalf("FromMessage: %v", err)
	}

	gotValue := decoded.GetValue()
	if !bytes.Equal(gotValue, wantValue) {
		t.Fatalf("value after long-shared-path roundtrip: want %q, got %q", wantValue, gotValue)
	}

	if got := decoded.GetSharedPath().Length(); got != sharedPath.Length() {
		t.Fatalf("shared path length after roundtrip: want %d, got %d", sharedPath.Length(), got)
	}
}
