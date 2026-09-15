package monitor

import (
	"context"
	"fmt"
	"math/big"
	"math/rand"
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

const erc20TransferABI = `[{
	"inputs": [
		{"name": "to", "type": "address"},
		{"name": "amount", "type": "uint256"}
	],
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

// ERC20TrafficSimulator sends ERC20 transfer() calls on L2 at a configurable
// rate. Uses the same multi-account derivation and worker-pool architecture
// as TrafficSimulator but produces contract-call transactions (~65k gas)
// instead of simple value transfers (21k gas).
type ERC20TrafficSimulator struct {
	ctx     context.Context
	cfg     *Config
	metrics *MetricsStore
	l2      *ethclient.Client

	tokenAddr common.Address
	tokenABI  abi.ABI

	mu       sync.Mutex
	running  bool
	rate     float64  // txs per second
	txValue  *big.Int // token amount per transfer (in token smallest unit)
	gasLimit uint64   // gas limit for transfer() calls
	stopCh   chan struct{}

	consecutiveUnderpriced atomic.Int64
	pausedForGas           atomic.Bool

	accounts       []*trafficAccount
	accountsMu     sync.RWMutex
	targetAccounts atomic.Int32
	readyAccounts  atomic.Int32
}

// NewERC20TrafficSimulator creates a new ERC20 transfer traffic generator.
// tokenAddr is the L2 ERC20 contract address.
func NewERC20TrafficSimulator(ctx context.Context, cfg *Config, metrics *MetricsStore, tokenAddr common.Address) (*ERC20TrafficSimulator, error) {
	if cfg.PrivateKey == "" {
		return nil, fmt.Errorf("private key required for ERC20 traffic simulator")
	}
	if tokenAddr == (common.Address{}) {
		return nil, fmt.Errorf("ERC20 token address required")
	}

	pk, err := crypto.HexToECDSA(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}

	parsed, err := abi.JSON(strings.NewReader(erc20TransferABI))
	if err != nil {
		return nil, fmt.Errorf("parse ERC20 ABI: %w", err)
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
	httpClient := &http.Client{Transport: transport}
	rpcClient, err := rpc.DialOptions(ctx, cfg.L2RPC, rpc.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("dial L2 for ERC20 traffic: %w", err)
	}
	l2 := ethclient.NewClient(rpcClient)

	master := &trafficAccount{
		key:  pk,
		addr: crypto.PubkeyToAddress(pk.PublicKey),
	}
	master.funded.Store(true)

	// Sync master nonce immediately to handle restarts where on-chain nonce > 0.
	if nonce, err := l2.PendingNonceAt(ctx, master.addr); err == nil {
		master.nonce.Store(nonce)
	}

	numAccounts := cfg.TrafficAccounts
	if numAccounts <= 0 {
		numAccounts = 1
	}

	sim := &ERC20TrafficSimulator{
		ctx:       ctx,
		cfg:       cfg,
		metrics:   metrics,
		l2:        l2,
		tokenAddr: tokenAddr,
		tokenABI:  parsed,
		gasLimit:  80_000,
		accounts:  []*trafficAccount{master},
	}
	sim.targetAccounts.Store(int32(numAccounts))
	sim.readyAccounts.Store(1)

	return sim, nil
}

func (es *ERC20TrafficSimulator) Start(rate float64) {
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.running {
		return
	}
	es.rate = rate
	if es.rate <= 0 {
		es.rate = 1
	}
	es.running = true
	es.stopCh = make(chan struct{})
	go es.loop()
}

func (es *ERC20TrafficSimulator) Stop() {
	es.mu.Lock()
	defer es.mu.Unlock()
	if !es.running {
		return
	}
	es.running = false
	close(es.stopCh)
}

func (es *ERC20TrafficSimulator) SetRate(rate float64) {
	es.mu.Lock()
	defer es.mu.Unlock()
	es.rate = rate
}

func (es *ERC20TrafficSimulator) SetTxValue(v *big.Int) {
	es.mu.Lock()
	defer es.mu.Unlock()
	if v != nil && v.Sign() > 0 {
		es.txValue = new(big.Int).Set(v)
	} else {
		es.txValue = nil
	}
}

func (es *ERC20TrafficSimulator) SetNumAccounts(n int) {
	if n < 1 {
		n = 1
	}
	es.targetAccounts.Store(int32(n))
}

func (es *ERC20TrafficSimulator) IsRunning() bool {
	es.mu.Lock()
	defer es.mu.Unlock()
	return es.running
}

func (es *ERC20TrafficSimulator) getTxValue() *big.Int {
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.txValue != nil {
		return new(big.Int).Set(es.txValue)
	}
	return big.NewInt(1)
}

func (es *ERC20TrafficSimulator) randomRecipient(sender *trafficAccount, accts []*trafficAccount) common.Address {
	if len(accts) <= 1 {
		return sender.addr
	}
	for attempts := 0; attempts < 3; attempts++ {
		pick := accts[rand.Intn(len(accts))]
		if pick.addr != sender.addr {
			return pick.addr
		}
	}
	return sender.addr
}

func (es *ERC20TrafficSimulator) loop() {
	master := es.accounts[0]

	chainID, err := es.l2.ChainID(es.ctx)
	if err != nil {
		es.metrics.AppendLog("[erc20-traffic] chainID error: %v", err)
		return
	}

	signer := types.LatestSignerForChainID(chainID)

	gasPrice, err := es.l2.SuggestGasPrice(es.ctx)
	if err != nil {
		es.metrics.AppendLog("[erc20-traffic] gasPrice error: %v", err)
		return
	}
	if gasPrice.Sign() == 0 {
		gasPrice = big.NewInt(1_000_000)
	}

	// Estimate gas limit for a transfer() call once.
	es.estimateGasLimit(master.addr, gasPrice)

	// Re-sync nonces for funded accounts.
	es.accountsMu.RLock()
	for _, acc := range es.accounts {
		if acc.funded.Load() {
			if nonce, nerr := es.l2.PendingNonceAt(es.ctx, acc.addr); nerr == nil {
				acc.nonce.Store(nonce)
			} else {
				es.metrics.AppendLog("[erc20-traffic] nonce sync failed for %s: %v", acc.addr.Hex()[:10], nerr)
			}
		}
	}
	es.accountsMu.RUnlock()

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

	es.metrics.AppendLog("[erc20-traffic] started token=%s master=%s gasLimit=%d",
		es.tokenAddr.Hex()[:10], master.addr.Hex()[:10], es.gasLimit)

	sendOneTx := func(acc *trafficAccount) {
		n := acc.nonce.Add(1) - 1

		gasMu.RLock()
		gp := new(big.Int).Set(currentGasPrice)
		gasMu.RUnlock()

		amount := es.getTxValue()

		activeAcctsMu.RLock()
		snap := activeAcctsSnapshot
		activeAcctsMu.RUnlock()
		recipient := es.randomRecipient(acc, snap)

		calldata, err := es.tokenABI.Pack("transfer", recipient, amount)
		if err != nil {
			errSign.Add(1)
			es.metrics.AppendLog("[erc20-traffic] pack error: %v", err)
			return
		}

		tx := types.NewTransaction(n, es.tokenAddr, big.NewInt(0), es.gasLimit, gp, calldata)
		signed, signErr := types.SignTx(tx, signer, acc.key)
		if signErr != nil {
			errSign.Add(1)
			return
		}

		if sendErr := es.l2.SendTransaction(es.ctx, signed); sendErr != nil {
			if strings.Contains(sendErr.Error(), "insufficient funds") {
				errFunds.Add(1)
				acc.paused.Store(true)
				if freshNonce, nerr := es.l2.PendingNonceAt(es.ctx, acc.addr); nerr == nil {
					acc.nonce.Store(freshNonce)
				}
				return
			}

			errMsg := sendErr.Error()
			if strings.Contains(errMsg, "underpriced") || strings.Contains(errMsg, "txpool is full") {
				errGas.Add(1)
				uc := es.consecutiveUnderpriced.Add(1)
				if uc >= underpricedPauseThreshold && !es.pausedForGas.Load() {
					es.pausedForGas.Store(true)
					es.metrics.AppendLog("[erc20-traffic] AUTO-PAUSED: %d consecutive underpriced errors", uc)
				}
				return
			}

			ec := errSend.Add(1)
			if ec <= 3 || ec%50 == 0 {
				es.metrics.AppendLog("[erc20-traffic] send error (acct=%s nonce=%d): %v",
					acc.addr.Hex()[:10], n, sendErr)
			}
			if ec%50 == 0 {
				if freshNonce, nerr := es.l2.PendingNonceAt(es.ctx, acc.addr); nerr == nil {
					acc.nonce.Store(freshNonce)
				}
			}
			return
		}

		es.consecutiveUnderpriced.Store(0)
		s := sent.Add(1)
		if s == 1 || s%100 == 0 {
			es.metrics.AppendLog("[erc20-traffic] sent=%d %s (acct=%s)",
				s, errSummary(), acc.addr.Hex()[:10])
		}
	}

	done := make(chan struct{})
	defer close(done)

	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for acc := range workCh {
				sendOneTx(acc)
			}
		}()
	}

	// Gas price refresh
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if gp, gpErr := es.l2.SuggestGasPrice(es.ctx); gpErr == nil && gp.Sign() > 0 {
					gasMu.Lock()
					currentGasPrice.Set(gp)
					gasMu.Unlock()
				}
			}
		}
	}()

	// Funder goroutine: funds derived accounts with RBTC (for gas) and ERC20 tokens.
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				es.fundPendingAccounts(signer, &gasMu, currentGasPrice, done)
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
		case <-es.ctx.Done():
			close(workCh)
			wg.Wait()
			return
		case <-es.stopCh:
			close(workCh)
			wg.Wait()
			es.metrics.AppendLog("[erc20-traffic] stopped (sent=%d %s)", sent.Load(), errSummary())
			return
		case <-dispatchTicker.C:
			es.mu.Lock()
			rate := es.rate
			es.mu.Unlock()

			if rate <= 0 || es.pausedForGas.Load() {
				continue
			}

			readyCount := int(es.readyAccounts.Load())
			targetCount := int(es.targetAccounts.Load())
			activeCount := readyCount
			if targetCount < activeCount {
				activeCount = targetCount
			}
			if activeCount <= 0 {
				continue
			}

			es.accountsMu.RLock()
			var accts []*trafficAccount
			funding := readyCount < targetCount
			startIdx := 0
			if funding && activeCount > 1 {
				startIdx = 1
			}
			for _, a := range es.accounts[startIdx:activeCount] {
				if !a.paused.Load() {
					accts = append(accts, a)
				}
			}
			es.accountsMu.RUnlock()
			if len(accts) == 0 {
				continue
			}

			activeAcctsMu.Lock()
			activeAcctsSnapshot = accts
			activeAcctsMu.Unlock()

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

// estimateGasLimit does a one-time gas estimate for a transfer() call.
func (es *ERC20TrafficSimulator) estimateGasLimit(from common.Address, gasPrice *big.Int) {
	calldata, err := es.tokenABI.Pack("transfer", from, big.NewInt(0))
	if err != nil {
		return
	}
	tokenAddr := es.tokenAddr
	estimated, err := es.l2.EstimateGas(es.ctx, ethereum.CallMsg{
		From:     from,
		To:       &tokenAddr,
		GasPrice: gasPrice,
		Data:     calldata,
	})
	if err == nil && estimated > 0 {
		es.gasLimit = estimated * 130 / 100 // 30% buffer
		es.metrics.AppendLog("[erc20-traffic] estimated gas: %d, using limit: %d", estimated, es.gasLimit)
	}
}

// fundPendingAccounts generates/funds derived accounts with RBTC for gas
// and distributes ERC20 tokens from the master.
func (es *ERC20TrafficSimulator) fundPendingAccounts(
	signer types.Signer,
	gasMu *sync.RWMutex,
	currentGasPrice *big.Int,
	done <-chan struct{},
) {
	target := int(es.targetAccounts.Load())
	ready := int(es.readyAccounts.Load())
	master := es.accounts[0]

	if ready < target {
		es.accountsMu.Lock()
		for len(es.accounts) < target {
			idx := len(es.accounts)
			// Offset 10_000 puts the ERC20 simulator's derived accounts in a
			// disjoint namespace from the simple_tx and swap_tx simulators
			// (offsets 0 and 20_000 respectively). Required when running
			// mixed-traffic experiments — same idx without offset would
			// produce the same address and nonce-race the other simulator.
			acc := deriveAccount(master.key, 10_000+idx)
			es.accounts = append(es.accounts, acc)
		}
		es.accountsMu.Unlock()

		for i := ready; i < target; i++ {
			select {
			case <-done:
				return
			default:
			}

			es.accountsMu.RLock()
			acc := es.accounts[i]
			es.accountsMu.RUnlock()

			if acc.funded.Load() {
				if nonce, err := es.l2.PendingNonceAt(es.ctx, acc.addr); err == nil {
					acc.nonce.Store(nonce)
				}
				es.readyAccounts.Store(int32(i + 1))
				continue
			}

			if !es.fundOneAccount(acc, master, signer, gasMu, currentGasPrice, done) {
				return
			}

			acc.funded.Store(true)
			es.readyAccounts.Store(int32(i + 1))
			es.metrics.AppendLog("[erc20-traffic] account %d/%d %s ready", i+1, target, acc.addr.Hex()[:10])
		}
	}

	// Re-fund paused accounts
	readyCount := int(es.readyAccounts.Load())
	es.accountsMu.RLock()
	var paused []*trafficAccount
	for _, a := range es.accounts[1:readyCount] {
		if a.paused.Load() {
			paused = append(paused, a)
		}
	}
	es.accountsMu.RUnlock()

	for _, acc := range paused {
		select {
		case <-done:
			return
		default:
		}
		if !es.fundOneAccount(acc, master, signer, gasMu, currentGasPrice, done) {
			return
		}
		nonce, nerr := es.l2.PendingNonceAt(es.ctx, acc.addr)
		if nerr != nil {
			es.metrics.AppendLog("[erc20-traffic] nonce sync failed after re-fund for %s: %v", acc.addr.Hex()[:10], nerr)
			return // retry next tick
		}
		acc.nonce.Store(nonce)
		acc.paused.Store(false)
		es.metrics.AppendLog("[erc20-traffic] account %s re-funded and unpaused (nonce=%d)", acc.addr.Hex()[:10], nonce)
	}
}

// fundOneAccount sends RBTC (for gas) and ERC20 tokens to a derived account.
func (es *ERC20TrafficSimulator) fundOneAccount(
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

	// Phase 1: Fund with RBTC for gas (enough for ~5000 ERC20 transfers).
	bal, err := es.l2.BalanceAt(es.ctx, acc.addr, nil)
	if err != nil {
		return false
	}
	minBalance := new(big.Int).Mul(gp, big.NewInt(int64(es.gasLimit)*5000))
	if bal.Cmp(minBalance) < 0 {
		fundAmount := new(big.Int).Sub(minBalance, bal)
		n := master.nonce.Add(1) - 1
		tx := types.NewTransaction(n, acc.addr, fundAmount, 21000, gp, nil)
		signed, signErr := types.SignTx(tx, signer, master.key)
		if signErr != nil {
			master.nonce.Add(^uint64(0))
			return false
		}
		if sendErr := es.l2.SendTransaction(es.ctx, signed); sendErr != nil {
			master.nonce.Add(^uint64(0))
			if freshNonce, nerr := es.l2.PendingNonceAt(es.ctx, master.addr); nerr == nil {
				master.nonce.Store(freshNonce)
			}
			return false
		}
		// Wait for receipt
		hash := signed.Hash()
		for attempt := 0; attempt < 60; attempt++ {
			select {
			case <-done:
				return false
			case <-time.After(500 * time.Millisecond):
			}
			receipt, rErr := es.l2.TransactionReceipt(es.ctx, hash)
			if rErr == nil && receipt != nil {
				break
			}
		}
	}

	// Phase 2: Transfer ERC20 tokens from master to the derived account.
	// Check master's token balance first.
	masterTokenBal := es.tokenBalanceOf(master.addr)
	accTokenBal := es.tokenBalanceOf(acc.addr)

	// Fund enough for ~5000 transfers at current txValue.
	txVal := es.getTxValue()
	minTokenBal := new(big.Int).Mul(txVal, big.NewInt(5000))
	if accTokenBal.Cmp(minTokenBal) >= 0 {
		// Already has sufficient tokens. Sync nonce from chain.
		nonce, nerr := es.l2.PendingNonceAt(es.ctx, acc.addr)
		if nerr != nil {
			es.metrics.AppendLog("[erc20-traffic] failed to sync nonce for %s: %v", acc.addr.Hex()[:10], nerr)
			return false // retry later
		}
		acc.nonce.Store(nonce)
		es.metrics.AppendLog("[erc20-traffic] account %s already funded (nonce=%d)", acc.addr.Hex()[:10], nonce)
		return true
	}

	tokenFund := new(big.Int).Sub(minTokenBal, accTokenBal)
	if masterTokenBal.Cmp(tokenFund) < 0 {
		es.metrics.AppendLog("[erc20-traffic] master has insufficient ERC20 balance (%s < %s) — deposit more via bridge",
			masterTokenBal.String(), tokenFund.String())
		return true // don't block; the account can still be funded later
	}

	calldata, err := es.tokenABI.Pack("transfer", acc.addr, tokenFund)
	if err != nil {
		return false
	}

	n := master.nonce.Add(1) - 1
	tx := types.NewTransaction(n, es.tokenAddr, big.NewInt(0), es.gasLimit, gp, calldata)
	signed, signErr := types.SignTx(tx, signer, master.key)
	if signErr != nil {
		master.nonce.Add(^uint64(0))
		return false
	}
	if sendErr := es.l2.SendTransaction(es.ctx, signed); sendErr != nil {
		master.nonce.Add(^uint64(0))
		if freshNonce, nerr := es.l2.PendingNonceAt(es.ctx, master.addr); nerr == nil {
			master.nonce.Store(freshNonce)
		}
		return false
	}

	hash := signed.Hash()
	for attempt := 0; attempt < 60; attempt++ {
		select {
		case <-done:
			return false
		case <-time.After(500 * time.Millisecond):
		}
		receipt, rErr := es.l2.TransactionReceipt(es.ctx, hash)
		if rErr == nil && receipt != nil {
			// Sync child account nonce (may have txs from previous runs).
			nonce, nerr := es.l2.PendingNonceAt(es.ctx, acc.addr)
			if nerr != nil {
				es.metrics.AppendLog("[erc20-traffic] failed to sync nonce for %s after funding: %v", acc.addr.Hex()[:10], nerr)
				return false
			}
			acc.nonce.Store(nonce)
			es.metrics.AppendLog("[erc20-traffic] funded %s (nonce=%d)", acc.addr.Hex()[:10], nonce)
			return true
		}
	}

	return false
}

func (es *ERC20TrafficSimulator) tokenBalanceOf(addr common.Address) *big.Int {
	calldata, err := es.tokenABI.Pack("balanceOf", addr)
	if err != nil {
		return big.NewInt(0)
	}
	tokenAddr := es.tokenAddr
	result, err := es.l2.CallContract(es.ctx, ethereum.CallMsg{
		To:   &tokenAddr,
		Data: calldata,
	}, nil)
	if err != nil || len(result) < 32 {
		return big.NewInt(0)
	}
	return new(big.Int).SetBytes(result[:32])
}
