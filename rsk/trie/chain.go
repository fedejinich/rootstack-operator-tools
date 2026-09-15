// Package trie provides RSK-specific trie root computation and chain detection.
// It is the OP-Stack-side adapter for gorsk's Unitrie / rskblocks codecs and
// replaces the former op-geth/rsk package; the move avoids forking op-geth.
package trie

const (
	MainnetChainID = 30
	TestnetChainID = 31
	RegtestChainID = 33
)

// IsRSKChain reports whether the given chain ID is one of RSK's known networks.
func IsRSKChain(chainID uint64) bool {
	return chainID == MainnetChainID || chainID == TestnetChainID || chainID == RegtestChainID
}
