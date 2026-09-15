package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/fedejinich/rootstack-operator-tools/cmd/rollup-monitor/monitor"
)

const (
	// setGasLimitABI is the ABI for SystemConfig.setGasLimit(uint64).
	setGasLimitABI = `[{"inputs":[{"internalType":"uint64","name":"_gasLimit","type":"uint64"}],"name":"setGasLimit","outputs":[],"stateMutability":"nonpayable","type":"function"}]`
	// getGasLimitABI is the ABI for SystemConfig.gasLimit() public getter (uint64).
	getGasLimitABI = `[{"inputs":[],"name":"gasLimit","outputs":[{"internalType":"uint64","name":"","type":"uint64"}],"stateMutability":"view","type":"function"}]`

	l2GasLimitPollInterval = 2 * time.Second
	l2GasLimitWaitTimeout  = 5 * time.Minute
)

// GasLimitRunRecord is one line in gas_limit_verification.jsonl for manual audit.
type GasLimitRunRecord struct {
	RunID              int    `json:"run_id"`
	TargetGasLimit     uint64 `json:"target_gas_limit"`
	L1GasLimitBefore   uint64 `json:"l1_gas_limit_before"`
	L2GasLimitBefore   uint64 `json:"l2_gas_limit_before"`
	TxSubmitted        bool   `json:"tx_submitted"`
	SetGasLimitTxHash  string `json:"set_gas_limit_tx_hash,omitempty"`
	L1GasLimitAfter    uint64 `json:"l1_gas_limit_after,omitempty"`
	L2GasLimitAfter    uint64 `json:"l2_gas_limit_after"`
	L2BlockWhenMatched uint64 `json:"l2_block_when_matched"`
	WaitDuration       string `json:"wait_duration"`
	RecordedAt         string `json:"recorded_at"`
	Error              string `json:"error,omitempty"`
}

// GasLimitChanger calls SystemConfig.setGasLimit() on L1 to change the L2 gas limit at runtime.
type GasLimitChanger struct {
	logger       *slog.Logger
	l1RPC        string
	systemConfig common.Address
	privateKey   *ecdsa.PrivateKey
	chainID      *big.Int
}

// NewGasLimitChanger creates a gas limit changer using the experiment's L1 RPC
// and the SystemConfigProxy address from l1.json in the workdir.
//
// The SystemConfig.setGasLimit call requires the contract owner, which is the
// address derived from master_private_key in the rollup-node config (set as
// finalSystemOwner / systemConfigOwner during deployment). If rollupNodeConfig
// is provided, master_private_key is read from it; otherwise the experiment's
// target.private_key is used as a fallback.
func NewGasLimitChanger(logger *slog.Logger, target TargetConfig, rollupNodeConfig string) (*GasLimitChanger, error) {
	addrs, err := monitor.LoadL1Contracts(target.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("load l1.json: %w", err)
	}

	sysConfigAddr := addrs["SystemConfigProxy"]
	if sysConfigAddr == "" {
		return nil, fmt.Errorf("SystemConfigProxy not found in l1.json")
	}

	pkHex, err := resolveOwnerKey(target, rollupNodeConfig)
	if err != nil {
		return nil, err
	}

	key, err := crypto.HexToECDSA(pkHex)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	client, err := ethclient.Dial(target.L1RPC)
	if err != nil {
		return nil, fmt.Errorf("dial L1 RPC: %w", err)
	}
	defer client.Close()

	chainID, err := client.ChainID(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get L1 chain ID: %w", err)
	}

	addr := crypto.PubkeyToAddress(key.PublicKey)
	logger.Info("Gas limit changer initialized",
		"system_config", sysConfigAddr,
		"owner", addr.Hex(),
	)

	return &GasLimitChanger{
		logger:       logger,
		l1RPC:        target.L1RPC,
		systemConfig: common.HexToAddress(sysConfigAddr),
		privateKey:   key,
		chainID:      chainID,
	}, nil
}

// resolveOwnerKey returns the hex-encoded private key (without 0x prefix) for
// the SystemConfig owner. It reads master_private_key from the rollup-node
// config when available, falling back to the experiment's target.private_key.
func resolveOwnerKey(target TargetConfig, rollupNodeConfig string) (string, error) {
	if rollupNodeConfig != "" {
		mk, err := readMasterKey(rollupNodeConfig)
		if err == nil && mk != "" {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(mk), "0x")), nil
		}
	}

	pk := target.PrivateKey
	if pk == "" {
		return "", fmt.Errorf("no private key available: set master_private_key in rollup-node config or target.private_key in experiment config")
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(pk), "0x")), nil
}

// rollupNodeKeyConfig is a minimal struct for extracting master_private_key
// from a rollup-node TOML config.
type rollupNodeKeyConfig struct {
	MasterPrivateKey *string `toml:"master_private_key"`
}

// readMasterKey parses just the master_private_key from a rollup-node TOML.
func readMasterKey(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read rollup-node config %s: %w", path, err)
	}
	var cfg rollupNodeKeyConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parse rollup-node config %s: %w", path, err)
	}
	if cfg.MasterPrivateKey == nil {
		return "", nil
	}
	return *cfg.MasterPrivateKey, nil
}

// CurrentGasLimit queries the latest L2 block to determine the current gas limit.
func (g *GasLimitChanger) CurrentGasLimit(ctx context.Context, l2RPC string) (uint64, error) {
	client, err := ethclient.DialContext(ctx, l2RPC)
	if err != nil {
		return 0, fmt.Errorf("dial L2: %w", err)
	}
	defer client.Close()

	block, err := client.BlockByNumber(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("get latest L2 block: %w", err)
	}
	return block.GasLimit(), nil
}

// SystemConfigGasLimit reads SystemConfig.gasLimit() on L1 via eth_call.
func (g *GasLimitChanger) SystemConfigGasLimit(ctx context.Context) (uint64, error) {
	client, err := ethclient.DialContext(ctx, g.l1RPC)
	if err != nil {
		return 0, fmt.Errorf("dial L1: %w", err)
	}
	defer client.Close()

	parsed, err := abi.JSON(strings.NewReader(getGasLimitABI))
	if err != nil {
		return 0, fmt.Errorf("parse getGasLimit ABI: %w", err)
	}
	data, err := parsed.Pack("gasLimit")
	if err != nil {
		return 0, fmt.Errorf("pack gasLimit: %w", err)
	}

	to := g.systemConfig
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
	if err != nil {
		return 0, fmt.Errorf("call SystemConfig.gasLimit: %w", err)
	}

	vals, err := parsed.Unpack("gasLimit", out)
	if err != nil {
		return 0, fmt.Errorf("unpack gasLimit: %w", err)
	}
	if len(vals) == 0 {
		return 0, fmt.Errorf("empty gasLimit return")
	}
	gl, ok := vals[0].(uint64)
	if !ok {
		return 0, fmt.Errorf("gasLimit return type %T", vals[0])
	}
	return gl, nil
}

// waitL2GasLimit polls the L2 head until block.GasLimit() == want or timeout.
func (g *GasLimitChanger) waitL2GasLimit(ctx context.Context, l2RPC string, want uint64) (blockNum uint64, waited time.Duration, err error) {
	waitCtx, cancel := context.WithTimeout(ctx, l2GasLimitWaitTimeout)
	defer cancel()

	client, err := ethclient.DialContext(waitCtx, l2RPC)
	if err != nil {
		return 0, 0, fmt.Errorf("dial L2 for gas limit wait: %w", err)
	}
	defer client.Close()

	start := time.Now()
	ticker := time.NewTicker(l2GasLimitPollInterval)
	defer ticker.Stop()

	for {
		block, err := client.BlockByNumber(waitCtx, nil)
		if err != nil {
			g.logger.Debug("L2 head fetch during gas limit wait", "err", err)
			select {
			case <-waitCtx.Done():
				return 0, time.Since(start), fmt.Errorf("wait L2 gas limit %d: %w", want, waitCtx.Err())
			case <-ticker.C:
				continue
			}
		}
		if block.GasLimit() == want {
			return block.NumberU64(), time.Since(start), nil
		}
		g.logger.Debug("L2 gas limit not yet at target",
			"have", block.GasLimit(),
			"want", want,
			"l2_block", block.NumberU64(),
		)
		select {
		case <-waitCtx.Done():
			return 0, time.Since(start), fmt.Errorf("timeout waiting for L2 gas limit %d (last have %d): %w", want, block.GasLimit(), waitCtx.Err())
		case <-ticker.C:
		}
	}
}

// SetGasLimit sends a transaction to SystemConfig.setGasLimit(newLimit) on L1
// and waits for confirmation. Returns the transaction hash.
func (g *GasLimitChanger) SetGasLimit(ctx context.Context, newLimit uint64) (common.Hash, error) {
	var zero common.Hash
	client, err := ethclient.DialContext(ctx, g.l1RPC)
	if err != nil {
		return zero, fmt.Errorf("dial L1: %w", err)
	}
	defer client.Close()

	parsed, err := abi.JSON(strings.NewReader(setGasLimitABI))
	if err != nil {
		return zero, fmt.Errorf("parse ABI: %w", err)
	}

	data, err := parsed.Pack("setGasLimit", newLimit)
	if err != nil {
		return zero, fmt.Errorf("pack setGasLimit: %w", err)
	}

	from := crypto.PubkeyToAddress(g.privateKey.PublicKey)
	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return zero, fmt.Errorf("get nonce: %w", err)
	}

	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		return zero, fmt.Errorf("suggest gas price: %w", err)
	}

	tx := types.NewTransaction(nonce, g.systemConfig, big.NewInt(0), 200_000, gasPrice, data)
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(g.chainID), g.privateKey)
	if err != nil {
		return zero, fmt.Errorf("sign tx: %w", err)
	}

	if err := client.SendTransaction(ctx, signedTx); err != nil {
		return zero, fmt.Errorf("send setGasLimit tx: %w", err)
	}

	hash := signedTx.Hash()
	g.logger.Info("setGasLimit tx sent, waiting for confirmation",
		"tx", hash.Hex(),
		"gas_limit", newLimit,
	)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	timeout := time.After(120 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-timeout:
			return zero, fmt.Errorf("timeout waiting for setGasLimit tx %s", hash.Hex())
		case <-ticker.C:
			receipt, err := client.TransactionReceipt(ctx, hash)
			if err != nil {
				continue
			}
			if receipt.Status == types.ReceiptStatusFailed {
				return zero, fmt.Errorf("setGasLimit tx reverted: %s", hash.Hex())
			}
			g.logger.Info("setGasLimit confirmed on L1",
				"tx", hash.Hex(),
				"block", receipt.BlockNumber,
				"gas_limit", newLimit,
			)
			return hash, nil
		}
	}
}

// EnsureGasLimit aligns L1 SystemConfig and L2 head with want: skips setGasLimit
// when L1 already holds want, always waits until the L2 head reports want.
func (g *GasLimitChanger) EnsureGasLimit(ctx context.Context, l2RPC string, want uint64) (*GasLimitRunRecord, error) {
	rec := &GasLimitRunRecord{
		TargetGasLimit: want,
		RecordedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}

	if want == 0 {
		return rec, fmt.Errorf("EnsureGasLimit: want must be non-zero")
	}

	l1Before, err := g.SystemConfigGasLimit(ctx)
	if err != nil {
		return rec, fmt.Errorf("read L1 SystemConfig gas limit: %w", err)
	}
	rec.L1GasLimitBefore = l1Before

	l2Before, err := g.CurrentGasLimit(ctx, l2RPC)
	if err != nil {
		return rec, fmt.Errorf("read L2 head gas limit: %w", err)
	}
	rec.L2GasLimitBefore = l2Before

	if l1Before != want {
		txHash, err := g.SetGasLimit(ctx, want)
		if err != nil {
			return rec, err
		}
		rec.TxSubmitted = true
		rec.SetGasLimitTxHash = txHash.Hex()

		l1After, err := g.SystemConfigGasLimit(ctx)
		if err != nil {
			return rec, fmt.Errorf("read L1 gas limit after setGasLimit: %w", err)
		}
		rec.L1GasLimitAfter = l1After
		if l1After != want {
			g.logger.Warn("L1 SystemConfig gas limit mismatch after setGasLimit tx",
				"have", l1After,
				"want", want,
			)
		}
	}

	if l1Before == want && l2Before == want {
		client, err := ethclient.DialContext(ctx, l2RPC)
		if err != nil {
			return rec, fmt.Errorf("dial L2 for head block: %w", err)
		}
		defer client.Close()
		block, err := client.BlockByNumber(ctx, nil)
		if err != nil {
			return rec, fmt.Errorf("get L2 head when already at target: %w", err)
		}
		rec.L2GasLimitAfter = block.GasLimit()
		rec.L2BlockWhenMatched = block.NumberU64()
		rec.WaitDuration = "0s"
		g.logger.Info("Gas limit already at target on L1 and L2",
			"gas_limit", want,
			"l2_block", rec.L2BlockWhenMatched,
		)
		return rec, nil
	}

	blockNum, waited, err := g.waitL2GasLimit(ctx, l2RPC, want)
	if err != nil {
		return rec, err
	}
	rec.L2GasLimitAfter = want
	rec.L2BlockWhenMatched = blockNum
	rec.WaitDuration = waited.String()
	g.logger.Info("L2 head reached target gas limit",
		"gas_limit", want,
		"l2_block", blockNum,
		"wait", rec.WaitDuration,
	)
	return rec, nil
}

// AppendGasLimitVerificationRecord appends one JSON line to path (JSONL).
func AppendGasLimitVerificationRecord(path string, rec *GasLimitRunRecord) error {
	if rec == nil {
		return fmt.Errorf("nil GasLimitRunRecord")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return err
	}
	return nil
}
