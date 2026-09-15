package monitor

import (
	"fmt"
	"math/big"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/gethclient"

	"github.com/ethereum-optimism/optimism/op-node/bindings"
	bindingspreview "github.com/ethereum-optimism/optimism/op-node/bindings/preview"
	"github.com/ethereum-optimism/optimism/op-node/withdrawals"
)

// buildL2Tx builds an EIP-1559 (type 2) transaction for the L2 chain.
// Legacy (type 0) transactions can cause sender-recovery mismatches on OP Stack.
func (s *L1Sender) buildL2Tx(to common.Address, value *big.Int, gasLimit uint64, data []byte) (*types.Transaction, error) {
	l2ChainID, err := s.l2.ChainID(s.ctx)
	if err != nil {
		return nil, fmt.Errorf("L2 chainID: %w", err)
	}
	head, err := s.l2.HeaderByNumber(s.ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("L2 header: %w", err)
	}
	baseFee := head.BaseFee
	if baseFee == nil || baseFee.Sign() == 0 {
		baseFee = big.NewInt(1_000_000)
	}
	tip := big.NewInt(1_000_000) // 1 Mwei tip
	feeCap := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), tip)

	pk, err := crypto.HexToECDSA(s.cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("key error: %w", err)
	}
	from := crypto.PubkeyToAddress(pk.PublicKey)

	nonce, err := s.l2.PendingNonceAt(s.ctx, from)
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}

	dynTx := &types.DynamicFeeTx{
		ChainID:   l2ChainID,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: feeCap,
		Gas:       gasLimit,
		To:        &to,
		Value:     value,
		Data:      data,
	}
	signed, err := types.SignTx(types.NewTx(dynTx), types.LatestSignerForChainID(l2ChainID), pk)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	return signed, nil
}

// Withdraw performs the full L2-to-L1 withdrawal flow:
// 1. Initiate withdrawal on L2 (send to L2ToL1MessagePasser)
// 2. Wait for dispute game covering the L2 block
// 3. Prove withdrawal on L1
// 4. Resolve dispute game
// 5. Wait for challenge period
// 6. Finalize withdrawal on L1
func (s *L1Sender) Withdraw() {
	eventID := s.metrics.AddEvent("withdraw-rbtc", weiToRBTCStr(s.bridgeAmountWei)+" RBTC")

	pk, err := crypto.HexToECDSA(s.cfg.PrivateKey)
	if err != nil {
		s.metrics.AppendLog("[withdraw] key error: %v", err)
		s.metrics.UpdateEvent(eventID, EventFailed, "key error")
		return
	}

	from := crypto.PubkeyToAddress(pk.PublicKey)

	// ---- Step 1: Initiate withdrawal on L2 ----
	s.metrics.AppendLog("[withdraw] step 1/6: initiating %s RBTC from %s on L2...",
		weiToRBTCStr(s.bridgeAmountWei), from.Hex()[:10])

	signed, err := s.buildL2Tx(l2ToL1MessagePasserAddr, s.bridgeAmountWei, 100_000, nil)
	if err != nil {
		s.metrics.AppendLog("[withdraw] build L2 tx error: %v", err)
		s.metrics.UpdateEvent(eventID, EventFailed, "build tx error")
		return
	}

	if err := s.l2.SendTransaction(s.ctx, signed); err != nil {
		s.metrics.AppendLog("[withdraw] send error: %v", err)
		s.metrics.UpdateEvent(eventID, EventFailed, "send error")
		return
	}

	s.metrics.UpdateEventTxHash(eventID, signed.Hash().Hex())
	s.metrics.UpdateEvent(eventID, EventPending, "confirming on L2")
	s.metrics.AppendLog("[withdraw] L2 tx sent %s, waiting for receipt...", signed.Hash().Hex()[:10])

	// Wait for L2 receipt
	var l2Receipt *types.Receipt
	for i := 0; i < 60; i++ {
		select {
		case <-s.ctx.Done():
			s.metrics.UpdateEvent(eventID, EventFailed, "context cancelled")
			return
		case <-time.After(2 * time.Second):
		}
		receipt, err := s.l2.TransactionReceipt(s.ctx, signed.Hash())
		if err == nil && receipt != nil {
			if receipt.Status != 1 {
				s.metrics.AppendLog("[withdraw] FAILED at L2 block #%d", receipt.BlockNumber.Uint64())
				s.metrics.UpdateEvent(eventID, EventFailed, fmt.Sprintf("reverted at L2 #%d", receipt.BlockNumber.Uint64()))
				return
			}
			l2Receipt = receipt
			break
		}
	}
	if l2Receipt == nil {
		s.metrics.AppendLog("[withdraw] receipt timeout after 120s")
		s.metrics.UpdateEvent(eventID, EventFailed, "receipt timeout")
		return
	}

	s.metrics.UpdateEvent(eventID, EventConfirmed, fmt.Sprintf("L2 #%d", l2Receipt.BlockNumber.Uint64()))

	// Track L2 initiation gas cost
	l2InitCost := receiptGasCost(l2Receipt)
	l2InitCostRBTC := weiToFloat(l2InitCost)

	s.metrics.AppendLog("[withdraw] initiated at L2 #%d (L2 gas: %.8f RBTC). Starting prove+finalize pipeline...",
		l2Receipt.BlockNumber.Uint64(), l2InitCostRBTC)

	// Continue in the same goroutine (Withdraw is already called in a goroutine from TUI)
	s.metrics.UpdateEvent(eventID, EventProving, "waiting for dispute game")
	proveCost, finalizeCost, ok := s.proveAndFinalize(from, signed.Hash(), l2Receipt, eventID)
	if ok {
		s.metrics.SetWithdrawCost(l2InitCostRBTC, proveCost, finalizeCost)
		s.metrics.AppendLog("[withdraw] COMPLETE! %s RBTC returned to %s. Total exit cost: %.8f RBTC (L2:%.8f P:%.8f F:%.8f)",
			weiToRBTCStr(s.bridgeAmountWei), from.Hex()[:10],
			l2InitCostRBTC+proveCost+finalizeCost, l2InitCostRBTC, proveCost, finalizeCost)
		s.metrics.UpdateEvent(eventID, EventComplete, fmt.Sprintf("exit cost: %.8f RBTC", l2InitCostRBTC+proveCost+finalizeCost))
	} else {
		s.metrics.UpdateEvent(eventID, EventFailed, "prove/finalize failed")
	}
}

// proveAndFinalize handles steps 2-6 of the withdrawal flow.
// Returns (proveCostRBTC, finalizeCostRBTC, success).
// eventID is used to update the pending event tracker through the lifecycle.
func (s *L1Sender) proveAndFinalize(from common.Address, l2TxHash common.Hash, l2Receipt *types.Receipt, eventID ...string) (float64, float64, bool) {
	evtID := ""
	if len(eventID) > 0 {
		evtID = eventID[0]
	}
	privKey, err := crypto.HexToECDSA(s.cfg.PrivateKey)
	if err != nil {
		s.metrics.AppendLog("[withdraw] key error: %v", err)
		return 0, 0, false
	}

	portalAddr := s.cfg.L1Addrs.OptimismPortal()
	dgfAddr := s.cfg.L1Addrs.DisputeGameFactory()

	if portalAddr == (common.Address{}) {
		s.metrics.AppendLog("[withdraw] OptimismPortalProxy not configured in l1.json")
		return 0, 0, false
	}
	if dgfAddr == (common.Address{}) {
		s.metrics.AppendLog("[withdraw] DisputeGameFactoryProxy not configured in l1.json")
		return 0, 0, false
	}

	// ---- Step 2: Wait for dispute game covering our L2 block ----
	s.metrics.AppendLog("[withdraw] step 2/6: waiting for dispute game covering L2 #%d...", l2Receipt.BlockNumber.Uint64())

	dgfCaller, err := bindings.NewDisputeGameFactoryCaller(dgfAddr, s.l1)
	if err != nil {
		s.metrics.AppendLog("[withdraw] create DGF caller error: %v", err)
		return 0, 0, false
	}
	portal2Caller, err := bindingspreview.NewOptimismPortal2Caller(portalAddr, s.l1)
	if err != nil {
		s.metrics.AppendLog("[withdraw] create portal2 caller error: %v", err)
		return 0, 0, false
	}

	// Poll until a dispute game covers our L2 block
	var gameL2BlockNumber *big.Int
	deadline := time.After(10 * time.Minute)
	for {
		select {
		case <-s.ctx.Done():
			return 0, 0, false
		case <-deadline:
			s.metrics.AppendLog("[withdraw] timeout waiting for dispute game (10m). Is op-proposer running?")
			return 0, 0, false
		case <-time.After(5 * time.Second):
		}

		latestGame, err := withdrawals.FindLatestGame(s.ctx, dgfCaller, portal2Caller)
		if err != nil {
			// No games yet, keep waiting
			continue
		}
		gameL2BlockNumber = new(big.Int).SetBytes(latestGame.ExtraData[0:32])
		if gameL2BlockNumber.Cmp(l2Receipt.BlockNumber) >= 0 {
			s.metrics.AppendLog("[withdraw] dispute game found covering L2 #%d (game covers up to #%d)",
				l2Receipt.BlockNumber.Uint64(), gameL2BlockNumber.Uint64())
			if evtID != "" {
				s.metrics.UpdateEvent(evtID, EventProving, "building proof")
			}
			break
		}
	}

	// Wait one more L1 block for timestamp safety
	time.Sleep(5 * time.Second)

	// ---- Step 3: Prove withdrawal on L1 ----
	s.metrics.AppendLog("[withdraw] step 3/6: building withdrawal proof...")

	proofCl := gethclient.New(s.l2RPC)
	params, err := withdrawals.ProveWithdrawalParametersFaultProofs(
		s.ctx, proofCl, s.l2, s.l2, l2TxHash, dgfCaller, portal2Caller,
	)
	if err != nil {
		s.metrics.AppendLog("[withdraw] build proof error: %v", err)
		return 0, 0, false
	}

	s.metrics.AppendLog("[withdraw] proof built: gameIndex=%s proofNodes=%d",
		params.L2OutputIndex.String(), len(params.WithdrawalProof))

	l1ChainID, err := s.l1.ChainID(s.ctx)
	if err != nil {
		s.metrics.AppendLog("[withdraw] L1 chainID error: %v", err)
		return 0, 0, false
	}

	portal, err := bindings.NewOptimismPortal(portalAddr, s.l1)
	if err != nil {
		s.metrics.AppendLog("[withdraw] create portal error: %v", err)
		return 0, 0, false
	}

	opts, err := bind.NewKeyedTransactorWithChainID(privKey, l1ChainID)
	if err != nil {
		s.metrics.AppendLog("[withdraw] create transactor error: %v", err)
		return 0, 0, false
	}

	// RSK's eth_estimateGas incorrectly reverts for complex calls that succeed on-chain.
	// Bypass gas estimation by setting a fixed gas limit.
	opts.GasLimit = 500_000
	// RSK also needs an explicit gas price (no EIP-1559 support)
	opts.GasPrice = big.NewInt(500_000_000)

	s.metrics.AppendLog("[withdraw] sending proveWithdrawalTransaction on L1 (gasLimit=500000)...")

	var proveTx *types.Transaction
	proveTx, err = portal.ProveWithdrawalTransaction(
		opts,
		bindings.TypesWithdrawalTransaction{
			Nonce:    params.Nonce,
			Sender:   params.Sender,
			Target:   params.Target,
			Value:    params.Value,
			GasLimit: params.GasLimit,
			Data:     params.Data,
		},
		params.L2OutputIndex,
		params.OutputRootProof,
		params.WithdrawalProof,
	)
	if err != nil {
		s.metrics.AppendLog("[withdraw] prove failed after retries: %v", err)
		return 0, 0, false
	}

	// Wait for prove receipt
	proveReceipt := s.waitForL1Receipt(proveTx.Hash(), "prove")
	if proveReceipt == nil {
		return 0, 0, false
	}
	if proveReceipt.Status != types.ReceiptStatusSuccessful {
		s.metrics.AppendLog("[withdraw] prove tx REVERTED at L1 #%d", proveReceipt.BlockNumber.Uint64())
		return 0, 0, false
	}
	proveCost := receiptGasCost(proveReceipt)
	proveCostRBTC := weiToFloat(proveCost)
	s.metrics.AppendLog("[withdraw] proved at L1 #%d (gas cost: %.8f RBTC)", proveReceipt.BlockNumber.Uint64(), proveCostRBTC)

	// ---- Step 4: Resolve dispute game ----
	if evtID != "" {
		s.metrics.UpdateEvent(evtID, EventProving, "resolving dispute game")
	}
	s.metrics.AppendLog("[withdraw] step 4/6: resolving dispute game...")

	// Get the dispute game proxy address from the proven withdrawal
	wdHash, err := withdrawals.WithdrawalHash(&bindings.L2ToL1MessagePasserMessagePassed{
		Nonce:    params.Nonce,
		Sender:   params.Sender,
		Target:   params.Target,
		Value:    params.Value,
		GasLimit: params.GasLimit,
		Data:     params.Data,
	})
	if err != nil {
		s.metrics.AppendLog("[withdraw] compute withdrawal hash error: %v", err)
		return 0, 0, false
	}

	proven, err := portal2Caller.ProvenWithdrawals(&bind.CallOpts{Context: s.ctx}, wdHash, from)
	if err != nil {
		s.metrics.AppendLog("[withdraw] get proven withdrawal error: %v", err)
		return 0, 0, false
	}

	gameProxy := proven.DisputeGameProxy
	s.metrics.AppendLog("[withdraw] dispute game at %s, resolving...", gameProxy.Hex()[:10])

	// Build minimal ABI for resolveClaim and resolve
	if err := s.resolveDisputeGame(l1ChainID, gameProxy); err != nil {
		s.metrics.AppendLog("[withdraw] game resolution error: %v", err)
		return 0, 0, false
	}

	// ---- Step 5: Wait for challenge period ----
	if evtID != "" {
		s.metrics.UpdateEvent(evtID, EventWaiting, "challenge period")
	}
	s.metrics.AppendLog("[withdraw] step 5/6: waiting for challenge period...")

	// Poll CheckWithdrawal until it succeeds.
	// The withdrawal becomes ready after disputeGameFinalityDelay + proofMaturityDelay
	// have elapsed since game resolution / proof submission.
	checkDeadline := time.After(s.cfg.DeployConfig.FinalizeTimeout())
	checkAttempts := 0
checkLoop:
	for {
		select {
		case <-s.ctx.Done():
			return 0, 0, false
		case <-checkDeadline:
			s.metrics.AppendLog("[withdraw] checkWithdrawal did not pass within %s, proceeding to finalize anyway", s.cfg.DeployConfig.FinalizeTimeout())
			break checkLoop
		case <-time.After(10 * time.Second):
		}
		checkAttempts++

		err := portal2Caller.CheckWithdrawal(&bind.CallOpts{Context: s.ctx}, wdHash, from)
		if err == nil {
			s.metrics.AppendLog("[withdraw] withdrawal ready for finalization!")
			break checkLoop
		}
		s.metrics.AppendLog("[withdraw] checkWithdrawal attempt %d: %v", checkAttempts, err)
	}

	// ---- Step 6: Finalize withdrawal on L1 ----
	if evtID != "" {
		s.metrics.UpdateEvent(evtID, EventFinalizing, "finalizing on L1")
	}
	s.metrics.AppendLog("[withdraw] step 6/6: finalizing withdrawal on L1...")

	// Need fresh opts with a new nonce
	opts, err = bind.NewKeyedTransactorWithChainID(privKey, l1ChainID)
	if err != nil {
		s.metrics.AppendLog("[withdraw] create transactor error: %v", err)
		return 0, 0, false
	}
	// RSK gas estimation workaround
	opts.GasLimit = 500_000
	opts.GasPrice = big.NewInt(500_000_000)

	finalizeTx, err := portal.FinalizeWithdrawalTransaction(
		opts,
		bindings.TypesWithdrawalTransaction{
			Nonce:    params.Nonce,
			Sender:   params.Sender,
			Target:   params.Target,
			Value:    params.Value,
			GasLimit: params.GasLimit,
			Data:     params.Data,
		},
	)
	if err != nil {
		s.metrics.AppendLog("[withdraw] finalize tx error: %v", err)
		return 0, 0, false
	}

	finalizeReceipt := s.waitForL1Receipt(finalizeTx.Hash(), "finalize")
	if finalizeReceipt == nil {
		return 0, 0, false
	}
	if finalizeReceipt.Status != types.ReceiptStatusSuccessful {
		s.metrics.AppendLog("[withdraw] finalize REVERTED at L1 #%d", finalizeReceipt.BlockNumber.Uint64())
		return 0, 0, false
	}

	finalizeCost := receiptGasCost(finalizeReceipt)
	finalizeCostRBTC := weiToFloat(finalizeCost)

	s.metrics.AppendLog("[withdraw] COMPLETE! Withdrawal finalized at L1 #%d. Exit cost (prove+finalize): %.8f RBTC (prove: %.8f, finalize: %.8f)",
		finalizeReceipt.BlockNumber.Uint64(),
		proveCostRBTC+finalizeCostRBTC, proveCostRBTC, finalizeCostRBTC)

	return proveCostRBTC, finalizeCostRBTC, true
}

// resolveDisputeGame drives the dispute game proxy to DEFENDER_WINS by nudging
// resolveClaim(0, 0) and resolve() until the game says so.
//
// The root claim resolves only after its chess clock expires, and the challenger
// may be the party that actually resolves it. A tx receipt alone proves inclusion,
// not success, so the game's status() is the only success signal trusted here:
// each call is dry-run first (so calls that would revert are skipped instead of
// mined) and the loop ends only when status() reports DEFENDER_WINS (success),
// CHALLENGER_WINS (the root claim was rejected — an error, never a skip) or the
// deadline expires.
func (s *L1Sender) resolveDisputeGame(l1ChainID *big.Int, gameProxy common.Address) error {
	privKey, err := crypto.HexToECDSA(s.cfg.PrivateKey)
	if err != nil {
		return fmt.Errorf("key error: %w", err)
	}
	sender := crypto.PubkeyToAddress(privKey.PublicKey)

	// Parse minimal ABI for FaultDisputeGame
	gameABI, err := abi.JSON(strings.NewReader(faultDisputeGameABI))
	if err != nil {
		return fmt.Errorf("parse game ABI: %w", err)
	}

	contract := bind.NewBoundContract(gameProxy, gameABI, s.l1, s.l1, s.l1)

	deadline := time.After(s.cfg.DeployConfig.ResolveClaimTimeout())
	for {
		var statusResult []interface{}
		if e := contract.Call(&bind.CallOpts{Context: s.ctx}, &statusResult, "status"); e == nil && len(statusResult) > 0 {
			if st, ok := statusResult[0].(uint8); ok {
				switch st {
				case 1:
					return fmt.Errorf("game %s resolved CHALLENGER_WINS: the root claim was rejected", gameProxy.Hex())
				case 2:
					s.metrics.AppendLog("[withdraw] game %s resolved (DEFENDER_WINS)", gameProxy.Hex()[:10])
					return nil
				}
			}
		}

		for _, c := range []struct {
			method string
			args   []interface{}
		}{
			{"resolveClaim", []interface{}{big.NewInt(0), big.NewInt(0)}},
			{"resolve", nil},
		} {
			// Dry-run first: a revert means "not yet" (clock still running) or
			// "not ours to resolve" (the challenger won the race), so wait
			// instead of mining a tx that would revert.
			var dry []interface{}
			if e := contract.Call(&bind.CallOpts{Context: s.ctx, From: sender}, &dry, c.method, c.args...); e != nil {
				continue
			}
			opts, e := bind.NewKeyedTransactorWithChainID(privKey, l1ChainID)
			if e != nil {
				return fmt.Errorf("create transactor: %w", e)
			}
			// RSK gas estimation workaround
			opts.GasLimit = 200_000
			opts.GasPrice = big.NewInt(500_000_000)

			tx, e := contract.Transact(opts, c.method, c.args...)
			if e != nil {
				s.metrics.AppendLog("[withdraw] %s tx error: %v", c.method, e)
				continue
			}
			// The receipt only proves inclusion; status() above decides success.
			s.waitForL1Receipt(tx.Hash(), c.method)
		}

		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-deadline:
			return fmt.Errorf("game %s not resolved within %s", gameProxy.Hex(), s.cfg.DeployConfig.ResolveClaimTimeout())
		case <-time.After(10 * time.Second):
		}
	}
}

// WithdrawUSDRIF performs the full L2-to-L1 USDRIF withdrawal flow:
// 1. On L2: call L2StandardBridge.withdraw(l2Token, amount, minGasLimit, extraData)
// 2-6. Same as RBTC withdrawal (wait for game, prove, resolve, challenge, finalize).
func (s *L1Sender) WithdrawUSDRIF() {
	if s.cfg.USDRIFTokenL1 == "" {
		s.metrics.AppendLog("[usdrif-withdraw] USDRIF L1 token address not configured")
		return
	}

	amountStr := s.cfg.BridgeAmountUSDRIF
	if amountStr == "" {
		amountStr = "0.001"
	}
	eventID := s.metrics.AddEvent("withdraw-usdrif", amountStr+" USDRIF")

	pk, err := crypto.HexToECDSA(s.cfg.PrivateKey)
	if err != nil {
		s.metrics.AppendLog("[usdrif-withdraw] key error: %v", err)
		s.metrics.UpdateEvent(eventID, EventFailed, "key error")
		return
	}

	from := crypto.PubkeyToAddress(pk.PublicKey)
	l1Token := common.HexToAddress(s.cfg.USDRIFTokenL1)

	// Ensure L2 token exists (auto-deploy if needed)
	l2Token, err := s.ensureUSDRIFL2Token(from, l1Token)
	if err != nil {
		s.metrics.AppendLog("[usdrif-withdraw] L2 token error: %v", err)
		return
	}

	// Parse bridge amount
	amount := parseBridgeAmount(s.cfg.BridgeAmountUSDRIF)
	if amount.Sign() <= 0 {
		amount = big.NewInt(1e15) // 0.001 USDRIF default
	}

	// ---- Step 1: Initiate withdrawal on L2 via L2StandardBridge ----

	// Check L2 USDRIF balance before attempting withdrawal
	balABI, _ := abi.JSON(strings.NewReader(`[{"inputs":[{"name":"account","type":"address"}],"name":"balanceOf","outputs":[{"name":"","type":"uint256"}],"stateMutability":"view","type":"function"}]`))
	balData, _ := balABI.Pack("balanceOf", from)
	balResult, balErr := s.l2.CallContract(s.ctx, ethereum.CallMsg{
		To:   &l2Token,
		Data: balData,
	}, nil)
	if balErr != nil {
		s.metrics.AppendLog("[usdrif-withdraw] cannot query L2 USDRIF balance: %v", balErr)
	} else if len(balResult) >= 32 {
		l2Balance := new(big.Int).SetBytes(balResult[:32])
		s.metrics.AppendLog("[usdrif-withdraw] L2 USDRIF balance of %s: %s (%s USDRIF)",
			from.Hex()[:10], l2Balance.String(), weiToRBTCStr(l2Balance))
		if l2Balance.Cmp(amount) < 0 {
			s.metrics.AppendLog("[usdrif-withdraw] INSUFFICIENT L2 USDRIF balance (%s < %s). Did the L1 deposit finalize on L2?",
				weiToRBTCStr(l2Balance), weiToRBTCStr(amount))
			return
		}
	}

	s.metrics.AppendLog("[usdrif-withdraw] step 1/6: withdrawing %s USDRIF from %s via L2StandardBridge...",
		weiToRBTCStr(amount), from.Hex()[:10])

	// Parse L2StandardBridge withdraw ABI
	bridgeABI, err := abi.JSON(strings.NewReader(l2StandardBridgeWithdrawABI))
	if err != nil {
		s.metrics.AppendLog("[usdrif-withdraw] parse bridge ABI: %v", err)
		return
	}

	withdrawData, err := bridgeABI.Pack("withdraw", l2Token, amount, uint32(200_000), []byte{})
	if err != nil {
		s.metrics.AppendLog("[usdrif-withdraw] pack withdraw: %v", err)
		return
	}

	// L2StandardBridge is at predeploy 0x4200000000000000000000000000000000000010
	l2BridgeAddr := common.HexToAddress("0x4200000000000000000000000000000000000010")

	// Estimate gas for the L2 withdrawal
	estimatedGas, estErr := s.l2.EstimateGas(s.ctx, ethereum.CallMsg{
		From: from,
		To:   &l2BridgeAddr,
		Data: withdrawData,
	})
	gasLimit := uint64(500_000) // fallback
	if estErr != nil {
		s.metrics.AppendLog("[usdrif-withdraw] gas estimate failed (%v), using fallback %d", estErr, gasLimit)
	} else {
		gasLimit = estimatedGas * 130 / 100 // 30% buffer
		s.metrics.AppendLog("[usdrif-withdraw] estimated gas: %d, using limit: %d", estimatedGas, gasLimit)
	}

	signed, err := s.buildL2Tx(l2BridgeAddr, big.NewInt(0), gasLimit, withdrawData)
	if err != nil {
		s.metrics.AppendLog("[usdrif-withdraw] build L2 tx error: %v", err)
		return
	}

	if err := s.l2.SendTransaction(s.ctx, signed); err != nil {
		s.metrics.AppendLog("[usdrif-withdraw] send error: %v", err)
		s.metrics.UpdateEvent(eventID, EventFailed, "send error")
		return
	}

	s.metrics.UpdateEventTxHash(eventID, signed.Hash().Hex())
	s.metrics.UpdateEvent(eventID, EventPending, "confirming on L2")
	s.metrics.AppendLog("[usdrif-withdraw] L2 tx sent %s, waiting for receipt...", signed.Hash().Hex()[:10])

	// Wait for L2 receipt
	var l2Receipt *types.Receipt
	for i := 0; i < 60; i++ {
		select {
		case <-s.ctx.Done():
			s.metrics.UpdateEvent(eventID, EventFailed, "context cancelled")
			return
		case <-time.After(2 * time.Second):
		}
		receipt, err := s.l2.TransactionReceipt(s.ctx, signed.Hash())
		if err == nil && receipt != nil {
			if receipt.Status != 1 {
				s.metrics.AppendLog("[usdrif-withdraw] FAILED at L2 block #%d gasUsed=%d/%d tx=%s",
					receipt.BlockNumber.Uint64(), receipt.GasUsed, signed.Gas(), signed.Hash().Hex())
				callMsg := ethereum.CallMsg{
					From: from,
					To:   &l2BridgeAddr,
					Gas:  signed.Gas(),
					Data: withdrawData,
				}
				_, callErr := s.l2.CallContract(s.ctx, callMsg, receipt.BlockNumber)
				if callErr != nil {
					s.metrics.AppendLog("[usdrif-withdraw] revert reason: %v", callErr)
				} else {
					s.metrics.AppendLog("[usdrif-withdraw] eth_call replay succeeded (no revert reason available)")
				}
				s.metrics.UpdateEvent(eventID, EventFailed, fmt.Sprintf("reverted at L2 #%d", receipt.BlockNumber.Uint64()))
				return
			}
			l2Receipt = receipt
			break
		}
	}
	if l2Receipt == nil {
		s.metrics.AppendLog("[usdrif-withdraw] receipt timeout after 120s")
		s.metrics.UpdateEvent(eventID, EventFailed, "receipt timeout")
		return
	}

	// Track L2 initiation gas cost
	l2InitCost := receiptGasCost(l2Receipt)
	l2InitCostRBTC := weiToFloat(l2InitCost)

	s.metrics.UpdateEvent(eventID, EventConfirmed, fmt.Sprintf("L2 #%d", l2Receipt.BlockNumber.Uint64()))
	s.metrics.AppendLog("[usdrif-withdraw] initiated at L2 #%d (L2 gas: %.8f RBTC). Starting prove+finalize pipeline...",
		l2Receipt.BlockNumber.Uint64(), l2InitCostRBTC)

	// Steps 2-6: reuse the same prove+finalize pipeline as RBTC withdrawal
	s.metrics.UpdateEvent(eventID, EventProving, "waiting for dispute game")
	proveCost, finalizeCost, ok := s.proveAndFinalize(from, signed.Hash(), l2Receipt, eventID)
	if ok {
		s.metrics.SetUSDRIFWithdrawCost(l2InitCostRBTC, proveCost, finalizeCost)
		s.metrics.AppendLog("[usdrif-withdraw] COMPLETE! %s USDRIF returned to %s. Total exit cost: %.8f RBTC (L2:%.8f P:%.8f F:%.8f)",
			weiToRBTCStr(amount), from.Hex()[:10],
			l2InitCostRBTC+proveCost+finalizeCost, l2InitCostRBTC, proveCost, finalizeCost)
		s.metrics.UpdateEvent(eventID, EventComplete, fmt.Sprintf("exit cost: %.8f RBTC", l2InitCostRBTC+proveCost+finalizeCost))
	} else {
		s.metrics.UpdateEvent(eventID, EventFailed, "prove/finalize failed")
	}
}

// ensureUSDRIFL2Token checks whether the L2 USDRIF token exists (has code).
// If cfg.USDRIFTokenL2 is set and has code, it returns that address.
// Otherwise it deploys a new OptimismMintableERC20 via the L2 factory predeploy
// and caches the address in cfg.USDRIFTokenL2.
func (s *L1Sender) ensureUSDRIFL2Token(from common.Address, l1Token common.Address) (common.Address, error) {
	// If already configured, check if it has code
	if s.cfg.USDRIFTokenL2 != "" {
		addr := common.HexToAddress(s.cfg.USDRIFTokenL2)
		code, err := s.l2.CodeAt(s.ctx, addr, nil)
		if err == nil && len(code) > 0 {
			return addr, nil
		}
		s.metrics.AppendLog("[usdrif] configured L2 token %s has no code, will deploy via factory", addr.Hex()[:10])
	}

	s.metrics.AppendLog("[usdrif] deploying L2 USDRIF token via OptimismMintableERC20Factory...")

	factoryAddr := common.HexToAddress("0x4200000000000000000000000000000000000012")

	factoryABI, err := abi.JSON(strings.NewReader(optimismMintableERC20FactoryABI))
	if err != nil {
		return common.Address{}, fmt.Errorf("parse factory ABI: %w", err)
	}

	data, err := factoryABI.Pack("createOptimismMintableERC20", l1Token, "USDRIF", "USDRIF")
	if err != nil {
		return common.Address{}, fmt.Errorf("pack createOptimismMintableERC20: %w", err)
	}

	// Check L2 balance for diagnostics
	bal, _ := s.l2.BalanceAt(s.ctx, from, nil)
	s.metrics.AppendLog("[usdrif] L2 factory deploy: from=%s balance=%s", from.Hex()[:10], bal)

	// Estimate gas
	estimatedGas, estErr := s.l2.EstimateGas(s.ctx, ethereum.CallMsg{
		From: from,
		To:   &factoryAddr,
		Data: data,
	})
	gasLimit := uint64(3_000_000) // fallback
	if estErr != nil {
		s.metrics.AppendLog("[usdrif] factory gas estimate failed (%v), using fallback %d", estErr, gasLimit)
	} else {
		gasLimit = estimatedGas * 130 / 100
		s.metrics.AppendLog("[usdrif] factory gas estimate: %d, using limit: %d", estimatedGas, gasLimit)
	}

	signed, err := s.buildL2Tx(factoryAddr, big.NewInt(0), gasLimit, data)
	if err != nil {
		return common.Address{}, err
	}

	if err := s.l2.SendTransaction(s.ctx, signed); err != nil {
		return common.Address{}, fmt.Errorf("send: %w", err)
	}

	s.metrics.AppendLog("[usdrif] factory tx sent %s, waiting for receipt...", signed.Hash().Hex()[:10])

	// Wait for receipt
	for i := 0; i < 30; i++ {
		select {
		case <-s.ctx.Done():
			return common.Address{}, fmt.Errorf("context cancelled")
		case <-time.After(2 * time.Second):
		}
		receipt, err := s.l2.TransactionReceipt(s.ctx, signed.Hash())
		if err == nil && receipt != nil {
			if receipt.Status != 1 {
				return common.Address{}, fmt.Errorf("factory tx reverted at L2 block #%d", receipt.BlockNumber.Uint64())
			}
			// Extract the L2 token address from the OptimismMintableERC20Created event.
			// Event signature: OptimismMintableERC20Created(address indexed localToken, address indexed remoteToken, address deployer)
			// The localToken (topic[1]) is the new L2 token address.
			eventSig := crypto.Keccak256Hash([]byte("OptimismMintableERC20Created(address,address,address)"))
			for _, log := range receipt.Logs {
				if log.Address == factoryAddr && len(log.Topics) >= 2 && log.Topics[0] == eventSig {
					l2TokenAddr := common.BytesToAddress(log.Topics[1].Bytes())
					s.metrics.AppendLog("[usdrif] L2 token deployed at %s", l2TokenAddr.Hex())
					// Cache in runtime config so subsequent calls don't redeploy
					s.cfg.USDRIFTokenL2 = l2TokenAddr.Hex()
					// Persist to config file so restarts don't re-deploy
					if err := SaveConfigField(s.cfg.ConfigPath, "usdrif_token_l2", l2TokenAddr.Hex()); err != nil {
						s.metrics.AppendLog("[usdrif] warning: could not save L2 token to config: %v", err)
					} else {
						s.metrics.AppendLog("[usdrif] saved usdrif_token_l2=%s to config", l2TokenAddr.Hex()[:10])
					}
					return l2TokenAddr, nil
				}
			}
			return common.Address{}, fmt.Errorf("factory tx succeeded but no OptimismMintableERC20Created event found in %d logs", len(receipt.Logs))
		}
	}
	return common.Address{}, fmt.Errorf("factory receipt timeout after 60s")
}

// Minimal ABI for FaultDisputeGame contract (resolveClaim, resolve, and status).
const faultDisputeGameABI = `[
	{
		"inputs": [{"name": "_claimIndex", "type": "uint256"}, {"name": "_numToResolve", "type": "uint256"}],
		"name": "resolveClaim",
		"outputs": [],
		"stateMutability": "nonpayable",
		"type": "function"
	},
	{
		"inputs": [],
		"name": "resolve",
		"outputs": [{"name": "status_", "type": "uint8"}],
		"stateMutability": "nonpayable",
		"type": "function"
	},
	{
		"inputs": [],
		"name": "status",
		"outputs": [{"name": "", "type": "uint8"}],
		"stateMutability": "view",
		"type": "function"
	}
]`

// Minimal ABI for OptimismMintableERC20Factory.createOptimismMintableERC20.
const optimismMintableERC20FactoryABI = `[
	{
		"inputs": [
			{"name": "_remoteToken", "type": "address"},
			{"name": "_name", "type": "string"},
			{"name": "_symbol", "type": "string"}
		],
		"name": "createOptimismMintableERC20",
		"outputs": [{"name": "", "type": "address"}],
		"stateMutability": "nonpayable",
		"type": "function"
	}
]`

// Minimal ABI for L2StandardBridge.withdraw (used for USDRIF withdrawal on L2).
const l2StandardBridgeWithdrawABI = `[
	{
		"inputs": [
			{"name": "_l2Token", "type": "address"},
			{"name": "_amount", "type": "uint256"},
			{"name": "_minGasLimit", "type": "uint32"},
			{"name": "_extraData", "type": "bytes"}
		],
		"name": "withdraw",
		"outputs": [],
		"stateMutability": "payable",
		"type": "function"
	}
]`
