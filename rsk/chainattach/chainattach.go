// Package chainattach provides a small, shared surface for pointing the
// operator and observability tools at an arbitrary already-running chain
// instead of an implicitly-local one.
//
// It unifies three things that were previously re-implemented per tool:
//
//   - the L1 contract "address book" (l1.json), loadable from a file path,
//     an http(s) URL, or stdin, with explicit per-address overrides;
//   - RPC endpoint resolution with a flag > env > local-default precedence
//     that logs a loud warning whenever it has to fall back to localhost
//     (so "accidentally attached to the local dev stack" is never silent);
//   - a chain-id readiness poll + optional assertion, lifted from
//     deploy-rollup, so a tool fails loudly when pointed at the wrong chain.
//
// It depends only on go-ethereum, rsk/trie, and the standard library — never
// on any cmd/* package — so every binary can import it without creating a
// cmd->cmd dependency.
package chainattach

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/fedejinich/rootstack-operator-tools/rsk/trie"
)

// ---------- Canonical address book ----------

// Addresses is the canonical L1 contract address book shared by the operator
// and observability tools. The fields cover every address those tools read
// from a deploy's l1.json; tools that don't need a given address simply leave
// it zero.
type Addresses struct {
	OptimismPortal      common.Address // l1.json "OptimismPortalProxy"
	DisputeGameFactory  common.Address // l1.json "DisputeGameFactoryProxy"
	AnchorStateRegistry common.Address // l1.json "AnchorStateRegistryProxy" (withdraw-l1 finalize)
	L1StandardBridge    common.Address // l1.json "L1StandardBridgeProxy" (monitor/healthcheck)
	EthLockbox          common.Address // l1.json "EthLockboxProxy" (unsafe withdrawal target)
}

// HasPortal reports whether OptimismPortal is set (non-zero).
func (a Addresses) HasPortal() bool { return a.OptimismPortal != (common.Address{}) }

// HasDGF reports whether DisputeGameFactory is set (non-zero).
func (a Addresses) HasDGF() bool { return a.DisputeGameFactory != (common.Address{}) }

// HasASR reports whether AnchorStateRegistry is set (non-zero).
func (a Addresses) HasASR() bool { return a.AnchorStateRegistry != (common.Address{}) }

// HasL1StandardBridge reports whether L1StandardBridge is set (non-zero).
func (a Addresses) HasL1StandardBridge() bool { return a.L1StandardBridge != (common.Address{}) }

// HasEthLockbox reports whether EthLockbox is set (non-zero). Deployments that
// predate the lockbox simply leave it unset.
func (a Addresses) HasEthLockbox() bool { return a.EthLockbox != (common.Address{}) }

// l1JSON is the unmarshal target for the flat top-level keys emitted in
// l1.json (the same keys rollup-monitor already reads).
type l1JSON struct {
	OptimismPortalProxy      string `json:"OptimismPortalProxy"`
	DisputeGameFactoryProxy  string `json:"DisputeGameFactoryProxy"`
	AnchorStateRegistryProxy string `json:"AnchorStateRegistryProxy"`
	L1StandardBridgeProxy    string `json:"L1StandardBridgeProxy"`
	EthLockboxProxy          string `json:"EthLockboxProxy"`
}

// ---------- Address source loading ----------

// AddressSource describes where to load l1.json from:
//
//	""  or "none"  -> no file load (rely purely on Overrides)
//	"-"            -> read l1.json content from stdin
//	"http(s)://…"  -> GET the URL; the body is l1.json
//	anything else  -> a filesystem path; a directory has "/l1.json" appended
type AddressSource string

// Overrides are explicit per-address values (hex strings; "" means unset).
// Any non-empty override wins over the value loaded from the source.
type Overrides struct {
	OptimismPortal      string // --portal
	DisputeGameFactory  string // --dgf
	AnchorStateRegistry string // --anchor-state-registry
	L1StandardBridge    string // --l1-standard-bridge
}

// LoadAddresses loads l1.json from source (if any) and then applies non-empty
// Overrides on top. A missing/empty source is not in itself an error — callers
// decide whether the resulting Addresses has everything they require. Malformed
// hex (in the source or an override) is an error.
func LoadAddresses(source AddressSource, ov Overrides) (Addresses, error) {
	var addr Addresses

	if s := strings.TrimSpace(string(source)); s != "" && s != "none" {
		raw, err := loadRaw(AddressSource(s))
		if err != nil {
			return Addresses{}, err
		}
		var j l1JSON
		if err := json.Unmarshal(raw, &j); err != nil {
			return Addresses{}, fmt.Errorf("parse l1.json from %s: %w", describeSource(s), err)
		}
		for _, f := range []struct {
			dst *common.Address
			val string
			key string
		}{
			{&addr.OptimismPortal, j.OptimismPortalProxy, "OptimismPortalProxy"},
			{&addr.DisputeGameFactory, j.DisputeGameFactoryProxy, "DisputeGameFactoryProxy"},
			{&addr.AnchorStateRegistry, j.AnchorStateRegistryProxy, "AnchorStateRegistryProxy"},
			{&addr.L1StandardBridge, j.L1StandardBridgeProxy, "L1StandardBridgeProxy"},
			{&addr.EthLockbox, j.EthLockboxProxy, "EthLockboxProxy"},
		} {
			if f.val == "" {
				continue
			}
			if !common.IsHexAddress(f.val) {
				return Addresses{}, fmt.Errorf("invalid %s in l1.json from %s: %q", f.key, describeSource(s), f.val)
			}
			*f.dst = common.HexToAddress(f.val)
		}
	}

	for _, o := range []struct {
		dst  *common.Address
		val  string
		flag string
	}{
		{&addr.OptimismPortal, ov.OptimismPortal, "--portal"},
		{&addr.DisputeGameFactory, ov.DisputeGameFactory, "--dgf"},
		{&addr.AnchorStateRegistry, ov.AnchorStateRegistry, "--anchor-state-registry"},
		{&addr.L1StandardBridge, ov.L1StandardBridge, "--l1-standard-bridge"},
	} {
		if strings.TrimSpace(o.val) == "" {
			continue
		}
		if !common.IsHexAddress(o.val) {
			return Addresses{}, fmt.Errorf("invalid %s address: %q", o.flag, o.val)
		}
		*o.dst = common.HexToAddress(o.val)
	}

	return addr, nil
}

// loadRaw fetches the raw l1.json bytes for an already-trimmed, non-empty source.
func loadRaw(source AddressSource) ([]byte, error) {
	s := string(source)
	switch {
	case s == "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read l1.json from stdin: %w", err)
		}
		return b, nil
	case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"):
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(s) //nolint:noctx // short, bounded by the client timeout
		if err != nil {
			return nil, fmt.Errorf("fetch l1.json from %s: %w", s, err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return nil, fmt.Errorf("read l1.json from %s: %w", s, err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch l1.json from %s: HTTP %d", s, resp.StatusCode)
		}
		return body, nil
	default:
		path := s
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			path = filepath.Join(path, "l1.json")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read l1.json: %w", err)
		}
		return b, nil
	}
}

func describeSource(s string) string {
	if s == "-" {
		return "stdin"
	}
	return s
}

// ---------- Endpoint resolution ----------

// Endpoints is the resolved RPC surface. Tools use whichever fields apply.
type Endpoints struct {
	L1RPC      string
	L2RPC      string
	NodeRPC    string // op-node (optimism_syncStatus)
	BatcherRPC string // batcher admin RPC
}

// LocalDefaults are the canonical last-resort localhost endpoints. 127.0.0.1
// (not "localhost") is deliberate — it sidesteps the macOS IPv6/localhost
// resolution issue the bridge tooling hit.
var LocalDefaults = Endpoints{
	L1RPC:   "http://127.0.0.1:4444",
	L2RPC:   "http://127.0.0.1:8545",
	NodeRPC: "http://127.0.0.1:9545",
}

// ResolveEndpoint applies flag > env > localDefault precedence. When it has to
// fall back to localDefault (both flag and env empty) it emits exactly one loud
// warning via warnf, so a run that silently dials the local dev stack is never a
// surprise. Pass warnf == nil to suppress the warning. localDefault is per-call,
// so each tool keeps its own sensible fallback (e.g. an RSK public node) rather
// than being forced to localhost.
func ResolveEndpoint(name, flagVal, envVal, localDefault string, warnf func(string, ...any)) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	if v := strings.TrimSpace(envVal); v != "" {
		return v
	}
	if warnf != nil {
		warnf("WARN: %s not set via flag or env; falling back to %s — pass the flag/env (or --addresses) to attach to a remote chain\n", name, localDefault)
	}
	return localDefault
}

// ---------- Chain-id readiness + assertion ----------

// WaitForChainID polls eth_chainId until the RPC responds or ~90s elapses. A
// freshly-started node may refuse connections briefly; this rides out that
// startup window instead of dying on the first dial. logf (may be nil) receives
// retry notices.
func WaitForChainID(ctx context.Context, client *ethclient.Client, endpoint string, logf func(string, ...any)) (*big.Int, error) {
	const timeout = 90 * time.Second
	const interval = 2 * time.Second
	deadline := time.Now().Add(timeout)
	for {
		chainID, err := client.ChainID(ctx)
		if err == nil {
			return chainID, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("fetch chain ID from %s: not ready after %s: %w", endpoint, timeout, err)
		}
		if logf != nil {
			logf("chain ID from %s not ready yet, retrying: %v\n", endpoint, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// AssertChainID waits for the chain id and optionally validates it:
//
//	expected != nil               -> live id MUST equal expected (else error)
//	expected == nil && requireRSK -> live id MUST be an RSK network (30/31/33)
//	expected == nil && !requireRSK -> no assertion; just return the live id
//
// Passing expected = the rollup's configured l1_chain_id turns "attached to the
// wrong endpoint" from a silent, confusing failure into an immediate error.
func AssertChainID(ctx context.Context, client *ethclient.Client, endpoint string, expected *big.Int, requireRSK bool, logf func(string, ...any)) (*big.Int, error) {
	live, err := WaitForChainID(ctx, client, endpoint, logf)
	if err != nil {
		return nil, err
	}
	if expected != nil && live.Cmp(expected) != 0 {
		return nil, fmt.Errorf("chain ID mismatch at %s: connected chain reports %s but %s was expected (wrong endpoint?)", endpoint, live, expected)
	}
	if expected == nil && requireRSK && !trie.IsRSKChain(live.Uint64()) {
		return nil, fmt.Errorf("chain at %s reports non-RSK chain ID %s (expected an RSK network: 30/31/33)", endpoint, live)
	}
	return live, nil
}
