// Package keyderive provides deterministic private-key derivation from a
// master key and a role name. It is used by deploy-rollup (to set on-chain
// role addresses) and rollup-node (to derive signing keys at runtime) so
// that batcher, proposer, and other roles each get their own nonce space
// on L1 without requiring the operator to manage multiple secrets.
package keyderive

import (
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Well-known role names.
const (
	RoleBatcher    = "batcher"
	RoleProposer   = "proposer"
	RoleChallenger = "challenger"
	RoleDeployer   = "deployer"
)

// DeriveKey deterministically derives a child private key for the given role
// from a master private key. The derivation is:
//
//	childKeyBytes = keccak256(masterKeyBytes || role)
//
// masterKeyHex may have an optional "0x" prefix.
//
// Returns the derived private key (hex, no 0x prefix) and its address.
func DeriveKey(masterKeyHex string, role string) (privateKeyHex string, addr common.Address, err error) {
	raw := strings.TrimPrefix(masterKeyHex, "0x")
	masterBytes, err := hex.DecodeString(raw)
	if err != nil {
		return "", common.Address{}, fmt.Errorf("invalid master key hex: %w", err)
	}
	if len(masterBytes) != 32 {
		return "", common.Address{}, fmt.Errorf("master key must be 32 bytes, got %d", len(masterBytes))
	}

	// keccak256(masterKeyBytes || role)
	input := append(masterBytes, []byte(role)...)
	childBytes := crypto.Keccak256(input)

	childKey, err := crypto.ToECDSA(childBytes)
	if err != nil {
		return "", common.Address{}, fmt.Errorf("derived key is not a valid secp256k1 private key: %w", err)
	}

	childHex := hex.EncodeToString(crypto.FromECDSA(childKey))
	childAddr := crypto.PubkeyToAddress(childKey.PublicKey)
	return childHex, childAddr, nil
}

// DeriveECDSA is like DeriveKey but returns the raw *ecdsa.PrivateKey.
func DeriveECDSA(masterKeyHex string, role string) (*ecdsa.PrivateKey, common.Address, error) {
	raw := strings.TrimPrefix(masterKeyHex, "0x")
	masterBytes, err := hex.DecodeString(raw)
	if err != nil {
		return nil, common.Address{}, fmt.Errorf("invalid master key hex: %w", err)
	}
	if len(masterBytes) != 32 {
		return nil, common.Address{}, fmt.Errorf("master key must be 32 bytes, got %d", len(masterBytes))
	}

	input := append(masterBytes, []byte(role)...)
	childBytes := crypto.Keccak256(input)

	childKey, err := crypto.ToECDSA(childBytes)
	if err != nil {
		return nil, common.Address{}, fmt.Errorf("derived key is not a valid secp256k1 private key: %w", err)
	}

	return childKey, crypto.PubkeyToAddress(childKey.PublicKey), nil
}

// AddressFromKey returns the Ethereum address for a hex-encoded private key.
func AddressFromKey(privateKeyHex string) (common.Address, error) {
	raw := strings.TrimPrefix(privateKeyHex, "0x")
	keyBytes, err := hex.DecodeString(raw)
	if err != nil {
		return common.Address{}, fmt.Errorf("invalid key hex: %w", err)
	}
	key, err := crypto.ToECDSA(keyBytes)
	if err != nil {
		return common.Address{}, fmt.Errorf("invalid private key: %w", err)
	}
	return crypto.PubkeyToAddress(key.PublicKey), nil
}
