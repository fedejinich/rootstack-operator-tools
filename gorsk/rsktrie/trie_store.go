package rsktrie

import (
	"encoding/hex"
)

type TrieStore interface {
	Save(t *Trie)
	Retrieve(hash []byte) *Trie
	RetrieveValue(hash []byte) []byte
}

type MemTrieStore struct {
	nodes  map[string]*Trie
	values map[string][]byte
}

func NewMemTrieStore() *MemTrieStore {
	return &MemTrieStore{
		nodes:  make(map[string]*Trie),
		values: make(map[string][]byte),
	}
}

func (s *MemTrieStore) Save(t *Trie) {
	if t == nil {
		return
	}
	hash := t.GetHash()
	key := hex.EncodeToString(hash)
	s.nodes[key] = t
	t.saved = true
}

func (s *MemTrieStore) Retrieve(hash []byte) *Trie {
	if hash == nil {
		return nil
	}
	key := hex.EncodeToString(hash)
	return s.nodes[key]
}

func (s *MemTrieStore) RetrieveValue(hash []byte) []byte {
	if hash == nil {
		return nil
	}
	key := hex.EncodeToString(hash)
	val := s.values[key]
	// Return copy?
	if val == nil {
		return nil
	}
	dst := make([]byte, len(val))
	copy(dst, val)
	return dst
}

func (s *MemTrieStore) AddValue(hash []byte, val []byte) {
	key := hex.EncodeToString(hash)
	v := make([]byte, len(val))
	copy(v, val)
	s.values[key] = v
}
