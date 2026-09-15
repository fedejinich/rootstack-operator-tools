package monitor

import (
	"context"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// swapRouterMinimalABI captures just the swap entrypoint. The deploy
// path uses the full embedded artifact's ABI; the traffic loop only
// needs to pack `swap(...)`.
const swapRouterMinimalABI = `[{
	"inputs": [
		{"name": "tokenIn", "type": "address"},
		{"name": "tokenOut", "type": "address"},
		{"name": "amountIn", "type": "uint256"},
		{"name": "amountOutMin", "type": "uint256"},
		{"name": "to", "type": "address"},
		{"name": "deadline", "type": "uint256"}
	],
	"name": "swap",
	"outputs": [{"name": "amountOut", "type": "uint256"}],
	"stateMutability": "nonpayable",
	"type": "function"
}]`

// erc20ApproveABI augments erc20TransferABI with approve. Kept separate to
// avoid disturbing the existing ERC20 traffic simulator.
const erc20ApproveABI = `[{
	"inputs": [
		{"name": "spender", "type": "address"},
		{"name": "amount", "type": "uint256"}
	],
	"name": "approve",
	"outputs": [{"name": "", "type": "bool"}],
	"stateMutability": "nonpayable",
	"type": "function"
}]`

// SwapTrafficSimulator drives a mock swap protocol on L2. Each derived
// account holds reserves of TokenA + TokenB, has pre-approved the
// router for both, and alternates direction (A→B, B→A) on each call.
// Designed to measure L1 batch-cost overhead of swap-shaped calldata
// (~196-byte payload) vs simple_tx / erc20_tx (~68 bytes).
type SwapTrafficSimulator struct {
	ctx     context.Context
	cfg     *Config
	metrics *MetricsStore
	l2      *ethclient.Client

	env        SwapEnv
	routerABI  abi.ABI
	tokenABI   abi.ABI // transfer + balanceOf
	approveABI abi.ABI

	mu       sync.Mutex
	running  bool
	rate     float64
	txValue  *big.Int
	gasLimit uint64
	stopCh   chan struct{}

	consecutiveUnderpriced atomic.Int64
	pausedForGas           atomic.Bool

	accounts       []*trafficAccount
	accountsMu     sync.RWMutex
	targetAccounts atomic.Int32
	readyAccounts  atomic.Int32

	// Per-account swap-direction toggle. Index in es.accounts → bool
	// (false = A→B, true = B→A). Atomic for lock-free read in the loop.
	directionMu sync.Mutex
	direction   []bool
}

// NewSwapTrafficSimulator builds a swap traffic generator. `env` must
// already be deployed (use DeploySwapEnv if you don't have one yet).
func NewSwapTrafficSimulator(ctx context.Context, cfg *Config, metrics *MetricsStore, env SwapEnv) (*SwapTrafficSimulator, error) {
	if cfg.PrivateKey == "" {
		return nil, fmt.Errorf("private key required for swap traffic simulator")
	}
	if env.Router == (common.Address{}) || env.TokenA == (common.Address{}) || env.TokenB == (common.Address{}) {
		return nil, fmt.Errorf("swap env must have Router, TokenA, TokenB set")
	}

	pk, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(cfg.PrivateKey), "0x"))
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}

	routerABI, err := abi.JSON(strings.NewReader(swapRouterMinimalABI))
	if err != nil {
		return nil, fmt.Errorf("parse router ABI: %w", err)
	}
	tokenABI, err := abi.JSON(strings.NewReader(erc20TransferABI))
	if err != nil {
		return nil, fmt.Errorf("parse token ABI: %w", err)
	}
	approveABI, err := abi.JSON(strings.NewReader(erc20ApproveABI))
	if err != nil {
		return nil, fmt.Errorf("parse approve ABI: %w", err)
	}

	transport := &http.Transport{
		MaxConnsPerHost:     64,
		MaxIdleConnsPerHost: 64,
		MaxIdleConns:        64,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	rpcClient, err := rpc.DialOptions(ctx, cfg.L2RPC, rpc.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		return nil, fmt.Errorf("dial L2 for swap traffic: %w", err)
	}
	l2 := ethclient.NewClient(rpcClient)

	master := &trafficAccount{
		key:  pk,
		addr: crypto.PubkeyToAddress(pk.PublicKey),
	}
	master.funded.Store(true)
	if n, err := l2.PendingNonceAt(ctx, master.addr); err == nil {
		master.nonce.Store(n)
	}

	numAccounts := cfg.TrafficAccounts
	if numAccounts <= 0 {
		numAccounts = 1
	}

	sim := &SwapTrafficSimulator{
		ctx:        ctx,
		cfg:        cfg,
		metrics:    metrics,
		l2:         l2,
		env:        env,
		routerABI:  routerABI,
		tokenABI:   tokenABI,
		approveABI: approveABI,
		gasLimit:   120_000, // realistic for a 2-hop transferFrom+transfer+event
		accounts:   []*trafficAccount{master},
		direction:  []bool{false},
	}
	sim.targetAccounts.Store(int32(numAccounts))
	sim.readyAccounts.Store(1)
	return sim, nil
}

func (ss *SwapTrafficSimulator) Start(rate float64) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.running {
		return
	}
	ss.rate = rate
	if ss.rate <= 0 {
		ss.rate = 1
	}
	ss.running = true
	ss.stopCh = make(chan struct{})
	go ss.loop()
}

func (ss *SwapTrafficSimulator) Stop() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if !ss.running {
		return
	}
	ss.running = false
	close(ss.stopCh)
}

func (ss *SwapTrafficSimulator) SetRate(rate float64) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.rate = rate
}

func (ss *SwapTrafficSimulator) SetTxValue(v *big.Int) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if v != nil && v.Sign() > 0 {
		ss.txValue = new(big.Int).Set(v)
	} else {
		ss.txValue = nil
	}
}

func (ss *SwapTrafficSimulator) SetNumAccounts(n int) {
	if n < 1 {
		n = 1
	}
	ss.targetAccounts.Store(int32(n))
}

func (ss *SwapTrafficSimulator) IsRunning() bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.running
}

func (ss *SwapTrafficSimulator) getTxValue() *big.Int {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.txValue != nil {
		return new(big.Int).Set(ss.txValue)
	}
	// Default: 1 token-unit per swap (1 wei in token decimals). Small
	// enough that even a few hundred million swaps from the same
	// account stay within the funded balance.
	return big.NewInt(1)
}

func (ss *SwapTrafficSimulator) loop() {
	master := ss.accounts[0]

	chainID, err := ss.l2.ChainID(ss.ctx)
	if err != nil {
		ss.metrics.AppendLog("[swap-traffic] chainID error: %v", err)
		return
	}
	signer := types.LatestSignerForChainID(chainID)

	gasPrice, err := ss.l2.SuggestGasPrice(ss.ctx)
	if err != nil {
		ss.metrics.AppendLog("[swap-traffic] gasPrice error: %v", err)
		return
	}
	if gasPrice.Sign() == 0 {
		gasPrice = big.NewInt(1_000_000)
	}

	ss.estimateGasLimit(master.addr, gasPrice)

	// Re-sync nonces for funded accounts.
	ss.accountsMu.RLock()
	for _, acc := range ss.accounts {
		if acc.funded.Load() {
			if n, nerr := ss.l2.PendingNonceAt(ss.ctx, acc.addr); nerr == nil {
				acc.nonce.Store(n)
			}
		}
	}
	ss.accountsMu.RUnlock()

	var sent atomic.Int64
	var errSign, errFunds, errGas, errSend atomic.Int64
	totalErrs := func() int64 { return errSign.Load() + errFunds.Load() + errGas.Load() + errSend.Load() }
	errSummary := func() string {
		return fmt.Sprintf("errs=%d(sign=%d,funds=%d,gas=%d,send=%d)",
			totalErrs(), errSign.Load(), errFunds.Load(), errGas.Load(), errSend.Load())
	}

	var gasMu sync.RWMutex
	currentGasPrice := new(big.Int).Set(gasPrice)

	numWorkers := 32
	workCh := make(chan *trafficAccount, numWorkers*2)
	var activeAcctsMu sync.RWMutex
	var activeAcctsSnapshot []*trafficAccount

	ss.metrics.AppendLog("[swap-traffic] started router=%s tokenA=%s tokenB=%s master=%s gasLimit=%d",
		ss.env.Router.Hex()[:10], ss.env.TokenA.Hex()[:10], ss.env.TokenB.Hex()[:10],
		master.addr.Hex()[:10], ss.gasLimit)

	farDeadline := new(big.Int).SetInt64(1 << 62) // effectively never expires

	sendOneTx := func(acc *trafficAccount, idx int) {
		n := acc.nonce.Add(1) - 1

		gasMu.RLock()
		gp := new(big.Int).Set(currentGasPrice)
		gasMu.RUnlock()

		amount := ss.getTxValue()

		// Alternate direction per account.
		ss.directionMu.Lock()
		dir := false
		if idx < len(ss.direction) {
			dir = ss.direction[idx]
			ss.direction[idx] = !dir
		}
		ss.directionMu.Unlock()
		tokenIn, tokenOut := ss.env.TokenA, ss.env.TokenB
		if dir {
			tokenIn, tokenOut = ss.env.TokenB, ss.env.TokenA
		}

		calldata, err := ss.routerABI.Pack("swap",
			tokenIn, tokenOut, amount, big.NewInt(0), acc.addr, farDeadline)
		if err != nil {
			errSign.Add(1)
			ss.metrics.AppendLog("[swap-traffic] pack error: %v", err)
			return
		}

		tx := types.NewTransaction(n, ss.env.Router, big.NewInt(0), ss.gasLimit, gp, calldata)
		signed, signErr := types.SignTx(tx, signer, acc.key)
		if signErr != nil {
			errSign.Add(1)
			return
		}

		if sendErr := ss.l2.SendTransaction(ss.ctx, signed); sendErr != nil {
			errMsg := sendErr.Error()
			if strings.Contains(errMsg, "insufficient funds") {
				errFunds.Add(1)
				acc.paused.Store(true)
				if fresh, nerr := ss.l2.PendingNonceAt(ss.ctx, acc.addr); nerr == nil {
					acc.nonce.Store(fresh)
				}
				return
			}
			if strings.Contains(errMsg, "underpriced") || strings.Contains(errMsg, "txpool is full") {
				errGas.Add(1)
				uc := ss.consecutiveUnderpriced.Add(1)
				if uc >= underpricedPauseThreshold && !ss.pausedForGas.Load() {
					ss.pausedForGas.Store(true)
					ss.metrics.AppendLog("[swap-traffic] AUTO-PAUSED: %d consecutive underpriced errors", uc)
				}
				return
			}
			ec := errSend.Add(1)
			if ec <= 3 || ec%50 == 0 {
				ss.metrics.AppendLog("[swap-traffic] send error (acct=%s nonce=%d): %v",
					acc.addr.Hex()[:10], n, sendErr)
			}
			if ec%50 == 0 {
				if fresh, nerr := ss.l2.PendingNonceAt(ss.ctx, acc.addr); nerr == nil {
					acc.nonce.Store(fresh)
				}
			}
			return
		}

		ss.consecutiveUnderpriced.Store(0)
		s := sent.Add(1)
		if s == 1 || s%100 == 0 {
			ss.metrics.AppendLog("[swap-traffic] sent=%d %s (acct=%s dir=%v)",
				s, errSummary(), acc.addr.Hex()[:10], dir)
		}
	}

	done := make(chan struct{})
	defer close(done)

	var wg sync.WaitGroup
	type workItem struct {
		acc *trafficAccount
		idx int
	}
	workItems := make(chan workItem, numWorkers*2)
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for wi := range workItems {
				sendOneTx(wi.acc, wi.idx)
			}
		}()
	}
	// adapter: workCh → workItems with index lookup
	go func() {
		for acc := range workCh {
			ss.accountsMu.RLock()
			idx := -1
			for i, a := range ss.accounts {
				if a == acc {
					idx = i
					break
				}
			}
			ss.accountsMu.RUnlock()
			workItems <- workItem{acc: acc, idx: idx}
		}
		close(workItems)
	}()

	// Gas price refresh
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if gp, gpErr := ss.l2.SuggestGasPrice(ss.ctx); gpErr == nil && gp.Sign() > 0 {
					gasMu.Lock()
					currentGasPrice.Set(gp)
					gasMu.Unlock()
				}
			}
		}
	}()

	// Funder goroutine.
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				ss.fundPendingAccounts(signer, &gasMu, currentGasPrice, done)
			}
		}
	}()

	const ticksPerSec = 100
	dispatchTicker := time.NewTicker(time.Second / ticksPerSec)
	defer dispatchTicker.Stop()

	var accum float64
	var nextIdx uint64

	for {
		select {
		case <-ss.ctx.Done():
			close(workCh)
			wg.Wait()
			return
		case <-ss.stopCh:
			close(workCh)
			wg.Wait()
			ss.metrics.AppendLog("[swap-traffic] stopped (sent=%d %s)", sent.Load(), errSummary())
			return
		case <-dispatchTicker.C:
			ss.mu.Lock()
			rate := ss.rate
			ss.mu.Unlock()
			if rate <= 0 || ss.pausedForGas.Load() {
				continue
			}
			readyCount := int(ss.readyAccounts.Load())
			targetCount := int(ss.targetAccounts.Load())
			activeCount := readyCount
			if targetCount < activeCount {
				activeCount = targetCount
			}
			if activeCount <= 0 {
				continue
			}
			ss.accountsMu.RLock()
			var accts []*trafficAccount
			funding := readyCount < targetCount
			startIdx := 0
			if funding && activeCount > 1 {
				startIdx = 1
			}
			for _, a := range ss.accounts[startIdx:activeCount] {
				if !a.paused.Load() {
					accts = append(accts, a)
				}
			}
			ss.accountsMu.RUnlock()
			if len(accts) == 0 {
				continue
			}
			activeAcctsMu.Lock()
			activeAcctsSnapshot = accts
			activeAcctsMu.Unlock()
			_ = activeAcctsSnapshot

			accum += rate / ticksPerSec
			batch := int(accum)
			accum -= float64(batch)
			for i := 0; i < batch; i++ {
				acc := accts[nextIdx%uint64(len(accts))]
				nextIdx++
				select {
				case workCh <- acc:
				default:
				}
			}
		}
	}
}

func (ss *SwapTrafficSimulator) estimateGasLimit(from common.Address, gasPrice *big.Int) {
	farDeadline := new(big.Int).SetInt64(1 << 62)
	calldata, err := ss.routerABI.Pack("swap",
		ss.env.TokenA, ss.env.TokenB, big.NewInt(1), big.NewInt(0), from, farDeadline)
	if err != nil {
		return
	}
	router := ss.env.Router
	estimated, err := ss.l2.EstimateGas(ss.ctx, ethereum.CallMsg{
		From:     from,
		To:       &router,
		GasPrice: gasPrice,
		Data:     calldata,
	})
	if err == nil && estimated > 0 {
		ss.gasLimit = estimated * 130 / 100
		ss.metrics.AppendLog("[swap-traffic] estimated gas: %d, using limit: %d", estimated, ss.gasLimit)
	}
}

// fundPendingAccounts creates derived accounts on demand and funds them
// with RBTC for gas, tokens A and B for swapping, and pre-approves the
// router for both tokens. Only after all four phases succeed is the
// account marked ready.
func (ss *SwapTrafficSimulator) fundPendingAccounts(
	signer types.Signer,
	gasMu *sync.RWMutex,
	currentGasPrice *big.Int,
	done <-chan struct{},
) {
	target := int(ss.targetAccounts.Load())
	ready := int(ss.readyAccounts.Load())
	master := ss.accounts[0]

	if ready < target {
		ss.accountsMu.Lock()
		for len(ss.accounts) < target {
			idx := len(ss.accounts)
			// Offset 20_000 keeps swap traffic in a disjoint account
			// namespace from simple_tx (offset 0) and erc20_tx
			// (offset 10_000). Avoids nonce collisions when all three
			// simulators run concurrently in a mixed-traffic experiment.
			acc := deriveAccount(master.key, 20_000+idx)
			ss.accounts = append(ss.accounts, acc)
			ss.directionMu.Lock()
			ss.direction = append(ss.direction, false)
			ss.directionMu.Unlock()
		}
		ss.accountsMu.Unlock()

		for i := ready; i < target; i++ {
			select {
			case <-done:
				return
			default:
			}

			ss.accountsMu.RLock()
			acc := ss.accounts[i]
			ss.accountsMu.RUnlock()

			if acc.funded.Load() {
				if n, err := ss.l2.PendingNonceAt(ss.ctx, acc.addr); err == nil {
					acc.nonce.Store(n)
				}
				ss.readyAccounts.Store(int32(i + 1))
				continue
			}

			if !ss.fundOneAccount(acc, master, signer, gasMu, currentGasPrice, done) {
				return
			}

			acc.funded.Store(true)
			ss.readyAccounts.Store(int32(i + 1))
			ss.metrics.AppendLog("[swap-traffic] account %d/%d %s ready", i+1, target, acc.addr.Hex()[:10])
		}
	}

	// Re-fund paused accounts (same pattern as ERC20 sim).
	readyCount := int(ss.readyAccounts.Load())
	ss.accountsMu.RLock()
	var paused []*trafficAccount
	for _, a := range ss.accounts[1:readyCount] {
		if a.paused.Load() {
			paused = append(paused, a)
		}
	}
	ss.accountsMu.RUnlock()

	for _, acc := range paused {
		select {
		case <-done:
			return
		default:
		}
		if !ss.fundOneAccount(acc, master, signer, gasMu, currentGasPrice, done) {
			return
		}
		n, nerr := ss.l2.PendingNonceAt(ss.ctx, acc.addr)
		if nerr != nil {
			return
		}
		acc.nonce.Store(n)
		acc.paused.Store(false)
	}
}

// fundOneAccount: 4 phases — RBTC top-up, transfer TokenA, transfer
// TokenB, approve router for both. Returns true only on full success.
func (ss *SwapTrafficSimulator) fundOneAccount(
	acc *trafficAccount,
	master *trafficAccount,
	signer types.Signer,
	gasMu *sync.RWMutex,
	currentGasPrice *big.Int,
	done <-chan struct{},
) bool {
	gasMu.RLock()
	gp := new(big.Int).Set(currentGasPrice)
	gasMu.RUnlock()

	// Phase 1: RBTC for gas. Fund enough for ~500 swaps; the funder
	// goroutine re-funds paused accounts in the steady state. Keeps the
	// initial deposit per account small so a 10-account run doesn't
	// hoard the master's L2 balance up-front.
	bal, err := ss.l2.BalanceAt(ss.ctx, acc.addr, nil)
	if err != nil {
		return false
	}
	minBalance := new(big.Int).Mul(gp, big.NewInt(int64(ss.gasLimit)*500))
	if bal.Cmp(minBalance) < 0 {
		need := new(big.Int).Sub(minBalance, bal)
		ss.metrics.AppendLog("[swap-traffic] funding RBTC for %s: bal=%s need=%s gp=%s",
			acc.addr.Hex()[:10], bal.String(), need.String(), gp.String())
		if !ss.sendFromMaster(master, acc.addr, need, 21_000, nil, gp, signer, done, "rbtc-fund-"+acc.addr.Hex()[:10]) {
			return false
		}
	}

	// Phase 2 + 3: transfer TokenA and TokenB. ~5000 swaps per direction.
	txVal := ss.getTxValue()
	perTokenFund := new(big.Int).Mul(txVal, big.NewInt(5000))
	for _, tok := range []common.Address{ss.env.TokenA, ss.env.TokenB} {
		curBal := ss.tokenBalanceOf(tok, acc.addr)
		if curBal.Cmp(perTokenFund) >= 0 {
			continue
		}
		need := new(big.Int).Sub(perTokenFund, curBal)
		data, err := ss.tokenABI.Pack("transfer", acc.addr, need)
		if err != nil {
			return false
		}
		if !ss.sendFromMaster(master, tok, big.NewInt(0), 100_000, data, gp, signer, done, "token-fund-"+acc.addr.Hex()[:10]+"-"+tok.Hex()[:10]) {
			return false
		}
	}

	// Phase 4: approve router for both tokens (sent from the derived
	// account itself, not from master). Skip if already approved.
	if !ss.ensureAccountApprovals(acc, gp, signer, done) {
		return false
	}

	// Sync child nonce.
	n, nerr := ss.l2.PendingNonceAt(ss.ctx, acc.addr)
	if nerr != nil {
		return false
	}
	acc.nonce.Store(n)
	return true
}

// sendFromMaster sends a transaction from `master` to `to`. Handles
// nonce tracking and waits for the receipt. Logs each failure mode so a
// stuck funder is debuggable in the experiment output.
func (ss *SwapTrafficSimulator) sendFromMaster(
	master *trafficAccount,
	to common.Address,
	value *big.Int,
	gasLimit uint64,
	data []byte,
	gasPrice *big.Int,
	signer types.Signer,
	done <-chan struct{},
	label string,
) bool {
	n := master.nonce.Add(1) - 1
	tx := types.NewTransaction(n, to, value, gasLimit, gasPrice, data)
	signed, signErr := types.SignTx(tx, signer, master.key)
	if signErr != nil {
		master.nonce.Add(^uint64(0))
		ss.metrics.AppendLog("[swap-traffic] %s sign error: %v", label, signErr)
		return false
	}
	if sendErr := ss.l2.SendTransaction(ss.ctx, signed); sendErr != nil {
		master.nonce.Add(^uint64(0))
		if fresh, nerr := ss.l2.PendingNonceAt(ss.ctx, master.addr); nerr == nil {
			master.nonce.Store(fresh)
		}
		ss.metrics.AppendLog("[swap-traffic] %s send error (nonce=%d): %v", label, n, sendErr)
		return false
	}
	hash := signed.Hash()
	for attempt := 0; attempt < 120; attempt++ { // 60s total
		select {
		case <-done:
			return false
		case <-time.After(500 * time.Millisecond):
		}
		receipt, rerr := ss.l2.TransactionReceipt(ss.ctx, hash)
		if rerr == nil && receipt != nil {
			if receipt.Status != 1 {
				ss.metrics.AppendLog("[swap-traffic] %s tx reverted: %s nonce=%d gasUsed=%d", label, hash.Hex()[:12], n, receipt.GasUsed)
				return false
			}
			return true
		}
	}
	ss.metrics.AppendLog("[swap-traffic] %s receipt timeout: %s nonce=%d to=%s", label, hash.Hex()[:12], n, to.Hex()[:10])
	return false
}

// ensureAccountApprovals sends approve(router, max) for both tokens
// from the derived account, unless allowance is already nonzero.
func (ss *SwapTrafficSimulator) ensureAccountApprovals(
	acc *trafficAccount,
	gasPrice *big.Int,
	signer types.Signer,
	done <-chan struct{},
) bool {
	maxApproval := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

	// Sync account nonce first.
	n, err := ss.l2.PendingNonceAt(ss.ctx, acc.addr)
	if err != nil {
		return false
	}
	acc.nonce.Store(n)

	for _, tok := range []common.Address{ss.env.TokenA, ss.env.TokenB} {
		allowance, _ := ss.tokenAllowance(tok, acc.addr, ss.env.Router)
		if allowance.Sign() > 0 {
			continue
		}
		data, err := ss.approveABI.Pack("approve", ss.env.Router, maxApproval)
		if err != nil {
			return false
		}
		nonce := acc.nonce.Add(1) - 1
		tx := types.NewTransaction(nonce, tok, big.NewInt(0), 80_000, gasPrice, data)
		signed, signErr := types.SignTx(tx, signer, acc.key)
		if signErr != nil {
			acc.nonce.Add(^uint64(0))
			return false
		}
		if sendErr := ss.l2.SendTransaction(ss.ctx, signed); sendErr != nil {
			acc.nonce.Add(^uint64(0))
			return false
		}
		hash := signed.Hash()
		ok := false
		for attempt := 0; attempt < 60; attempt++ {
			select {
			case <-done:
				return false
			case <-time.After(500 * time.Millisecond):
			}
			receipt, rerr := ss.l2.TransactionReceipt(ss.ctx, hash)
			if rerr == nil && receipt != nil {
				ok = receipt.Status == 1
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func (ss *SwapTrafficSimulator) tokenBalanceOf(token, addr common.Address) *big.Int {
	calldata, err := ss.tokenABI.Pack("balanceOf", addr)
	if err != nil {
		return big.NewInt(0)
	}
	tok := token
	result, err := ss.l2.CallContract(ss.ctx, ethereum.CallMsg{To: &tok, Data: calldata}, nil)
	if err != nil || len(result) < 32 {
		return big.NewInt(0)
	}
	return new(big.Int).SetBytes(result[:32])
}

const erc20AllowanceABI = `[{
	"inputs": [
		{"name": "owner", "type": "address"},
		{"name": "spender", "type": "address"}
	],
	"name": "allowance",
	"outputs": [{"name": "", "type": "uint256"}],
	"stateMutability": "view",
	"type": "function"
}]`

func (ss *SwapTrafficSimulator) tokenAllowance(token, owner, spender common.Address) (*big.Int, error) {
	parsed, err := abi.JSON(strings.NewReader(erc20AllowanceABI))
	if err != nil {
		return big.NewInt(0), err
	}
	calldata, err := parsed.Pack("allowance", owner, spender)
	if err != nil {
		return big.NewInt(0), err
	}
	tok := token
	result, err := ss.l2.CallContract(ss.ctx, ethereum.CallMsg{To: &tok, Data: calldata}, nil)
	if err != nil || len(result) < 32 {
		return big.NewInt(0), err
	}
	return new(big.Int).SetBytes(result[:32]), nil
}
