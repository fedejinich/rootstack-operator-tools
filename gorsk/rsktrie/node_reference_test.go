package rsktrie

import "testing"

// TestNodeReference_GetNode_NilStore guards against a nil-pointer panic in
// NodeReference.GetNode when the reference was constructed with a nil store
// (e.g. a verify-only caller that deserialized a proof node without a backing
// store). The reference still has a lazyHash; previously GetNode would
// unconditionally call n.store.Retrieve(...) and panic.
func TestNodeReference_GetNode_NilStore(t *testing.T) {
	ref := NewNodeReference(nil, nil, []byte{0x01, 0x02, 0x03})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GetNode panicked with nil store: %v", r)
		}
	}()

	got := ref.GetNode()
	if got != nil {
		t.Fatalf("GetNode with nil store: want nil, got %v", got)
	}
}
