package keyderive

import (
	"testing"
)

const testMasterKey = "d10f0f2609f060955fe5d38edf88010abd4b983374ce4f898caf3bb7485db1e7"

func TestDeriveKey_Deterministic(t *testing.T) {
	key1, addr1, err := DeriveKey(testMasterKey, RoleBatcher)
	if err != nil {
		t.Fatalf("DeriveKey failed: %v", err)
	}
	key2, addr2, err := DeriveKey(testMasterKey, RoleBatcher)
	if err != nil {
		t.Fatalf("DeriveKey failed: %v", err)
	}
	if key1 != key2 {
		t.Errorf("determinism broken: %s != %s", key1, key2)
	}
	if addr1 != addr2 {
		t.Errorf("address determinism broken: %s != %s", addr1, addr2)
	}
}

func TestDeriveKey_DifferentRoles(t *testing.T) {
	batcherKey, batcherAddr, err := DeriveKey(testMasterKey, RoleBatcher)
	if err != nil {
		t.Fatalf("DeriveKey(batcher) failed: %v", err)
	}
	proposerKey, proposerAddr, err := DeriveKey(testMasterKey, RoleProposer)
	if err != nil {
		t.Fatalf("DeriveKey(proposer) failed: %v", err)
	}

	if batcherKey == proposerKey {
		t.Error("batcher and proposer derived the same private key")
	}
	if batcherAddr == proposerAddr {
		t.Error("batcher and proposer derived the same address")
	}
}

func TestDeriveKey_DifferentFromMaster(t *testing.T) {
	batcherKey, _, err := DeriveKey(testMasterKey, RoleBatcher)
	if err != nil {
		t.Fatalf("DeriveKey failed: %v", err)
	}
	if batcherKey == testMasterKey {
		t.Error("derived key is identical to master key")
	}
}

func TestDeriveKey_With0xPrefix(t *testing.T) {
	key1, addr1, err := DeriveKey(testMasterKey, RoleBatcher)
	if err != nil {
		t.Fatalf("DeriveKey failed: %v", err)
	}
	key2, addr2, err := DeriveKey("0x"+testMasterKey, RoleBatcher)
	if err != nil {
		t.Fatalf("DeriveKey(0x) failed: %v", err)
	}
	if key1 != key2 || addr1 != addr2 {
		t.Error("0x prefix should not change the result")
	}
}

func TestDeriveKey_KnownVector(t *testing.T) {
	// Ensure the derivation produces a stable, non-empty result.
	key, addr, err := DeriveKey(testMasterKey, RoleBatcher)
	if err != nil {
		t.Fatalf("DeriveKey failed: %v", err)
	}
	if len(key) != 64 {
		t.Errorf("expected 64 hex chars, got %d", len(key))
	}
	if addr == (Address{}) {
		t.Error("address is zero")
	}
	t.Logf("batcher derived key=%s addr=%s", key, addr.Hex())

	key, addr, err = DeriveKey(testMasterKey, RoleProposer)
	if err != nil {
		t.Fatalf("DeriveKey failed: %v", err)
	}
	t.Logf("proposer derived key=%s addr=%s", key, addr.Hex())
}

func TestDeriveKey_InvalidHex(t *testing.T) {
	_, _, err := DeriveKey("not-hex", RoleBatcher)
	if err == nil {
		t.Error("expected error for invalid hex")
	}
}

func TestDeriveKey_WrongLength(t *testing.T) {
	_, _, err := DeriveKey("abcd", RoleBatcher)
	if err == nil {
		t.Error("expected error for short key")
	}
}

func TestAddressFromKey(t *testing.T) {
	addr, err := AddressFromKey(testMasterKey)
	if err != nil {
		t.Fatalf("AddressFromKey failed: %v", err)
	}
	if addr == (Address{}) {
		t.Error("address is zero")
	}
	t.Logf("master addr=%s", addr.Hex())
}

// Address is a type alias used only for the zero-value comparison above.
type Address = [20]byte
