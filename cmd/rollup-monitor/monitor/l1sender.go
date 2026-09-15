package monitor

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// L1Sender handles sending deposits (L1->L2) and withdrawals (L2->L1).
type L1Sender struct {
	ctx     context.Context
	cfg     *Config
	metrics *MetricsStore
	l1      *ethclient.Client
	l2      *ethclient.Client
	l2RPC   *rpc.Client // raw RPC client for gethclient (eth_getProof)

	bridgeAmountWei *big.Int
}

// NewL1Sender creates a new L1Sender connected to L1 and L2 RPCs.
func NewL1Sender(ctx context.Context, cfg *Config, metrics *MetricsStore) (*L1Sender, error) {
	if cfg.PrivateKey == "" {
		return nil, fmt.Errorf("private key required for L1 sender")
	}
	if _, err := crypto.HexToECDSA(cfg.PrivateKey); err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}
	if cfg.Rollup == nil {
		return nil, fmt.Errorf("rollup config required for L1 sender (need deposit contract address)")
	}

	l1, err := ethclient.DialContext(ctx, cfg.L1RPC)
	if err != nil {
		return nil, fmt.Errorf("dial L1 for sender: %w", err)
	}

	l2, err := ethclient.DialContext(ctx, cfg.L2RPC)
	if err != nil {
		l1.Close()
		return nil, fmt.Errorf("dial L2 for sender: %w", err)
	}

	l2RPC, err := rpc.DialContext(ctx, cfg.L2RPC)
	if err != nil {
		l1.Close()
		l2.Close()
		return nil, fmt.Errorf("dial L2 RPC for sender: %w", err)
	}

	// Parse bridge amount
	amount := parseBridgeAmount(cfg.BridgeAmountRBTC)

	return &L1Sender{
		ctx:             ctx,
		cfg:             cfg,
		metrics:         metrics,
		l1:              l1,
		l2:              l2,
		l2RPC:           l2RPC,
		bridgeAmountWei: amount,
	}, nil
}

// SetBridgeAmountRBTC updates the RBTC bridge amount from a string like "0.05".
// This is used for one-time command overrides before dispatching a deposit/withdraw.
func (s *L1Sender) SetBridgeAmountRBTC(amountStr string) {
	s.bridgeAmountWei = parseBridgeAmount(amountStr)
}

// SetBridgeAmountUSDRIF updates the USDRIF bridge amount in the config.
// This is used for one-time command overrides before dispatching a USDRIF deposit/withdraw.
func (s *L1Sender) SetBridgeAmountUSDRIF(amountStr string) {
	s.cfg.BridgeAmountUSDRIF = amountStr
}

// Address returns the EOA address derived from the configured private key.
func (s *L1Sender) Address() common.Address {
	pk, err := crypto.HexToECDSA(s.cfg.PrivateKey)
	if err != nil {
		return common.Address{}
	}
	return crypto.PubkeyToAddress(pk.PublicKey)
}

// Deposit sends a native RBTC deposit from L1 to L2 via OptimismPortal.
func (s *L1Sender) Deposit() {
	eventID := s.metrics.AddEvent("deposit-rbtc", weiToRBTCStr(s.bridgeAmountWei)+" RBTC")

	pk, err := crypto.HexToECDSA(s.cfg.PrivateKey)
	if err != nil {
		s.metrics.AppendLog("[deposit] key error: %v", err)
		s.metrics.UpdateEvent(eventID, EventFailed, "key error")
		return
	}

	from := crypto.PubkeyToAddress(pk.PublicKey)
	portal := s.cfg.Rollup.DepositContract()

	s.metrics.AppendLog("[deposit] sending %s RBTC from %s to portal %s...",
		weiToRBTCStr(s.bridgeAmountWei), from.Hex()[:10], portal.Hex()[:10])

	chainID, err := s.l1.ChainID(s.ctx)
	if err != nil {
		s.metrics.AppendLog("[deposit] chainID error: %v", err)
		s.metrics.UpdateEvent(eventID, EventFailed, "chainID error")
		return
	}

	nonce, err := s.l1.PendingNonceAt(s.ctx, from)
	if err != nil {
		s.metrics.AppendLog("[deposit] nonce error: %v", err)
		s.metrics.UpdateEvent(eventID, EventFailed, "nonce error")
		return
	}

	gasPrice, err := s.l1GasPrice()
	if err != nil {
		s.metrics.AppendLog("[deposit] gasPrice error: %v", err)
		s.metrics.UpdateEvent(eventID, EventFailed, "gasPrice error")
		return
	}

	s.metrics.AppendLog("[deposit] nonce=%d gasPrice=%s", nonce, gasPrice)
	tx := types.NewTransaction(nonce, portal, s.bridgeAmountWei, 200_000, gasPrice, nil)
	signed, err := types.SignTx(tx, types.NewEIP155Signer(chainID), pk)
	if err != nil {
		s.metrics.AppendLog("[deposit] sign error: %v", err)
		s.metrics.UpdateEvent(eventID, EventFailed, "sign error")
		return
	}

	if err := s.l1.SendTransaction(s.ctx, signed); err != nil {
		s.metrics.AppendLog("[deposit] send error: %v", err)
		s.metrics.UpdateEvent(eventID, EventFailed, "send error")
		return
	}

	s.metrics.UpdateEventTxHash(eventID, signed.Hash().Hex())
	s.metrics.UpdateEvent(eventID, EventPending, "confirming on L1")
	s.metrics.AppendLog("[deposit] tx sent %s, waiting for L1 receipt...", signed.Hash().Hex()[:10])

	receipt := s.waitForL1Receipt(signed.Hash(), "deposit")
	if receipt == nil {
		s.metrics.UpdateEvent(eventID, EventFailed, "receipt timeout")
		return
	}

	if receipt.Status != types.ReceiptStatusSuccessful {
		s.metrics.AppendLog("[deposit] REVERTED at L1 block #%d", receipt.BlockNumber.Uint64())
		s.metrics.UpdateEvent(eventID, EventFailed, fmt.Sprintf("reverted at L1 #%d", receipt.BlockNumber.Uint64()))
		return
	}

	gasCost := receiptGasCost(receipt)
	s.metrics.AppendLog("[deposit] COMPLETE! %s RBTC deposited at L1 #%d. Gas cost: %s RBTC",
		weiToRBTCStr(s.bridgeAmountWei), receipt.BlockNumber.Uint64(), weiToRBTCStr(gasCost))
	s.metrics.SetDepositCost(weiToFloat(gasCost))
	s.metrics.UpdateEvent(eventID, EventComplete, fmt.Sprintf("L1 #%d", receipt.BlockNumber.Uint64()))
}

// DepositUSDRIF performs an ERC20 deposit of USDRIF from L1 to L2 via L1StandardBridge.
// Flow: ensure L2 token exists (auto-deploy via factory if needed), approve L1StandardBridge
// to spend USDRIF, then call depositERC20.
func (s *L1Sender) DepositUSDRIF() {
	if s.cfg.USDRIFTokenL1 == "" {
		s.metrics.AppendLog("[usdrif] USDRIF L1 token address not configured (usdrif_token_l1)")
		return
	}

	amountStr := s.cfg.BridgeAmountUSDRIF
	if amountStr == "" {
		amountStr = "0.001"
	}
	eventID := s.metrics.AddEvent("deposit-usdrif", amountStr+" USDRIF")

	pk, err := crypto.HexToECDSA(s.cfg.PrivateKey)
	if err != nil {
		s.metrics.AppendLog("[usdrif] key error: %v", err)
		s.metrics.UpdateEvent(eventID, EventFailed, "key error")
		return
	}
	from := crypto.PubkeyToAddress(pk.PublicKey)
	l1Token := common.HexToAddress(s.cfg.USDRIFTokenL1)

	// Auto-deploy L2 USDRIF token via OptimismMintableERC20Factory if needed
	l2Token, err := s.ensureUSDRIFL2Token(from, l1Token)
	if err != nil {
		s.metrics.AppendLog("[usdrif] L2 token deploy failed: %v", err)
		return
	}

	// Parse bridge amount (USDRIF has 18 decimals like RBTC)
	amount := parseBridgeAmount(s.cfg.BridgeAmountUSDRIF)
	if amount.Sign() <= 0 {
		amount = big.NewInt(1e15) // 0.001 USDRIF default
	}

	l1ChainID, err := s.l1.ChainID(s.ctx)
	if err != nil {
		s.metrics.AppendLog("[usdrif] chainID error: %v", err)
		return
	}

	bridgeAddr := s.cfg.L1Addrs.L1StandardBridge()
	if bridgeAddr == (common.Address{}) {
		s.metrics.AppendLog("[usdrif] L1StandardBridgeProxy not configured in l1.json")
		return
	}

	s.metrics.AppendLog("[usdrif] depositing %s USDRIF from %s via L1StandardBridge %s",
		weiToRBTCStr(amount), from.Hex(), bridgeAddr.Hex())

	// Parse minimal ABIs
	erc20ABI, err := abi.JSON(strings.NewReader(erc20ABI_JSON))
	if err != nil {
		s.metrics.AppendLog("[usdrif] parse ERC20 ABI: %v", err)
		return
	}
	bridgeABI, err := abi.JSON(strings.NewReader(l1StandardBridgeDepositABI))
	if err != nil {
		s.metrics.AppendLog("[usdrif] parse bridge ABI: %v", err)
		return
	}

	var totalGasCost big.Int

	// Step 1: Check existing allowance — skip approve if the bridge already
	// has enough approved (accounting for the pending block state so that
	// in-flight deposits reduce the visible allowance).
	needApproval := true
	allowanceData, err := erc20ABI.Pack("allowance", from, bridgeAddr)
	if err == nil {
		result, callErr := s.l1.PendingCallContract(s.ctx, ethereum.CallMsg{
			To:   &l1Token,
			Data: allowanceData,
		})
		if callErr == nil && len(result) >= 32 {
			currentAllowance := new(big.Int).SetBytes(result[:32])
			s.metrics.AppendLog("[usdrif] current allowance (pending): %s (%s USDRIF)",
				currentAllowance.String(), weiToRBTCStr(currentAllowance))
			if currentAllowance.Cmp(amount) >= 0 {
				s.metrics.AppendLog("[usdrif] allowance sufficient, skipping approve")
				needApproval = false
			}
		} else if callErr != nil {
			s.metrics.AppendLog("[usdrif] allowance query failed: %v, will approve", callErr)
		}
	}

	var approveCost *big.Int
	if needApproval {
		s.metrics.UpdateEvent(eventID, EventPending, "approving")
		s.metrics.AppendLog("[usdrif] step 1/2: approving bridge to spend USDRIF...")
		approveCallData, packErr := erc20ABI.Pack("approve", bridgeAddr, amount)
		if packErr != nil {
			s.metrics.AppendLog("[usdrif] pack approve: %v", packErr)
			return
		}

		nonce, err := s.l1.PendingNonceAt(s.ctx, from)
		if err != nil {
			s.metrics.AppendLog("[usdrif] nonce error: %v", err)
			return
		}
		gasPrice, err := s.l1GasPrice()
		if err != nil {
			s.metrics.AppendLog("[usdrif] gasPrice error: %v", err)
			return
		}

		s.metrics.AppendLog("[usdrif] approve nonce=%d gasPrice=%s", nonce, gasPrice)
		approveTx := types.NewTransaction(nonce, l1Token, big.NewInt(0), 100_000, gasPrice, approveCallData)
		signedApprove, err := types.SignTx(approveTx, types.NewEIP155Signer(l1ChainID), pk)
		if err != nil {
			s.metrics.AppendLog("[usdrif] sign approve: %v", err)
			return
		}
		if err := s.l1.SendTransaction(s.ctx, signedApprove); err != nil {
			s.metrics.AppendLog("[usdrif] send approve: %v", err)
			return
		}

		approveReceipt := s.waitForL1Receipt(signedApprove.Hash(), "usdrif-approve")
		if approveReceipt == nil {
			return
		}
		if approveReceipt.Status != types.ReceiptStatusSuccessful {
			s.metrics.AppendLog("[usdrif] approve REVERTED at L1 #%d", approveReceipt.BlockNumber.Uint64())
			s.metrics.UpdateEvent(eventID, EventFailed, "approve reverted")
			return
		}
		approveCost = receiptGasCost(approveReceipt)
		totalGasCost.Add(&totalGasCost, approveCost)
		s.metrics.AppendLog("[usdrif] approved at L1 #%d (gas: %.8f RBTC)", approveReceipt.BlockNumber.Uint64(), weiToFloat(approveCost))
	} else {
		s.metrics.AppendLog("[usdrif] step 1/2: skipped (allowance sufficient)")
	}

	// Step 2: Call depositERC20 on L1StandardBridge
	s.metrics.UpdateEvent(eventID, EventPending, "depositing via bridge")
	s.metrics.AppendLog("[usdrif] step 2/2: depositing via L1StandardBridge...")
	depositData, err := bridgeABI.Pack("depositERC20", l1Token, l2Token, amount, uint32(200_000), []byte{})
	if err != nil {
		s.metrics.AppendLog("[usdrif] pack depositERC20: %v", err)
		return
	}

	nonce, err := s.l1.PendingNonceAt(s.ctx, from)
	if err != nil {
		s.metrics.AppendLog("[usdrif] nonce error: %v", err)
		return
	}

	// Fetch fresh gas price for the deposit tx.
	gasPrice, err := s.l1GasPrice()
	if err != nil {
		s.metrics.AppendLog("[usdrif] gasPrice refresh error: %v", err)
		return
	}

	// Estimate gas for depositERC20 since the call traverses multiple proxy layers
	estimatedGas, estErr := s.l1.EstimateGas(s.ctx, ethereum.CallMsg{
		From:     from,
		To:       &bridgeAddr,
		GasPrice: gasPrice,
		Data:     depositData,
	})
	gasLimit := uint64(2_000_000) // fallback
	if estErr != nil {
		s.metrics.AppendLog("[usdrif] gas estimate failed (%v), using fallback %d", estErr, gasLimit)
	} else {
		gasLimit = estimatedGas * 130 / 100 // 30% buffer
		s.metrics.AppendLog("[usdrif] estimated gas: %d, using limit: %d", estimatedGas, gasLimit)
	}

	s.metrics.AppendLog("[usdrif] deposit nonce=%d gasPrice=%s gasLimit=%d", nonce, gasPrice, gasLimit)
	depositTx := types.NewTransaction(nonce, bridgeAddr, big.NewInt(0), gasLimit, gasPrice, depositData)
	signedDeposit, err := types.SignTx(depositTx, types.NewEIP155Signer(l1ChainID), pk)
	if err != nil {
		s.metrics.AppendLog("[usdrif] sign deposit: %v", err)
		return
	}
	if err := s.l1.SendTransaction(s.ctx, signedDeposit); err != nil {
		s.metrics.AppendLog("[usdrif] send deposit: %v", err)
		s.metrics.UpdateEvent(eventID, EventFailed, "send error")
		return
	}

	s.metrics.AppendLog("[usdrif] deposit tx sent %s to bridge %s, waiting for L1 receipt...",
		signedDeposit.Hash().Hex(), bridgeAddr.Hex())
	s.metrics.UpdateEventTxHash(eventID, signedDeposit.Hash().Hex())
	s.metrics.UpdateEvent(eventID, EventPending, "confirming deposit on L1")

	depositReceipt := s.waitForL1Receipt(signedDeposit.Hash(), "usdrif-deposit")
	if depositReceipt == nil {
		s.metrics.UpdateEvent(eventID, EventFailed, "receipt timeout")
		return
	}
	if depositReceipt.Status != types.ReceiptStatusSuccessful {
		// Log full TX hash and gas info for debugging
		s.metrics.AppendLog("[usdrif] deposit REVERTED at L1 #%d gasUsed=%d/%d tx=%s",
			depositReceipt.BlockNumber.Uint64(), depositReceipt.GasUsed, depositTx.Gas(), signedDeposit.Hash().Hex())
		// Try to replay the call to get the revert reason
		callMsg := ethereum.CallMsg{
			From:     from,
			To:       &bridgeAddr,
			Gas:      depositTx.Gas(),
			GasPrice: gasPrice,
			Data:     depositData,
		}
		_, callErr := s.l1.CallContract(s.ctx, callMsg, depositReceipt.BlockNumber)
		if callErr != nil {
			s.metrics.AppendLog("[usdrif] revert reason: %v", callErr)
		}
		s.metrics.UpdateEvent(eventID, EventFailed, fmt.Sprintf("reverted at L1 #%d", depositReceipt.BlockNumber.Uint64()))
		return
	}

	depositCost := receiptGasCost(depositReceipt)
	totalGasCost.Add(&totalGasCost, depositCost)

	approveF := 0.0
	if approveCost != nil {
		approveF = weiToFloat(approveCost)
	}
	s.metrics.AppendLog("[usdrif] COMPLETE! %s USDRIF deposited at L1 #%d. Total gas cost: %.8f RBTC (approve: %.8f, deposit: %.8f)",
		weiToRBTCStr(amount), depositReceipt.BlockNumber.Uint64(),
		weiToFloat(&totalGasCost), approveF, weiToFloat(depositCost))
	s.metrics.SetUSDRIFDepositCost(weiToFloat(&totalGasCost))
	s.metrics.UpdateEvent(eventID, EventComplete, fmt.Sprintf("L1 #%d", depositReceipt.BlockNumber.Uint64()))
}

// l1GasPrice returns a gas price suitable for L1 (RSK) transactions.
// It bumps the suggested gas price by 20% to improve inclusion speed,
// with a minimum floor of 60M wei.
func (s *L1Sender) l1GasPrice() (*big.Int, error) {
	gasPrice, err := s.l1.SuggestGasPrice(s.ctx)
	if err != nil {
		return nil, err
	}
	if gasPrice.Sign() == 0 {
		gasPrice = big.NewInt(60_000_000) // RSK minimum floor
	}
	// Bump by 20% for faster inclusion
	gasPrice = new(big.Int).Mul(gasPrice, big.NewInt(120))
	gasPrice = new(big.Int).Div(gasPrice, big.NewInt(100))
	return gasPrice, nil
}

// waitForL1Receipt polls for a transaction receipt on L1.
// RSK blocks can take 30s+ and tx inclusion may be delayed, so we
// wait up to 5 minutes (150 * 2s).
func (s *L1Sender) waitForL1Receipt(txHash common.Hash, label string) *types.Receipt {
	for i := 0; i < 150; i++ {
		select {
		case <-s.ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
		receipt, err := s.l1.TransactionReceipt(s.ctx, txHash)
		if err == nil && receipt != nil {
			return receipt
		}
	}
	// Timeout — try to determine whether the tx is still pending or was dropped.
	_, isPending, err := s.l1.TransactionByHash(s.ctx, txHash)
	if err != nil {
		s.metrics.AppendLog("[l1] %s receipt timeout after 5min (tx %s) — tx not found, likely dropped",
			label, txHash.Hex()[:10])
	} else if isPending {
		s.metrics.AppendLog("[l1] %s receipt timeout after 5min (tx %s) — tx still pending, gas price may be too low",
			label, txHash.Hex()[:10])
	} else {
		s.metrics.AppendLog("[l1] %s receipt timeout after 5min (tx %s) — tx mined but receipt unavailable",
			label, txHash.Hex()[:10])
	}
	return nil
}

// Minimal ABI for ERC20 approve + allowance.
const erc20ABI_JSON = `[
	{
		"inputs": [
			{"name": "spender", "type": "address"},
			{"name": "amount", "type": "uint256"}
		],
		"name": "approve",
		"outputs": [{"name": "", "type": "bool"}],
		"stateMutability": "nonpayable",
		"type": "function"
	},
	{
		"inputs": [
			{"name": "owner", "type": "address"},
			{"name": "spender", "type": "address"}
		],
		"name": "allowance",
		"outputs": [{"name": "", "type": "uint256"}],
		"stateMutability": "view",
		"type": "function"
	}
]`

// Minimal ABI for L1StandardBridge.depositERC20.
const l1StandardBridgeDepositABI = `[
	{
		"inputs": [
			{"name": "_l1Token", "type": "address"},
			{"name": "_l2Token", "type": "address"},
			{"name": "_amount", "type": "uint256"},
			{"name": "_minGasLimit", "type": "uint32"},
			{"name": "_extraData", "type": "bytes"}
		],
		"name": "depositERC20",
		"outputs": [],
		"stateMutability": "nonpayable",
		"type": "function"
	}
]`

// receiptGasCost computes the gas cost from a receipt: effectiveGasPrice * gasUsed.
func receiptGasCost(receipt *types.Receipt) *big.Int {
	gasPrice := receipt.EffectiveGasPrice
	if gasPrice == nil || gasPrice.Sign() == 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Mul(gasPrice, new(big.Int).SetUint64(receipt.GasUsed))
}

// parseBridgeAmount converts a decimal string like "0.01" to wei (18 decimals).
// Uses high-precision big.Float arithmetic to avoid truncation errors
// (e.g. 0.001 * 1e18 producing 999999999999999 instead of 1000000000000000).
func parseBridgeAmount(rbtcStr string) *big.Int {
	if rbtcStr == "" {
		return big.NewInt(1e16) // 0.01 RBTC
	}
	f := new(big.Float).SetPrec(256)
	if _, ok := f.SetString(rbtcStr); !ok {
		return big.NewInt(1e16)
	}
	// Construct 1e18 from big.Int for exact representation, then convert to big.Float.
	exp := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	weiMul := new(big.Float).SetPrec(256).SetInt(exp)
	f.Mul(f, weiMul)
	// Round to nearest integer (add 0.5 then truncate) to avoid off-by-one.
	f.Add(f, new(big.Float).SetPrec(256).SetFloat64(0.5))
	wei, _ := f.Int(nil)
	if wei.Sign() <= 0 {
		return big.NewInt(1e16)
	}
	return wei
}

// weiToRBTCStr converts wei to a human-readable RBTC string.
func weiToRBTCStr(wei *big.Int) string {
	if wei == nil {
		return "0"
	}
	f := new(big.Float).SetInt(wei)
	f.Quo(f, big.NewFloat(weiPerRBTC))
	return f.Text('f', 8)
}
