package chainattach

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	portalHex = "0x3F1C7014815b7d1e40349b9304D56e691DAC2F35"
	dgfHex    = "0x2a2D560a82EeF9Bf9969789dAbeF7d7A87491C14"
	asrHex    = "0x00000000000000000000000000000000000000aa"
	bridgeHex = "0x00000000000000000000000000000000000000bb"
)

const l1JSONBody = `{
  "OptimismPortalProxy": "` + portalHex + `",
  "DisputeGameFactoryProxy": "` + dgfHex + `",
  "L1StandardBridgeProxy": "` + bridgeHex + `"
}`

func TestLoadAddresses_FromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "l1.json")
	if err := os.WriteFile(path, []byte(l1JSONBody), 0o644); err != nil {
		t.Fatal(err)
	}

	// Explicit file path.
	got, err := LoadAddresses(AddressSource(path), Overrides{})
	if err != nil {
		t.Fatalf("LoadAddresses(file): %v", err)
	}
	if got.OptimismPortal != common.HexToAddress(portalHex) {
		t.Errorf("portal = %s, want %s", got.OptimismPortal, portalHex)
	}
	if got.DisputeGameFactory != common.HexToAddress(dgfHex) {
		t.Errorf("dgf = %s, want %s", got.DisputeGameFactory, dgfHex)
	}
	if got.HasASR() {
		t.Errorf("ASR should be zero (absent from l1.json), got %s", got.AnchorStateRegistry)
	}
	if got.HasEthLockbox() {
		t.Errorf("EthLockbox should be zero (absent from l1.json), got %s", got.EthLockbox)
	}
	if !got.HasPortal() || !got.HasDGF() || !got.HasL1StandardBridge() {
		t.Errorf("Has* predicates wrong for %+v", got)
	}

	// Directory source should auto-append l1.json.
	gotDir, err := LoadAddresses(AddressSource(dir), Overrides{})
	if err != nil {
		t.Fatalf("LoadAddresses(dir): %v", err)
	}
	if gotDir != got {
		t.Errorf("dir source %+v != file source %+v", gotDir, got)
	}
}

// The deployer emits EthLockboxProxy, and OptimismPortal2 treats the lockbox as an
// unsafe withdrawal target, so tools need to be able to read it back.
func TestLoadAddresses_EthLockbox(t *testing.T) {
	const lockboxHex = "0x00000000000000000000000000000000000000cc"
	dir := t.TempDir()
	path := filepath.Join(dir, "l1.json")
	body := `{
  "OptimismPortalProxy": "` + portalHex + `",
  "EthLockboxProxy": "` + lockboxHex + `"
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAddresses(AddressSource(path), Overrides{})
	if err != nil {
		t.Fatalf("LoadAddresses: %v", err)
	}
	if !got.HasEthLockbox() || got.EthLockbox != common.HexToAddress(lockboxHex) {
		t.Errorf("EthLockbox = %s, want %s", got.EthLockbox, lockboxHex)
	}
}

func TestLoadAddresses_OverridesWinAndFillGaps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "l1.json")
	if err := os.WriteFile(path, []byte(l1JSONBody), 0o644); err != nil {
		t.Fatal(err)
	}
	// Override portal, and supply ASR which the file lacks (the withdraw-l1 win).
	newPortal := "0x000000000000000000000000000000000000bEEF"
	got, err := LoadAddresses(AddressSource(path), Overrides{
		OptimismPortal:      newPortal,
		AnchorStateRegistry: asrHex,
	})
	if err != nil {
		t.Fatalf("LoadAddresses: %v", err)
	}
	if got.OptimismPortal != common.HexToAddress(newPortal) {
		t.Errorf("override did not win: portal = %s, want %s", got.OptimismPortal, newPortal)
	}
	if got.AnchorStateRegistry != common.HexToAddress(asrHex) {
		t.Errorf("ASR override not applied: %s", got.AnchorStateRegistry)
	}
	if got.DisputeGameFactory != common.HexToAddress(dgfHex) {
		t.Errorf("dgf should come from file: %s", got.DisputeGameFactory)
	}
}

func TestLoadAddresses_NoSourceOnlyOverrides(t *testing.T) {
	got, err := LoadAddresses("", Overrides{OptimismPortal: portalHex, DisputeGameFactory: dgfHex})
	if err != nil {
		t.Fatalf("LoadAddresses(none): %v", err)
	}
	if !got.HasPortal() || !got.HasDGF() {
		t.Errorf("overrides-only load failed: %+v", got)
	}

	// "none" is treated the same as empty.
	if _, err := LoadAddresses("none", Overrides{}); err != nil {
		t.Errorf("LoadAddresses(\"none\"): %v", err)
	}
}

func TestLoadAddresses_MalformedOverride(t *testing.T) {
	if _, err := LoadAddresses("", Overrides{OptimismPortal: "0xnothex"}); err == nil {
		t.Fatal("expected error for malformed override, got nil")
	}
}

func TestLoadAddresses_MalformedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "l1.json")
	if err := os.WriteFile(path, []byte(`{"OptimismPortalProxy":"0xnothex"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAddresses(AddressSource(path), Overrides{}); err == nil {
		t.Fatal("expected error for malformed address in l1.json, got nil")
	}
}

func TestLoadAddresses_FromURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(l1JSONBody))
	}))
	defer srv.Close()

	got, err := LoadAddresses(AddressSource(srv.URL), Overrides{})
	if err != nil {
		t.Fatalf("LoadAddresses(url): %v", err)
	}
	if got.OptimismPortal != common.HexToAddress(portalHex) {
		t.Errorf("url portal = %s, want %s", got.OptimismPortal, portalHex)
	}
}

func TestLoadAddresses_URLNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := LoadAddresses(AddressSource(srv.URL), Overrides{}); err == nil {
		t.Fatal("expected error for HTTP 404, got nil")
	}
}

func TestResolveEndpoint(t *testing.T) {
	const def = "http://127.0.0.1:8545"

	if got := ResolveEndpoint("L2 RPC", "http://flag", "http://env", def, failWarn(t)); got != "http://flag" {
		t.Errorf("flag should win: %s", got)
	}
	if got := ResolveEndpoint("L2 RPC", "", "http://env", def, failWarn(t)); got != "http://env" {
		t.Errorf("env should win when flag empty: %s", got)
	}

	// Fallback path: must return the default AND fire exactly one warning.
	warns := 0
	got := ResolveEndpoint("L2 RPC", "", "", def, func(string, ...any) { warns++ })
	if got != def {
		t.Errorf("fallback = %s, want %s", got, def)
	}
	if warns != 1 {
		t.Errorf("expected exactly 1 warning on fallback, got %d", warns)
	}
}

// failWarn returns a warnf that fails the test if invoked — used for branches
// that must NOT fall back to the local default.
func failWarn(t *testing.T) func(string, ...any) {
	t.Helper()
	return func(f string, a ...any) {
		t.Fatalf("unexpected fallback warning: "+f, a...)
	}
}

// chainIDServer returns a JSON-RPC server that answers eth_chainId with id.
func chainIDServer(t *testing.T, id int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"0x%x"}`, id)
	}))
}

func TestAssertChainID(t *testing.T) {
	srv := chainIDServer(t, 31) // RSK testnet
	defer srv.Close()
	client, err := ethclient.Dial(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// expected matches.
	if _, err := AssertChainID(ctx, client, srv.URL, big.NewInt(31), false, nil); err != nil {
		t.Errorf("expected-match should pass: %v", err)
	}
	// expected mismatch -> error.
	if _, err := AssertChainID(ctx, client, srv.URL, big.NewInt(1), false, nil); err == nil {
		t.Error("expected mismatch error, got nil")
	}
	// requireRSK passes for chain 31.
	if _, err := AssertChainID(ctx, client, srv.URL, nil, true, nil); err != nil {
		t.Errorf("requireRSK should pass for chain 31: %v", err)
	}
}

func TestAssertChainID_RequireRSKFails(t *testing.T) {
	srv := chainIDServer(t, 1) // Ethereum mainnet — not RSK
	defer srv.Close()
	client, err := ethclient.Dial(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AssertChainID(context.Background(), client, srv.URL, nil, true, nil); err == nil {
		t.Error("requireRSK should fail for chain 1, got nil")
	}
}
