package rskblocks

import (
	"errors"
	"testing"
)

func TestConfigForBlockNumberUnknownNetwork(t *testing.T) {
	for _, network := range []string{"", "mainet", "MAINNET", "rsk", "sepolia"} {
		t.Run(network, func(t *testing.T) {
			cfg, err := ConfigForBlockNumber(9000000, network)
			if !errors.Is(err, ErrUnsupportedNetwork) {
				t.Fatalf("network %q: got err %v, want ErrUnsupportedNetwork", network, err)
			}
			if cfg != (BlockHashConfig{}) {
				t.Errorf("network %q: config must be zero on error, got %+v", network, cfg)
			}
		})
	}
}

func TestConfigForBlockNumberOrchidTrieOutOfRange(t *testing.T) {
	for _, blockNum := range []int64{0, 1, 728999, 729000, 1590999} {
		cfg, err := ConfigForBlockNumber(blockNum, "mainnet")
		if !errors.Is(err, ErrUnsupportedBlockRange) {
			t.Fatalf("mainnet block %d: got err %v, want ErrUnsupportedBlockRange", blockNum, err)
		}
		if cfg != (BlockHashConfig{}) {
			t.Errorf("mainnet block %d: config must be zero on error, got %+v", blockNum, cfg)
		}
	}

	cfg, err := ConfigForBlockNumber(mainnetWasabi100, "mainnet")
	if err != nil {
		t.Fatalf("mainnet block %d (wasabi100): unexpected error %v", mainnetWasabi100, err)
	}
	if cfg.Version != 0 || cfg.Use4ByteGasLimit {
		t.Errorf("mainnet wasabi100 config = %+v, want V0 with legacy gasLimit flag clear", cfg)
	}
}

func TestConfigForBlockNumberRegtestGenesisFailsClosed(t *testing.T) {
	cfg, err := ConfigForBlockNumber(0, "regtest")
	if !errors.Is(err, ErrUnsupportedBlockRange) {
		t.Fatalf("regtest block 0: got err %v, want ErrUnsupportedBlockRange", err)
	}
	if cfg != (BlockHashConfig{}) {
		t.Errorf("regtest block 0: config must be zero on error, got %+v", cfg)
	}

	cfg, err = ConfigForBlockNumber(regtestVersioned, "regtest")
	if err != nil {
		t.Fatalf("regtest block %d: unexpected error %v", regtestVersioned, err)
	}
	if cfg.Version != 2 || !cfg.IncludeUmmRoot {
		t.Errorf("regtest block %d config = %+v, want V2 with ummRoot", regtestVersioned, cfg)
	}

	if _, err := ConfigForBlockNumber(0, "testnet"); err != nil {
		t.Errorf("testnet block 0: unexpected error %v", err)
	}
}
