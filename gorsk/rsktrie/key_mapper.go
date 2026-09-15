package rsktrie

import (
	"github.com/ethereum/go-ethereum/common"
)

const (
	SecureKeySize    = 10
	AddressKeySize   = 20
	SecureAccountKey = SecureKeySize + AddressKeySize
)

var (
	DomainPrefix  = []byte{0x00}
	StoragePrefix = []byte{0x00} // MSB 0 for branching
	CodePrefix    = []byte{0x80} // MSB 1 for branching
)

// TrieKeyMapper generates trie keys for accounts and storage in RSK's unified trie
type TrieKeyMapper struct{}

func NewTrieKeyMapper() *TrieKeyMapper {
	return &TrieKeyMapper{}
}

// GetAccountKey generates the trie key for an account address
// Format: DomainPrefix + SecureKeyPrefix(address) + address
func (m *TrieKeyMapper) GetAccountKey(addr common.Address) []byte {
	securePrefix := m.SecureKeyPrefix(addr.Bytes())
	result := make([]byte, 0, len(DomainPrefix)+len(securePrefix)+len(addr))
	result = append(result, DomainPrefix...)
	result = append(result, securePrefix...)
	result = append(result, addr.Bytes()...)
	return result
}

// GetCodeKey generates the trie key for contract code
// Format: AccountKey + CodePrefix
func (m *TrieKeyMapper) GetCodeKey(addr common.Address) []byte {
	accountKey := m.GetAccountKey(addr)
	result := make([]byte, 0, len(accountKey)+len(CodePrefix))
	result = append(result, accountKey...)
	result = append(result, CodePrefix...)
	return result
}

// GetAccountStoragePrefixKey generates the prefix for storage keys
// Format: AccountKey + StoragePrefix
func (m *TrieKeyMapper) GetAccountStoragePrefixKey(addr common.Address) []byte {
	accountKey := m.GetAccountKey(addr)
	result := make([]byte, 0, len(accountKey)+len(StoragePrefix))
	result = append(result, accountKey...)
	result = append(result, StoragePrefix...)
	return result
}

// GetAccountStorageKey generates the full trie key for a storage slot
// Format: StoragePrefixKey + SecureKeyPrefix(storageKey) + stripLeadingZeros(storageKey)
func (m *TrieKeyMapper) GetAccountStorageKey(addr common.Address, storageKey common.Hash) []byte {
	prefixKey := m.GetAccountStoragePrefixKey(addr)
	securePrefix := m.SecureKeyPrefix(storageKey.Bytes())
	strippedKey := stripLeadingZeros(storageKey.Bytes())

	result := make([]byte, 0, len(prefixKey)+len(securePrefix)+len(strippedKey))
	result = append(result, prefixKey...)
	result = append(result, securePrefix...)
	result = append(result, strippedKey...)
	return result
}

// SecureKeyPrefix returns the first 10 bytes of keccak256(key)
func (m *TrieKeyMapper) SecureKeyPrefix(key []byte) []byte {
	hash := Keccak256(key)
	return hash[:SecureKeySize]
}

// stripLeadingZeros removes leading zero bytes from a byte slice
func stripLeadingZeros(data []byte) []byte {
	for i := 0; i < len(data); i++ {
		if data[i] != 0 {
			return data[i:]
		}
	}
	// If all zeros, return empty slice (or single zero byte depending on RSK behavior)
	return []byte{}
}
