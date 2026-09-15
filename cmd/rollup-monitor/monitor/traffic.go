package monitor

import (
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"fmt"
	"math/big"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// underpricedPauseThreshold is the number of consecutive "underpriced" send
// errors before the traffic simulator auto-pauses and asks the user to bump gas.
const underpricedPauseThreshold = 20

// trafficAccount represents a single sender account used by the traffic simulator.
// Each account has its own private key, address, and nonce counter to eliminate
// cross-account nonce contention.
type trafficAccount struct {
	key    *ecdsa.PrivateKey
	addr   common.Address
	nonce  atomic.Uint64
	funded atomic.Bool
	paused atomic.Bool // set when account runs out of funds; cleared after re-funding
}

// TrafficSimulator sends L2 transactions at a configurable rate,
// measuring send-to-receipt latency. Uses a worker pool with multiple funded
// accounts for realistic high-throughput traffic.
type TrafficSimulator struct {
	ctx     context.Context
	cfg     *Config
	metrics *MetricsStore
	l2      *ethclient.Client

	mu             sync.Mutex
	running        bool
	rate           float64  // txs per second
	gasMultiplier  float64  // multiplier applied to SuggestGasPrice (default 1.0)
	txValue        *big.Int // per-tx send value (default 0 = self-transfer)
	onlyMonologues bool     // when true, always send to self; when false, send to random other account
	calldataSize   int      // bytes of random calldata per tx (0 = plain transfer)
	stopCh         chan struct{}

	// Underpriced error tracking: auto-pause when txpool is full.
	consecutiveUnderpriced atomic.Int64
	pausedForGas           atomic.Bool

	// Multi-account state. accounts slice is append-only (protected by accountsMu).
	// targetAccounts is the desired count; readyAccounts tracks how many are funded.
	accounts       []*trafficAccount
	accountsMu     sync.RWMutex
	targetAccounts atomic.Int32
	readyAccounts  atomic.Int32
}

// NewTrafficSimulator creates a new TrafficSimulator. Requires a private key.
func NewTrafficSimulator(ctx context.Context, cfg *Config, metrics *MetricsStore) (*TrafficSimulator, error) {
	if cfg.PrivateKey == "" {
		return nil, fmt.Errorf("private key required for traffic simulator")
	}

	pk, err := crypto.HexToECDSA(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}

	// Custom HTTP transport with aggressive connection pooling to prevent
	// ephemeral port exhaustion at high send rates.
	transport := &http.Transport{
		MaxConnsPerHost:     128,
		MaxIdleConnsPerHost: 128,
		MaxIdleConns:        128,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	httpClient := &http.Client{Transport: transport}
	rpcClient, err := rpc.DialOptions(ctx, cfg.L2RPC, rpc.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("dial L2 for traffic: %w", err)
	}
	l2 := ethclient.NewClient(rpcClient)

	// Account 0 is the master (configured private key), always considered funded.
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

	ts := &TrafficSimulator{
		ctx:            ctx,
		cfg:            cfg,
		metrics:        metrics,
		l2:             l2,
		rate:           cfg.TrafficRate,
		gasMultiplier:  1.0,
		onlyMonologues: cfg.OnlyMonologues,
		calldataSize:   cfg.TxCalldataSize,
		accounts:       []*trafficAccount{master},
	}
	ts.targetAccounts.Store(int32(numAccounts))
	ts.readyAccounts.Store(1) // master is always ready

	return ts, nil
}

// DeriveTrafficKey deterministically derives a child private key and address
// from a master key and an integer index.
// Key = ToECDSA(keccak256(masterKeyBytes || bigEndian(index)))
// Deterministic derivation means the same accounts are reused across restarts,
// avoiding the need to re-fund.
func DeriveTrafficKey(masterKey *ecdsa.PrivateKey, index int) (*ecdsa.PrivateKey, common.Address) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(index))
	seed := crypto.Keccak256(masterKey.D.Bytes(), buf[:])
	key, _ := crypto.ToECDSA(seed) // keccak256 output is always a valid scalar
	return key, crypto.PubkeyToAddress(key.PublicKey)
}

func deriveAccount(masterKey *ecdsa.PrivateKey, index int) *trafficAccount {
	key, addr := DeriveTrafficKey(masterKey, index)
	return &trafficAccount{key: key, addr: addr}
}

// Start begins sending traffic at the given rate (txs/second).
func (ts *TrafficSimulator) Start(rate float64) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.running {
		return
	}

	ts.rate = rate
	if ts.rate <= 0 {
		ts.rate = 1
	}
	ts.running = true
	ts.stopCh = make(chan struct{})
	go ts.loop()
}

// Stop halts traffic generation.
func (ts *TrafficSimulator) Stop() {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if !ts.running {
		return
	}
	ts.running = false
	close(ts.stopCh)
}

// SetRate adjusts the traffic rate while running.
func (ts *TrafficSimulator) SetRate(rate float64) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.rate = rate
}

// SetTxValue sets the value (in wei) sent with each self-transfer transaction.
// A nil or zero value means zero-value self-transfers (the default).
func (ts *TrafficSimulator) SetTxValue(v *big.Int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if v != nil && v.Sign() > 0 {
		ts.txValue = new(big.Int).Set(v)
	} else {
		ts.txValue = nil
	}
}

// SetOnlyMonologues controls whether transactions are self-transfers (true)
// or sent to random other accounts in the pool (false).
func (ts *TrafficSimulator) SetOnlyMonologues(v bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.onlyMonologues = v
}

// randomRecipient picks a random funded account address different from sender.
// Falls back to the sender's own address if only one account is available.
func (ts *TrafficSimulator) randomRecipient(sender *trafficAccount, accts []*trafficAccount) common.Address {
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

// getTxValue returns the current per-tx value under the lock.
func (ts *TrafficSimulator) getTxValue() *big.Int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.txValue != nil {
		return new(big.Int).Set(ts.txValue)
	}
	return big.NewInt(0)
}

// IsRunning returns whether the simulator is active.
func (ts *TrafficSimulator) IsRunning() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.running
}

// SetNumAccounts sets the target number of traffic accounts.
// New accounts are funded asynchronously by the funder goroutine inside loop().
func (ts *TrafficSimulator) SetNumAccounts(n int) {
	if n < 1 {
		n = 1
	}
	ts.targetAccounts.Store(int32(n))
}

// NumAccounts returns (target, ready) account counts for TUI display.
func (ts *TrafficSimulator) NumAccounts() (target, ready int) {
	return int(ts.targetAccounts.Load()), int(ts.readyAccounts.Load())
}

// GasMultiplier returns the current gas price multiplier.
func (ts *TrafficSimulator) GasMultiplier() float64 {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.gasMultiplier
}

// SetGasMultiplier sets the gas price multiplier. Also resets the
// consecutive underpriced counter so a fresh burst is needed to re-trigger a pause.
func (ts *TrafficSimulator) SetGasMultiplier(m float64) {
	if m < 1.0 {
		m = 1.0
	}
	ts.mu.Lock()
	ts.gasMultiplier = m
	ts.mu.Unlock()
	ts.consecutiveUnderpriced.Store(0)
}

// IsPausedForGas returns true when the simulator has auto-paused because the
// txpool is full (too many consecutive underpriced errors).
func (ts *TrafficSimulator) IsPausedForGas() bool {
	return ts.pausedForGas.Load()
}

// ResumeFromGasPause clears the gas-pause flag and resets the underpriced
// counter so the dispatcher resumes sending with the (presumably bumped) gas price.
func (ts *TrafficSimulator) ResumeFromGasPause() {
	ts.consecutiveUnderpriced.Store(0)
	ts.pausedForGas.Store(false)
}

// applyGasMultiplier returns gp * gasMultiplier using integer arithmetic.
func (ts *TrafficSimulator) applyGasMultiplier(gp *big.Int) *big.Int {
	ts.mu.Lock()
	mul := ts.gasMultiplier
	ts.mu.Unlock()
	if mul <= 1.0 {
		return gp
	}
	// Scale: gp * (mul*100) / 100  to avoid float→big.Int precision issues.
	scaled := new(big.Int).Mul(gp, big.NewInt(int64(mul*100)))
	return scaled.Div(scaled, big.NewInt(100))
}

// loop is the main traffic generation loop. It uses a dispatcher+worker-pool
// architecture with multiple funded accounts for high throughput:
//
//   - A funder goroutine monitors targetAccounts and funds new derived accounts
//     from the master account via L2 value transfers.
//   - A dispatcher fires at 100 ticks/sec, round-robining work items across
//     funded accounts and pushing them into a buffered work channel.
//   - A pool of worker goroutines reads work items and sends self-transfer txs
//     using the assigned account's key and per-account atomic nonce.
//   - Gas price is refreshed every 30s by a background goroutine.
func (ts *TrafficSimulator) loop() {
	master := ts.accounts[0]

	chainID, err := ts.l2.ChainID(ts.ctx)
	if err != nil {
		ts.metrics.AppendLog("[traffic] chainID error: %v", err)
		return
	}

	signer := types.LatestSignerForChainID(chainID)

	// Fetch gas price once (refreshed periodically by background goroutine).
	gasPrice, err := ts.l2.SuggestGasPrice(ts.ctx)
	if err != nil {
		ts.metrics.AppendLog("[traffic] gasPrice error: %v", err)
		return
	}
	if gasPrice.Sign() == 0 {
		gasPrice = big.NewInt(1_000_000)
	}

	// Re-sync nonces for all funded accounts (handles stop/restart correctly).
	ts.accountsMu.RLock()
	for _, acc := range ts.accounts {
		if acc.funded.Load() {
			if nonce, nerr := ts.l2.PendingNonceAt(ts.ctx, acc.addr); nerr == nil {
				acc.nonce.Store(nonce)
			} else {
				ts.metrics.AppendLog("[traffic] nonce sync failed for %s: %v", acc.addr.Hex()[:10], nerr)
			}
		}
	}
	ts.accountsMu.RUnlock()

	// --- Shared state ---

	var sent atomic.Int64
	var errSign, errFunds, errGas, errSend atomic.Int64
	totalErrs := func() int64 { return errSign.Load() + errFunds.Load() + errGas.Load() + errSend.Load() }
	errSummary := func() string {
		return fmt.Sprintf("errs=%d(sign=%d,funds=%d,gas=%d,send=%d)",
			totalErrs(), errSign.Load(), errFunds.Load(), errGas.Load(), errSend.Load())
	}

	// Gas price protected by RWMutex: refresher writes, workers read.
	var gasMu sync.RWMutex
	currentGasPrice := new(big.Int).Set(gasPrice)

	numWorkers := ts.cfg.TrafficWorkers
	if numWorkers <= 0 {
		numWorkers = 64
	}

	workCh := make(chan *trafficAccount, numWorkers*2)

	// Receipt watcher semaphore: limits concurrent waitForReceipt goroutines
	// to prevent ephemeral port exhaustion at high send rates.
	receiptSem := make(chan struct{}, 256)

	ts.metrics.AppendLog("[traffic] started master=%s nonce=%d gasPrice=%s workers=%d accounts=%d",
		master.addr.Hex()[:10], master.nonce.Load(), gasPrice.String(), numWorkers,
		ts.targetAccounts.Load())

	// activeAcctsSnapshot holds a recent copy of funded accounts for cross-account sends.
	var activeAcctsMu sync.RWMutex
	var activeAcctsSnapshot []*trafficAccount

	// sendOneTx builds, signs, and sends one transaction using the given
	// account's key and per-account nonce. When onlyMonologues is false,
	// transactions are sent to a random other account in the pool.
	sendOneTx := func(acc *trafficAccount) {
		n := acc.nonce.Add(1) - 1

		gasMu.RLock()
		gp := new(big.Int).Set(currentGasPrice)
		gasMu.RUnlock()

		// Apply user-configurable gas multiplier (bumped via TUI 'g' key).
		gp = ts.applyGasMultiplier(gp)

		txVal := ts.getTxValue()

		ts.mu.Lock()
		monologues := ts.onlyMonologues
		ts.mu.Unlock()

		recipient := acc.addr
		if !monologues {
			activeAcctsMu.RLock()
			snap := activeAcctsSnapshot
			activeAcctsMu.RUnlock()
			recipient = ts.randomRecipient(acc, snap)
		}
		var data []byte
		gasLimit := uint64(21000)
		if ts.calldataSize > 0 {
			data = make([]byte, ts.calldataSize)
			for i := range data {
				data[i] = byte(rand.Intn(256))
			}
			for _, b := range data {
				if b == 0 {
					gasLimit += 4
				} else {
					gasLimit += 16
				}
			}
		}
		tx := types.NewTransaction(n, recipient, txVal, gasLimit, gp, data)
		signed, signErr := types.SignTx(tx, signer, acc.key)
		if signErr != nil {
			errSign.Add(1)
			ts.metrics.AppendLog("[traffic] sign error: %v", signErr)
			return
		}

		start := time.Now()
		if sendErr := ts.l2.SendTransaction(ts.ctx, signed); sendErr != nil {
			// Detect depleted account: pause it so dispatcher skips it
			// and the funder re-funds it.
			if strings.Contains(sendErr.Error(), "insufficient funds") {
				errFunds.Add(1)
				acc.paused.Store(true)
				// Re-sync nonce so we don't waste nonces while paused.
				if freshNonce, nerr := ts.l2.PendingNonceAt(ts.ctx, acc.addr); nerr == nil {
					acc.nonce.Store(freshNonce)
				}
				ts.metrics.AppendLog("[traffic] account %s depleted, pausing for re-fund",
					acc.addr.Hex()[:10])
				return
			}

			// Detect txpool-full / underpriced errors: auto-pause after threshold.
			errMsg := sendErr.Error()
			if strings.Contains(errMsg, "underpriced") || strings.Contains(errMsg, "txpool is full") {
				errGas.Add(1)
				uc := ts.consecutiveUnderpriced.Add(1)
				if uc == 1 || uc%50 == 0 {
					ts.metrics.AppendLog("[traffic] underpriced tx (acct=%s nonce=%d, consecutive=%d): %v",
						acc.addr.Hex()[:10], n, uc, sendErr)
				}
				if uc >= underpricedPauseThreshold && !ts.pausedForGas.Load() {
					ts.pausedForGas.Store(true)
					ts.metrics.AppendLog("[traffic] AUTO-PAUSED: %d consecutive underpriced errors — press 'g' to bump gas price or 't' to stop", uc)
				}
				return
			}

			ec := errSend.Add(1)
			if ec <= 3 || ec%50 == 0 {
				ts.metrics.AppendLog("[traffic] send error (acct=%s nonce=%d): %v",
					acc.addr.Hex()[:10], n, sendErr)
			}
			// Re-sync nonce periodically on sustained errors.
			if ec%50 == 0 {
				if freshNonce, nerr := ts.l2.PendingNonceAt(ts.ctx, acc.addr); nerr == nil {
					old := acc.nonce.Load()
					if freshNonce > old {
						acc.nonce.Store(freshNonce)
						ts.metrics.AppendLog("[traffic] re-synced nonce %s: %d -> %d",
							acc.addr.Hex()[:10], old, freshNonce)
					}
				}
			}
			return
		}

		// Successful send — reset underpriced counter.
		ts.consecutiveUnderpriced.Store(0)

		s := sent.Add(1)
		if s == 1 || s%100 == 0 {
			ts.metrics.AppendLog("[traffic] sent=%d %s (acct=%s nonce=%d)",
				s, errSummary(), acc.addr.Hex()[:10], n+1)
		}

		// Register tx send time for accurate L2-to-L1 latency tracking.
		txHash := signed.Hash()
		ts.metrics.RegisterTxSendTime(txHash, start)

		// Guard receipt watchers with semaphore to prevent socket exhaustion.
		select {
		case receiptSem <- struct{}{}:
			go func() {
				defer func() { <-receiptSem }()
				ts.waitForReceipt(txHash, start)
			}()
		default:
			// Semaphore full; skip receipt tracking to prevent socket exhaustion.
			// TPS chart is unaffected (it counts txs in blocks, not receipts).
		}
	}

	// done is closed when loop() exits, signaling helper goroutines to stop.
	done := make(chan struct{})
	defer close(done)

	// Start worker pool.
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

	// Gas price refresh goroutine.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if gp, gpErr := ts.l2.SuggestGasPrice(ts.ctx); gpErr == nil && gp.Sign() > 0 {
					gasMu.Lock()
					currentGasPrice.Set(gp)
					gasMu.Unlock()
				}
			}
		}
	}()

	// Funder goroutine: watches targetAccounts and funds new derived accounts.
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				ts.fundPendingAccounts(signer, &gasMu, currentGasPrice, done)
			}
		}
	}()

	// Dispatcher: fixed tick rate, batch N items per tick, round-robin across funded accounts.
	const ticksPerSec = 100
	dispatchTicker := time.NewTicker(time.Second / ticksPerSec)
	defer dispatchTicker.Stop()

	var accum float64
	var nextIdx uint64

	for {
		select {
		case <-ts.ctx.Done():
			close(workCh)
			wg.Wait()
			return
		case <-ts.stopCh:
			close(workCh)
			wg.Wait()
			ts.metrics.AppendLog("[traffic] stopped (sent=%d %s)", sent.Load(), errSummary())
			return
		case <-dispatchTicker.C:
			ts.mu.Lock()
			rate := ts.rate
			ts.mu.Unlock()

			if rate <= 0 || ts.pausedForGas.Load() {
				continue
			}

			// Active accounts = min(ready, target) so decreasing target takes effect immediately.
			readyCount := int(ts.readyAccounts.Load())
			targetCount := int(ts.targetAccounts.Load())
			activeCount := readyCount
			if targetCount < activeCount {
				activeCount = targetCount
			}
			if activeCount <= 0 {
				continue
			}

			// While funding/re-funding is in progress, exclude the master (index 0)
			// from traffic so its nonces are reserved for funding transactions.
			// Once all target accounts are funded and none are paused, the master
			// rejoins the traffic pool.
			ts.accountsMu.RLock()
			var accts []*trafficAccount
			hasPaused := false
			startIdx := 0
			funding := readyCount < targetCount
			if funding && activeCount > 1 {
				startIdx = 1
			}
			for _, a := range ts.accounts[startIdx:activeCount] {
				if a.paused.Load() {
					hasPaused = true
					continue
				}
				accts = append(accts, a)
			}
			// Also exclude master from traffic while paused accounts need re-funding.
			if hasPaused && !funding && len(accts) > 0 && accts[0] == ts.accounts[0] {
				accts = accts[1:]
			}
			ts.accountsMu.RUnlock()
			if len(accts) == 0 {
				continue
			}

			// Update snapshot for cross-account recipient selection.
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
					// Workers saturated; drop to avoid blocking the dispatcher.
				}
			}
		}
	}
}

// fundPendingAccounts generates and funds accounts up to targetAccounts,
// and re-funds any depleted (paused) accounts.
// Called periodically by the funder goroutine inside loop().
func (ts *TrafficSimulator) fundPendingAccounts(
	signer types.Signer,
	gasMu *sync.RWMutex,
	currentGasPrice *big.Int,
	done <-chan struct{},
) {
	target := int(ts.targetAccounts.Load())
	ready := int(ts.readyAccounts.Load())

	master := ts.accounts[0]

	// --- Phase 1: fund new (not yet ready) accounts ---
	if ready < target {
		// Ensure we have enough account slots generated (append-only).
		ts.accountsMu.Lock()
		for len(ts.accounts) < target {
			idx := len(ts.accounts)
			acc := deriveAccount(master.key, idx)
			ts.accounts = append(ts.accounts, acc)
		}
		ts.accountsMu.Unlock()

		// Fund each unfunded account sequentially.
		for i := ready; i < target; i++ {
			select {
			case <-done:
				return
			default:
			}

			ts.accountsMu.RLock()
			acc := ts.accounts[i]
			ts.accountsMu.RUnlock()

			if acc.funded.Load() {
				// Already funded (e.g. from a previous session); just sync nonce.
				if nonce, err := ts.l2.PendingNonceAt(ts.ctx, acc.addr); err == nil {
					acc.nonce.Store(nonce)
				}
				ts.readyAccounts.Store(int32(i + 1))
				continue
			}

			if !ts.fundOneAccount(acc, master, signer, gasMu, currentGasPrice, done) {
				return // retry next tick
			}

			acc.funded.Store(true)
			ts.readyAccounts.Store(int32(i + 1))
			ts.metrics.AppendLog("[traffic] account %d/%d %s ready", i+1, target, acc.addr.Hex()[:10])
		}
	}

	// --- Phase 2: re-fund depleted (paused) accounts ---
	readyCount := int(ts.readyAccounts.Load())
	ts.accountsMu.RLock()
	accts := make([]*trafficAccount, 0)
	for _, a := range ts.accounts[1:readyCount] { // skip master (index 0)
		if a.paused.Load() {
			accts = append(accts, a)
		}
	}
	ts.accountsMu.RUnlock()

	for _, acc := range accts {
		select {
		case <-done:
			return
		default:
		}

		if !ts.fundOneAccount(acc, master, signer, gasMu, currentGasPrice, done) {
			return // retry next tick
		}

		// Re-sync nonce and unpause.
		nonce, nerr := ts.l2.PendingNonceAt(ts.ctx, acc.addr)
		if nerr != nil {
			ts.metrics.AppendLog("[traffic] nonce sync failed after re-fund for %s: %v", acc.addr.Hex()[:10], nerr)
			return // retry next tick
		}
		acc.nonce.Store(nonce)
		acc.paused.Store(false)
		ts.metrics.AppendLog("[traffic] account %s re-funded and unpaused (nonce=%d)", acc.addr.Hex()[:10], nonce)
	}
}

// fundOneAccount sends funds from master to acc and waits for the receipt.
// Returns true on success, false if funding failed (caller should retry later).
func (ts *TrafficSimulator) fundOneAccount(
	acc *trafficAccount,
	master *trafficAccount,
	signer types.Signer,
	gasMu *sync.RWMutex,
	currentGasPrice *big.Int,
	done <-chan struct{},
) bool {
	// Check if already has sufficient balance (deterministic keys survive restarts).
	bal, err := ts.l2.BalanceAt(ts.ctx, acc.addr, nil)
	if err != nil {
		ts.metrics.AppendLog("[traffic] balance check error for %s: %v", acc.addr.Hex()[:10], err)
		return false
	}

	gasMu.RLock()
	gp := new(big.Int).Set(currentGasPrice)
	gasMu.RUnlock()

	// Apply gas multiplier so funding txs also use the bumped price.
	gp = ts.applyGasMultiplier(gp)

	// Minimum balance: enough for 10,000 transfers at current gas price + tx value.
	perTxCost := new(big.Int).Mul(gp, big.NewInt(21000))
	if txVal := ts.getTxValue(); txVal != nil {
		perTxCost.Add(perTxCost, txVal)
	}
	minBalance := new(big.Int).Mul(perTxCost, big.NewInt(10000))

	if bal.Cmp(minBalance) >= 0 {
		// Already has sufficient balance. Sync nonce from chain.
		nonce, nerr := ts.l2.PendingNonceAt(ts.ctx, acc.addr)
		if nerr != nil {
			ts.metrics.AppendLog("[traffic] failed to sync nonce for %s: %v", acc.addr.Hex()[:10], nerr)
			return false // retry later
		}
		acc.nonce.Store(nonce)
		ts.metrics.AppendLog("[traffic] account %s already funded (bal=%s nonce=%d)",
			acc.addr.Hex()[:10], bal.String(), nonce)
		return true
	}

	// Fund from master account via L2 value transfer.
	// Send enough to top up to minBalance (subtract existing balance).
	fundAmount := new(big.Int).Sub(minBalance, bal)

	// Check master has enough balance before attempting to send.
	gasCost := new(big.Int).Mul(gp, big.NewInt(21000))
	txCost := new(big.Int).Add(fundAmount, gasCost)
	masterBal, mErr := ts.l2.BalanceAt(ts.ctx, master.addr, nil)
	if mErr != nil {
		ts.metrics.AppendLog("[traffic] master balance check error: %v", mErr)
		return false
	}
	if masterBal.Cmp(txCost) < 0 {
		ts.metrics.AppendLog("[traffic] master %s has insufficient balance to fund %s: have %s, need %s (deposit more L1->L2)",
			master.addr.Hex()[:10], acc.addr.Hex()[:10], masterBal.String(), txCost.String())
		return false
	}

	n := master.nonce.Add(1) - 1

	tx := types.NewTransaction(n, acc.addr, fundAmount, 21000, gp, nil)
	signed, signErr := types.SignTx(tx, signer, master.key)
	if signErr != nil {
		ts.metrics.AppendLog("[traffic] fund sign error: %v", signErr)
		master.nonce.Add(^uint64(0)) // rollback nonce claim
		return false
	}

	if sendErr := ts.l2.SendTransaction(ts.ctx, signed); sendErr != nil {
		ts.metrics.AppendLog("[traffic] fund send error for %s: %v",
			acc.addr.Hex()[:10], sendErr)
		master.nonce.Add(^uint64(0)) // rollback nonce claim
		// Re-sync master nonce.
		if freshNonce, nerr := ts.l2.PendingNonceAt(ts.ctx, master.addr); nerr == nil {
			master.nonce.Store(freshNonce)
		}
		return false
	}

	ts.metrics.RegisterFundingTx(signed.Hash())
	ts.metrics.AppendLog("[traffic] funding %s (amount=%s)",
		acc.addr.Hex()[:10], fundAmount.String())

	// Wait for receipt before marking funded (ensures balance is available).
	hash := signed.Hash()
	for attempt := 0; attempt < 60; attempt++ {
		select {
		case <-done:
			return false
		case <-time.After(500 * time.Millisecond):
		}
		receipt, rErr := ts.l2.TransactionReceipt(ts.ctx, hash)
		if rErr == nil && receipt != nil {
			// Sync child account nonce (may have txs from previous runs).
			nonce, nerr := ts.l2.PendingNonceAt(ts.ctx, acc.addr)
			if nerr != nil {
				ts.metrics.AppendLog("[traffic] failed to sync nonce for %s after funding: %v", acc.addr.Hex()[:10], nerr)
				return false
			}
			acc.nonce.Store(nonce)
			ts.metrics.AppendLog("[traffic] funded %s (nonce=%d)", acc.addr.Hex()[:10], nonce)
			return true
		}
	}

	ts.metrics.AppendLog("[traffic] fund timeout for %s", acc.addr.Hex()[:10])
	return false
}

func (ts *TrafficSimulator) waitForReceipt(txHash [32]byte, start time.Time) {
	hash := txHash
	for i := 0; i < 120; i++ {
		select {
		case <-ts.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}

		receipt, err := ts.l2.TransactionReceipt(ts.ctx, hash)
		if err == nil && receipt != nil {
			now := time.Now()
			latency := now.Sub(start).Seconds()
			ts.metrics.AddL2TxSpeed(now, latency)
			ts.metrics.RecordSimulatorConfirmation(now)
			return
		}
	}
}
