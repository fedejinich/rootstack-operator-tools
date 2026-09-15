package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/fedejinich/rootstack-operator-tools/cmd/rollup-monitor/monitor"
)

// DrainConfig holds the parameters needed to sweep derived account balances.
type DrainConfig struct {
	L2RPC          string
	PrivateKeyHex  string
	NumAccounts    int
	ERC20TokenAddr string // empty = skip ERC20 sweep
}

// DrainResult summarises the outcome of a drain operation.
type DrainResult struct {
	RBTCRecovered   *big.Int
	ERC20Recovered  *big.Int
	AccountsDrained int
	AccountsSkipped int
}

const drainERC20ABI = `[{
	"inputs": [{"name": "to", "type": "address"}, {"name": "amount", "type": "uint256"}],
	"name": "transfer",
	"outputs": [{"name": "", "type": "bool"}],
	"stateMutability": "nonpayable",
	"type": "function"
},{
	"inputs": [{"name": "account", "type": "address"}],
	"name": "balanceOf",
	"outputs": [{"name": "", "type": "uint256"}],
	"stateMutability": "view",
	"type": "function"
}]`

// drainDerivedAccounts sweeps remaining RBTC (and optionally ERC20) balances
// from all derived traffic accounts back to the master account.
func drainDerivedAccounts(ctx context.Context, logger *slog.Logger, cfg DrainConfig) (*DrainResult, error) {
	if cfg.NumAccounts <= 1 {
		return &DrainResult{RBTCRecovered: big.NewInt(0), ERC20Recovered: big.NewInt(0)}, nil
	}

	pkHex := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cfg.PrivateKeyHex), "0x"))
	masterKey, err := crypto.HexToECDSA(pkHex)
	if err != nil {
		return nil, fmt.Errorf("parse master private key: %w", err)
	}
	masterAddr := crypto.PubkeyToAddress(masterKey.PublicKey)

	l2, err := ethclient.DialContext(ctx, cfg.L2RPC)
	if err != nil {
		return nil, fmt.Errorf("dial L2 RPC: %w", err)
	}
	defer l2.Close()

	chainID, err := l2.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("get chain ID: %w", err)
	}
	signer := types.NewEIP155Signer(chainID)

	gasPrice, err := l2.SuggestGasPrice(ctx)
	if err != nil {
		return nil, fmt.Errorf("suggest gas price: %w", err)
	}

	var tokenABI abi.ABI
	var tokenAddr common.Address
	sweepERC20 := cfg.ERC20TokenAddr != ""
	if sweepERC20 {
		tokenAddr = common.HexToAddress(cfg.ERC20TokenAddr)
		parsed, err := abi.JSON(strings.NewReader(drainERC20ABI))
		if err != nil {
			return nil, fmt.Errorf("parse ERC20 ABI: %w", err)
		}
		tokenABI = parsed
	}

	result := &DrainResult{
		RBTCRecovered:  big.NewInt(0),
		ERC20Recovered: big.NewInt(0),
	}

	rbtcGasCost := new(big.Int).Mul(gasPrice, big.NewInt(21000))
	erc20GasLimit := uint64(80000)

	logger.Info("Draining derived accounts",
		"master", masterAddr.Hex()[:10],
		"accounts", cfg.NumAccounts-1,
		"erc20", sweepERC20,
	)

	for i := 1; i < cfg.NumAccounts; i++ {
		childKey, childAddr := monitor.DeriveTrafficKey(masterKey, i)

		// --- ERC20 sweep (before RBTC so we still have gas) ---
		if sweepERC20 {
			erc20Bal := tokenBalanceOf(ctx, l2, tokenABI, tokenAddr, childAddr)
			if erc20Bal != nil && erc20Bal.Sign() > 0 {
				calldata, packErr := tokenABI.Pack("transfer", masterAddr, erc20Bal)
				if packErr != nil {
					logger.Warn("ERC20 pack error", "account", i, "err", packErr)
				} else {
					nonce, nErr := l2.PendingNonceAt(ctx, childAddr)
					if nErr != nil {
						logger.Warn("ERC20 nonce error", "account", i, "err", nErr)
					} else {
						tx := types.NewTransaction(nonce, tokenAddr, big.NewInt(0), erc20GasLimit, gasPrice, calldata)
						signed, sErr := types.SignTx(tx, signer, childKey)
						if sErr != nil {
							logger.Warn("ERC20 sign error", "account", i, "err", sErr)
						} else if sendErr := l2.SendTransaction(ctx, signed); sendErr != nil {
							logger.Warn("ERC20 send error", "account", i, "err", sendErr)
						} else if err := waitForReceipt(ctx, l2, signed.Hash()); err != nil {
							logger.Warn("ERC20 receipt error", "account", i, "err", err)
						} else {
							result.ERC20Recovered.Add(result.ERC20Recovered, erc20Bal)
							logger.Debug("ERC20 swept", "account", i, "addr", childAddr.Hex()[:10], "amount", erc20Bal)
						}
					}
				}
			}
		}

		// --- RBTC sweep ---
		bal, bErr := l2.BalanceAt(ctx, childAddr, nil)
		if bErr != nil {
			logger.Warn("Balance query error", "account", i, "err", bErr)
			result.AccountsSkipped++
			continue
		}

		// Need enough for the transfer gas, plus ERC20 gas if we just sent one.
		minRequired := new(big.Int).Set(rbtcGasCost)
		if bal.Cmp(minRequired) <= 0 {
			if bal.Sign() > 0 {
				logger.Debug("Skipping account with dust", "account", i, "addr", childAddr.Hex()[:10], "balance_wei", bal)
			}
			result.AccountsSkipped++
			continue
		}

		sendAmount := new(big.Int).Sub(bal, rbtcGasCost)
		nonce, nErr := l2.PendingNonceAt(ctx, childAddr)
		if nErr != nil {
			logger.Warn("Nonce error", "account", i, "err", nErr)
			result.AccountsSkipped++
			continue
		}

		tx := types.NewTransaction(nonce, masterAddr, sendAmount, 21000, gasPrice, nil)
		signed, sErr := types.SignTx(tx, signer, childKey)
		if sErr != nil {
			logger.Warn("Sign error", "account", i, "err", sErr)
			result.AccountsSkipped++
			continue
		}

		if sendErr := l2.SendTransaction(ctx, signed); sendErr != nil {
			logger.Warn("Send error", "account", i, "err", sendErr)
			result.AccountsSkipped++
			continue
		}

		if err := waitForReceipt(ctx, l2, signed.Hash()); err != nil {
			logger.Warn("Receipt error", "account", i, "err", err)
			result.AccountsSkipped++
			continue
		}

		result.RBTCRecovered.Add(result.RBTCRecovered, sendAmount)
		result.AccountsDrained++
		logger.Debug("RBTC swept", "account", i, "addr", childAddr.Hex()[:10], "amount_wei", sendAmount)
	}

	logger.Info("Drain complete",
		"rbtc_recovered_rbtc", weiToRBTCFloat(result.RBTCRecovered),
		"erc20_recovered", result.ERC20Recovered,
		"drained", result.AccountsDrained,
		"skipped", result.AccountsSkipped,
	)
	return result, nil
}

// tokenBalanceOf queries an ERC20 balanceOf via eth_call.
func tokenBalanceOf(ctx context.Context, l2 *ethclient.Client, tokenABI abi.ABI, tokenAddr, account common.Address) *big.Int {
	calldata, err := tokenABI.Pack("balanceOf", account)
	if err != nil {
		return big.NewInt(0)
	}
	result, err := l2.CallContract(ctx, ethereum.CallMsg{To: &tokenAddr, Data: calldata}, nil)
	if err != nil || len(result) < 32 {
		return big.NewInt(0)
	}
	return new(big.Int).SetBytes(result[:32])
}

// waitForReceipt polls for a transaction receipt with a timeout.
func waitForReceipt(ctx context.Context, l2 *ethclient.Client, txHash common.Hash) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(60 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for tx %s", txHash.Hex())
		case <-ticker.C:
			receipt, err := l2.TransactionReceipt(ctx, txHash)
			if err != nil {
				continue
			}
			if receipt.Status == types.ReceiptStatusFailed {
				return fmt.Errorf("tx %s reverted", txHash.Hex())
			}
			return nil
		}
	}
}
