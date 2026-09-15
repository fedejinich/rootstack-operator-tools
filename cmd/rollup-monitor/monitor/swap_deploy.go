package monitor

import (
	"context"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

//go:embed contracts/SwapRouter.json
var swapRouterArtifactJSON []byte

// SwapEnv is the set of contract addresses needed to run swap traffic:
// two mock ERC-20s and the router that swaps between them.
type SwapEnv struct {
	Router common.Address
	TokenA common.Address
	TokenB common.Address
}

func swapRouterArtifact() (abi.ABI, []byte, error) {
	var a struct {
		ABI      json.RawMessage `json:"abi"`
		Bytecode struct {
			Object string `json:"object"`
		} `json:"bytecode"`
	}
	if err := json.Unmarshal(swapRouterArtifactJSON, &a); err != nil {
		return abi.ABI{}, nil, fmt.Errorf("parse SwapRouter artifact: %w", err)
	}
	parsed, err := abi.JSON(strings.NewReader(string(a.ABI)))
	if err != nil {
		return abi.ABI{}, nil, fmt.Errorf("parse SwapRouter ABI: %w", err)
	}
	raw := strings.TrimPrefix(a.Bytecode.Object, "0x")
	b, err := hex.DecodeString(raw)
	if err != nil {
		return abi.ABI{}, nil, fmt.Errorf("decode SwapRouter bytecode: %w", err)
	}
	return parsed, b, nil
}

// DeploySwapEnv deploys two mock ERC-20s (TokenA, TokenB) and one
// MockSwapRouter on L2 from `deployerKeyHex`. Both tokens are minted in
// full to the deployer; half of TokenB's supply is then transferred to
// the router to seed swap reserves. Returns the three addresses.
func DeploySwapEnv(ctx context.Context, l2 *ethclient.Client, deployerKeyHex string) (SwapEnv, error) {
	pk, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(deployerKeyHex), "0x"))
	if err != nil {
		return SwapEnv{}, fmt.Errorf("invalid deployer key: %w", err)
	}
	deployer := crypto.PubkeyToAddress(pk.PublicKey)

	chainID, err := l2.ChainID(ctx)
	if err != nil {
		return SwapEnv{}, fmt.Errorf("chainID: %w", err)
	}
	signer := types.LatestSignerForChainID(chainID)

	gasPrice, err := l2.SuggestGasPrice(ctx)
	if err != nil {
		return SwapEnv{}, fmt.Errorf("gasPrice: %w", err)
	}
	if gasPrice.Sign() == 0 {
		gasPrice = big.NewInt(1_000_000)
	}

	tokenBytecode, err := genericTokenBytecode()
	if err != nil {
		return SwapEnv{}, err
	}
	supplyArg := make([]byte, 32)
	genericTokenSupply.FillBytes(supplyArg)
	tokenInit := append(append([]byte{}, tokenBytecode...), supplyArg...)

	_, routerInit, err := swapRouterArtifact()
	if err != nil {
		return SwapEnv{}, err
	}

	nonce, err := l2.PendingNonceAt(ctx, deployer)
	if err != nil {
		return SwapEnv{}, fmt.Errorf("nonce: %w", err)
	}

	deploy := func(initCode []byte, label string) (common.Address, error) {
		tx := types.NewContractCreation(nonce, big.NewInt(0), 4_000_000, gasPrice, initCode)
		signed, err := types.SignTx(tx, signer, pk)
		if err != nil {
			return common.Address{}, fmt.Errorf("%s sign: %w", label, err)
		}
		if err := l2.SendTransaction(ctx, signed); err != nil {
			return common.Address{}, fmt.Errorf("%s send: %w", label, err)
		}
		nonce++
		deadline := time.Now().Add(120 * time.Second)
		for time.Now().Before(deadline) {
			receipt, rerr := l2.TransactionReceipt(ctx, signed.Hash())
			if rerr == nil && receipt != nil {
				if receipt.Status != 1 {
					return common.Address{}, fmt.Errorf("%s deploy reverted: tx %s", label, signed.Hash().Hex())
				}
				return receipt.ContractAddress, nil
			}
			select {
			case <-ctx.Done():
				return common.Address{}, ctx.Err()
			case <-time.After(time.Second):
			}
		}
		return common.Address{}, fmt.Errorf("%s deploy: timed out waiting for receipt", label)
	}

	tokenA, err := deploy(tokenInit, "TokenA")
	if err != nil {
		return SwapEnv{}, err
	}
	tokenB, err := deploy(tokenInit, "TokenB")
	if err != nil {
		return SwapEnv{}, err
	}
	router, err := deploy(routerInit, "Router")
	if err != nil {
		return SwapEnv{}, err
	}

	// Seed router reserves with half of each token's supply so the
	// router can pay out either direction without running dry.
	tokenABI, err := abi.JSON(strings.NewReader(erc20TransferABI))
	if err != nil {
		return SwapEnv{}, fmt.Errorf("parse ERC20 ABI: %w", err)
	}
	reserve := new(big.Int).Rsh(genericTokenSupply, 1) // supply / 2
	for _, tok := range []common.Address{tokenA, tokenB} {
		data, err := tokenABI.Pack("transfer", router, reserve)
		if err != nil {
			return SwapEnv{}, fmt.Errorf("pack reserve transfer: %w", err)
		}
		tx := types.NewTransaction(nonce, tok, big.NewInt(0), 100_000, gasPrice, data)
		signed, err := types.SignTx(tx, signer, pk)
		if err != nil {
			return SwapEnv{}, fmt.Errorf("seed reserve sign: %w", err)
		}
		if err := l2.SendTransaction(ctx, signed); err != nil {
			return SwapEnv{}, fmt.Errorf("seed reserve send: %w", err)
		}
		nonce++
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			receipt, rerr := l2.TransactionReceipt(ctx, signed.Hash())
			if rerr == nil && receipt != nil {
				if receipt.Status != 1 {
					return SwapEnv{}, fmt.Errorf("seed reserve reverted: %s", signed.Hash().Hex())
				}
				break
			}
			select {
			case <-ctx.Done():
				return SwapEnv{}, ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}

	// Approve router for both tokens FROM master so master can swap
	// without per-tx reverts during the initial-funding window (when
	// only master is "ready" and the dispatcher uses it as the sender).
	// Each derived account performs the same approval at fund time
	// (see ensureAccountApprovals in swaptraffic.go).
	approveABI, err := abi.JSON(strings.NewReader(erc20ApproveABI))
	if err != nil {
		return SwapEnv{}, fmt.Errorf("parse approve ABI: %w", err)
	}
	maxApproval := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	for _, tok := range []common.Address{tokenA, tokenB} {
		data, err := approveABI.Pack("approve", router, maxApproval)
		if err != nil {
			return SwapEnv{}, fmt.Errorf("pack master approve: %w", err)
		}
		tx := types.NewTransaction(nonce, tok, big.NewInt(0), 80_000, gasPrice, data)
		signed, err := types.SignTx(tx, signer, pk)
		if err != nil {
			return SwapEnv{}, fmt.Errorf("master approve sign: %w", err)
		}
		if err := l2.SendTransaction(ctx, signed); err != nil {
			return SwapEnv{}, fmt.Errorf("master approve send: %w", err)
		}
		nonce++
		deadline := time.Now().Add(60 * time.Second)
		ok := false
		for time.Now().Before(deadline) {
			receipt, rerr := l2.TransactionReceipt(ctx, signed.Hash())
			if rerr == nil && receipt != nil {
				if receipt.Status != 1 {
					return SwapEnv{}, fmt.Errorf("master approve reverted: %s", signed.Hash().Hex())
				}
				ok = true
				break
			}
			select {
			case <-ctx.Done():
				return SwapEnv{}, ctx.Err()
			case <-time.After(time.Second):
			}
		}
		if !ok {
			return SwapEnv{}, fmt.Errorf("master approve receipt timeout")
		}
	}

	return SwapEnv{Router: router, TokenA: tokenA, TokenB: tokenB}, nil
}
