package trie

import "testing"

func TestIsRSKChain(t *testing.T) {
	cases := map[uint64]bool{
		MainnetChainID: true,
		TestnetChainID: true,
		RegtestChainID: true,
		1:              false,
		11155111:       false,
		0:              false,
	}
	for id, want := range cases {
		if got := IsRSKChain(id); got != want {
			t.Errorf("IsRSKChain(%d) = %v, want %v", id, got, want)
		}
	}
}
