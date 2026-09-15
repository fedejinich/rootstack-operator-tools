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

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

//go:embed contracts/Token.json
var genericTokenArtifactJSON []byte

// genericTokenSupply: 100 trillion * 10^18 — enough to seed any practical
// number of derived traffic accounts.
var genericTokenSupply = new(big.Int).Mul(big.NewInt(1e14), big.NewInt(1e18))

// AddressFromKey derives an EVM address from a hex private key (with or
// without "0x" prefix). Useful for preflight checks without dialing.
func AddressFromKey(keyHex string) (common.Address, error) {
	pk, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(keyHex), "0x"))
	if err != nil {
		return common.Address{}, err
	}
	return crypto.PubkeyToAddress(pk.PublicKey), nil
}

func genericTokenBytecode() ([]byte, error) {
	var a struct {
		Bytecode struct {
			Object string `json:"object"`
		} `json:"bytecode"`
	}
	if err := json.Unmarshal(genericTokenArtifactJSON, &a); err != nil {
		return nil, fmt.Errorf("parse Token artifact: %w", err)
	}
	raw := strings.TrimPrefix(a.Bytecode.Object, "0x")
	b, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("decode Token bytecode: %w", err)
	}
	return b, nil
}

// DeployGenericERC20 deploys a self-contained fixed-supply ERC20 (Token.sol)
// on L2 from `deployerKeyHex`, minting the entire supply to the deployer. It
// returns the new token address. No L1 bridge involvement.
//
// Use this when running L2 ERC20 traffic without a pre-existing token and
// without periodic L1→L2 deposits.
func DeployGenericERC20(ctx context.Context, l2 *ethclient.Client, deployerKeyHex string) (common.Address, error) {
	pk, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(deployerKeyHex), "0x"))
	if err != nil {
		return common.Address{}, fmt.Errorf("invalid deployer key: %w", err)
	}
	deployer := crypto.PubkeyToAddress(pk.PublicKey)

	chainID, err := l2.ChainID(ctx)
	if err != nil {
		return common.Address{}, fmt.Errorf("chainID: %w", err)
	}

	bytecode, err := genericTokenBytecode()
	if err != nil {
		return common.Address{}, err
	}
	supplyArg := make([]byte, 32)
	genericTokenSupply.FillBytes(supplyArg)
	bytecode = append(bytecode, supplyArg...)

	nonce, err := l2.PendingNonceAt(ctx, deployer)
	if err != nil {
		return common.Address{}, fmt.Errorf("nonce: %w", err)
	}
	head, err := l2.HeaderByNumber(ctx, nil)
	if err != nil {
		return common.Address{}, fmt.Errorf("header: %w", err)
	}
	tip := big.NewInt(1e6)
	baseFee := head.BaseFee
	if baseFee == nil {
		baseFee = big.NewInt(0)
	}
	feeCap := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), tip)

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: feeCap,
		Gas:       2_000_000,
		To:        nil,
		Value:     common.Big0,
		Data:      bytecode,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), pk)
	if err != nil {
		return common.Address{}, fmt.Errorf("sign: %w", err)
	}
	if err := l2.SendTransaction(ctx, signed); err != nil {
		return common.Address{}, fmt.Errorf("send deploy: %w", err)
	}

	for range 60 {
		select {
		case <-ctx.Done():
			return common.Address{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		rcpt, rErr := l2.TransactionReceipt(ctx, signed.Hash())
		if rErr != nil || rcpt == nil {
			continue
		}
		if rcpt.Status != 1 {
			return common.Address{}, fmt.Errorf("token deploy reverted: %s", signed.Hash().Hex())
		}
		return rcpt.ContractAddress, nil
	}
	return common.Address{}, fmt.Errorf("token deploy receipt timeout")
}
