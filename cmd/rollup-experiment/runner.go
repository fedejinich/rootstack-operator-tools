package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/fedejinich/rootstack-operator-tools/cmd/rollup-monitor/monitor"
)

// ErrPreflight wraps configuration / environment errors detected before a run
// can begin (e.g. unfunded master account, unreachable RPC). When returned by
// Runner.Run, the outer sweep should abort rather than continue with other
// runs, since the same preflight will fail identically each time.
var ErrPreflight = errors.New("experiment preflight failed")

// rbtcToWei converts an RBTC amount (as float64) to a *big.Int in wei (1e18).
func rbtcToWei(rbtc float64) *big.Int {
	if rbtc <= 0 {
		return big.NewInt(0)
	}
	// Use big.Float for precision: rbtc * 1e18
	bf := new(big.Float).SetFloat64(rbtc)
	bf.Mul(bf, new(big.Float).SetFloat64(1e18))
	wei, _ := bf.Int(nil)
	return wei
}

// rbtcStr formats an RBTC float64 as a decimal string suitable for
// SetBridgeAmountRBTC / SetBridgeAmountUSDRIF (e.g. "0.001").
func rbtcStr(rbtc float64) string {
	if rbtc <= 0 {
		return "0"
	}
	// Use enough precision to represent small amounts accurately
	s := strconv.FormatFloat(rbtc, 'f', -1, 64)
	return s
}

// weiToRBTCFloat converts a wei amount to RBTC as a float64.
func weiToRBTCFloat(wei *big.Int) float64 {
	if wei == nil || wei.Sign() == 0 {
		return 0
	}
	f := new(big.Float).SetInt(wei)
	f.Quo(f, new(big.Float).SetFloat64(1e18))
	result, _ := f.Float64()
	return result
}

// FundingAction represents the user's choice when the L2 account needs funding.
type FundingAction int

const (
	FundingDeposit FundingAction = iota
	FundingQuit
)

// FundingInfo contains balance information shown to the user during a funding prompt.
type FundingInfo struct {
	Account         common.Address
	L2BalanceWei    *big.Int
	L1BalanceWei    *big.Int
	SuggestedAmount float64 // RBTC to deposit
	EstimatedNeed   float64 // total RBTC estimated for the run (0 if unknown)
}

// FundingPromptFn is called when the experiment detects that the L2 account needs
// funding. It presents balance info and returns whether to deposit (and how much)
// or quit the experiment.
type FundingPromptFn func(info FundingInfo) (FundingAction, float64)

// RunResult holds the result of a single experiment run.
type RunResult struct {
	RunID        int
	BatcherCfg   BatcherRunConfig
	CSVPath      string
	RowCount     int
	Duration     time.Duration
	FinalMetrics map[string]float64
}

// Runner executes a single experiment run: drives traffic via dynamic curves,
// collects metrics, and writes the enriched CSV output.
type Runner struct {
	runID             int
	logger            *slog.Logger
	target            TargetConfig
	sim               *ResolvedSimConfig
	batcher           BatcherRunConfig
	outputDir         string
	cleanStart        bool   // true if run started with fully drained batcher
	startUnsafeBlocks uint64 // L2 unsafe head block at run start (set on first sample)

	// sampleCh, when non-nil, receives each ExperimentSample as it is produced.
	// Used by --live mode to feed the analysis TUI in real time.
	sampleCh chan<- ExperimentSample

	// fundingPrompt, when non-nil, is called instead of aborting when the L2
	// account has insufficient balance to send traffic.
	fundingPrompt FundingPromptFn

	// pendingFunding holds the state of an L1→L2 deposit kicked off during
	// preflight. When non-nil, Run waits for it to land on L2 before doing
	// any work that requires a non-zero master L2 balance (e.g. ERC20 deploy).
	pendingFunding *pendingFunding

	// Adaptive rate controller state (persists across sample ticks).
	adaptiveLastDecision time.Time // when the last AIMD decision was made
	adaptivePendingEMA   float64   // exponential moving average of txpool_pending
	adaptiveEMAInit      bool      // true after the first pending sample
}

// NewRunner creates a runner for one experiment run.
// cleanStart indicates if the run started with a fully drained batcher
// (sequencer was paused and all pending data was posted).
func NewRunner(runID int, logger *slog.Logger, target TargetConfig, sim *ResolvedSimConfig, batcher BatcherRunConfig, outputDir string, cleanStart bool) *Runner {
	return &Runner{
		runID:      runID,
		logger:     logger,
		target:     target,
		sim:        sim,
		batcher:    batcher,
		outputDir:  outputDir,
		cleanStart: cleanStart,
	}
}

// Run executes the experiment. It blocks until the simulation length is reached
// or the context is cancelled.
func (r *Runner) Run(ctx context.Context) (*RunResult, error) {
	r.logger.Info("Starting experiment run",
		"run_id", r.runID,
		"max_l1_tx_size", r.batcher.MaxL1TxSize,
		"max_channel_duration", r.batcher.MaxChannelDuration,
		"length", r.sim.Length,
	)

	// Build a monitor.Config for the collector and traffic simulator
	monCfg := r.buildMonitorConfig()

	// Resolve chain parameters from rollup.json when not set by the experiment config,
	// so the CSV always records the actual runtime values.
	resolvedBlockTime := r.batcher.BlockTime
	if resolvedBlockTime == 0 && monCfg.Rollup != nil && monCfg.Rollup.BlockTime > 0 {
		resolvedBlockTime = monCfg.Rollup.BlockTime
		r.logger.Info("Resolved block_time from rollup.json", "block_time", resolvedBlockTime)
	}

	// Create metrics store
	metrics := monitor.NewMetricsStore(10000) // large buffer for experiments

	// Open CSV output, guarding against overwriting existing data.
	csvPath := fmt.Sprintf("%s/experiment_run_%d.csv", r.outputDir, r.runID)
	if _, err := os.Stat(csvPath); err == nil {
		maxID := MaxRunIDInDir(r.outputDir)
		newID := maxID + 1
		r.logger.Warn("CSV already exists, bumping run ID",
			"original", r.runID, "new", newID)
		r.runID = newID
		csvPath = fmt.Sprintf("%s/experiment_run_%d.csv", r.outputDir, r.runID)
	}
	csvFile, err := os.Create(csvPath)
	if err != nil {
		return nil, fmt.Errorf("create CSV file: %w", err)
	}
	defer csvFile.Close()

	csvWriter := csv.NewWriter(csvFile)
	defer csvWriter.Flush()

	// Write header
	if err := csvWriter.Write(ExperimentSampleCSVHeader()); err != nil {
		return nil, fmt.Errorf("write CSV header: %w", err)
	}

	// Initialize the collector
	collector, err := monitor.NewCollector(ctx, monCfg, metrics)
	if err != nil {
		r.logger.Warn("Collector init failed, metrics will be limited", "err", err)
	} else {
		go collector.Run()
	}

	// Initialize the traffic simulator. Fail hard if tx_rate > 0 but the
	// simulator cannot be created (e.g. bad L2 RPC, invalid private key).
	var traffic *monitor.TrafficSimulator
	if monCfg.PrivateKey != "" && (r.sim.TxRate.Start() > 0 || r.sim.TxRateAdaptive) {
		ts, err := monitor.NewTrafficSimulator(ctx, monCfg, metrics)
		if err != nil {
			return nil, fmt.Errorf("traffic simulator init failed (tx_rate > 0 but cannot send): %w", err)
		}
		traffic = ts
	}

	// Initialize ERC20 traffic simulator for L2 ERC20 transfer() calls.
	var erc20Traffic *monitor.ERC20TrafficSimulator
	if monCfg.PrivateKey != "" && (r.sim.ERC20TxRate.Start() > 0 || r.sim.ERC20TxRate.IsRandom() || !r.sim.ERC20TxRate.IsConstant()) {
		tokenAddr := monCfg.ERC20TrafficToken
		if tokenAddr == "" {
			tokenAddr = monCfg.USDRIFTokenL2
		}
		// Auto-deploy a self-contained fixed-supply ERC20 if none configured.
		// Entire supply is minted to the master key; the simulator then funds
		// derived accounts from master via direct L2 transfers — no L1 bridge.
		if tokenAddr == "" {
			r.logger.Info("No ERC20 token configured for L2 traffic; deploying generic fixed-supply ERC20 from master")
			masterAddr, addrErr := monitor.AddressFromKey(monCfg.PrivateKey)
			if addrErr != nil {
				return nil, fmt.Errorf("%w: ERC20 traffic enabled but master private key is invalid: %w", ErrPreflight, addrErr)
			}
			l2c, dialErr := ethclient.DialContext(ctx, r.target.L2RPC)
			if dialErr != nil {
				return nil, fmt.Errorf("%w: ERC20 traffic enabled but L2 dial failed for auto-deploy (%s): %w", ErrPreflight, r.target.L2RPC, dialErr)
			}
			if r.pendingFunding != nil {
				r.logger.Info("Waiting for preflight L2 deposit to land...", "account", masterAddr.Hex())
				newBal, werr := r.pendingFunding.WaitL2Credit(ctx, l2c, 5*time.Minute)
				if werr != nil {
					l2c.Close()
					return nil, fmt.Errorf("%w: preflight L1 deposit submitted but L2 credit never arrived: %w", ErrPreflight, werr)
				}
				r.logger.Info("Preflight L2 deposit credited",
					"account", masterAddr.Hex(),
					"balance_rbtc", fmt.Sprintf("%.6f", weiToRBTCFloat(newBal)),
				)
				r.pendingFunding = nil
			}
			bal, balErr := l2c.BalanceAt(ctx, masterAddr, nil)
			if balErr != nil {
				l2c.Close()
				return nil, fmt.Errorf("%w: ERC20 traffic enabled but could not read master L2 balance for %s: %w", ErrPreflight, masterAddr.Hex(), balErr)
			}
			if bal.Sign() == 0 {
				l2c.Close()
				return nil, fmt.Errorf("%w: ERC20 traffic enabled but master account %s still has zero L2 balance on %s — fund it (e.g. via bridge deposit) and retry", ErrPreflight, masterAddr.Hex(), r.target.L2RPC)
			}
			addr, depErr := monitor.DeployGenericERC20(ctx, l2c, monCfg.PrivateKey)
			l2c.Close()
			if depErr != nil {
				return nil, fmt.Errorf("%w: ERC20 auto-deploy failed (master %s, balance %s wei): %w", ErrPreflight, masterAddr.Hex(), bal.String(), depErr)
			}
			tokenAddr = addr.Hex()
			r.logger.Info("ERC20 auto-deployed", "address", tokenAddr, "master", masterAddr.Hex())
		}
		ets, err := monitor.NewERC20TrafficSimulator(ctx, monCfg, metrics, common.HexToAddress(tokenAddr))
		if err != nil {
			return nil, fmt.Errorf("%w: ERC20 traffic simulator init failed (token=%s): %w", ErrPreflight, tokenAddr, err)
		}
		erc20Traffic = ets
	}

	// Initialize swap traffic simulator for mock-swap router calls.
	var swapTraffic *monitor.SwapTrafficSimulator
	if monCfg.PrivateKey != "" && (r.sim.SwapTxRate.Start() > 0 || r.sim.SwapTxRate.IsRandom() || !r.sim.SwapTxRate.IsConstant()) {
		var env monitor.SwapEnv
		if r.sim.SwapRouter != "" && r.sim.SwapTokenA != "" && r.sim.SwapTokenB != "" {
			env = monitor.SwapEnv{
				Router: common.HexToAddress(r.sim.SwapRouter),
				TokenA: common.HexToAddress(r.sim.SwapTokenA),
				TokenB: common.HexToAddress(r.sim.SwapTokenB),
			}
			r.logger.Info("Using pre-deployed swap env",
				"router", env.Router.Hex(),
				"tokenA", env.TokenA.Hex(),
				"tokenB", env.TokenB.Hex())
		} else {
			r.logger.Info("No swap router configured; deploying MockSwapRouter + 2 ERC-20s from master")
			masterAddr, addrErr := monitor.AddressFromKey(monCfg.PrivateKey)
			if addrErr != nil {
				return nil, fmt.Errorf("%w: swap traffic enabled but master private key is invalid: %w", ErrPreflight, addrErr)
			}
			l2c, dialErr := ethclient.DialContext(ctx, r.target.L2RPC)
			if dialErr != nil {
				return nil, fmt.Errorf("%w: swap traffic enabled but L2 dial failed for auto-deploy (%s): %w", ErrPreflight, r.target.L2RPC, dialErr)
			}
			if r.pendingFunding != nil {
				r.logger.Info("Waiting for preflight L2 deposit to land...", "account", masterAddr.Hex())
				newBal, werr := r.pendingFunding.WaitL2Credit(ctx, l2c, 5*time.Minute)
				if werr != nil {
					l2c.Close()
					return nil, fmt.Errorf("%w: preflight L1 deposit submitted but L2 credit never arrived: %w", ErrPreflight, werr)
				}
				r.logger.Info("Preflight L2 deposit credited", "account", masterAddr.Hex(),
					"balance_rbtc", fmt.Sprintf("%.6f", weiToRBTCFloat(newBal)))
				r.pendingFunding = nil
			}
			bal, balErr := l2c.BalanceAt(ctx, masterAddr, nil)
			if balErr != nil {
				l2c.Close()
				return nil, fmt.Errorf("%w: swap traffic enabled but could not read master L2 balance: %w", ErrPreflight, balErr)
			}
			if bal.Sign() == 0 {
				l2c.Close()
				return nil, fmt.Errorf("%w: swap traffic enabled but master %s has zero L2 balance on %s — bridge-deposit and retry", ErrPreflight, masterAddr.Hex(), r.target.L2RPC)
			}
			deployed, depErr := monitor.DeploySwapEnv(ctx, l2c, monCfg.PrivateKey)
			l2c.Close()
			if depErr != nil {
				return nil, fmt.Errorf("%w: swap env auto-deploy failed (master %s, balance %s wei): %w", ErrPreflight, masterAddr.Hex(), bal.String(), depErr)
			}
			env = deployed
			r.logger.Info("Swap env auto-deployed",
				"router", env.Router.Hex(),
				"tokenA", env.TokenA.Hex(),
				"tokenB", env.TokenB.Hex())
		}
		sts, err := monitor.NewSwapTrafficSimulator(ctx, monCfg, metrics, env)
		if err != nil {
			return nil, fmt.Errorf("%w: swap traffic simulator init failed (router=%s): %w", ErrPreflight, env.Router.Hex(), err)
		}
		swapTraffic = sts
	}

	// Initialize L1 sender for bridging operations (deposits and withdrawals).
	var l1Sender *monitor.L1Sender
	needsBridge := func(c Curve) bool {
		return c.Start() > 0 || c.IsRandom() || !c.IsConstant()
	}
	if monCfg.PrivateKey != "" && (needsBridge(r.sim.DepositRBTCRate) || needsBridge(r.sim.DepositERC20Rate) ||
		needsBridge(r.sim.WithdrawRBTCRate) || needsBridge(r.sim.WithdrawERC20Rate)) {
		ls, err := monitor.NewL1Sender(ctx, monCfg, metrics)
		if err != nil {
			r.logger.Warn("L1 sender init failed, bridging will be disabled", "err", err)
		} else {
			l1Sender = ls
		}
	}

	// Start timing
	startTime := time.Now()
	sampleTicker := time.NewTicker(r.sim.SampleInterval)
	defer sampleTicker.Stop()

	// Accumulators for dynamic bridge scheduling.
	// Each tick, we accumulate rate * tickDuration and fire an operation when >= 1.0.
	var depositRBTCAccum, depositERC20Accum float64
	var withdrawRBTCAccum, withdrawERC20Accum float64

	// Pre-fund master L2 account if balance is insufficient for the run.
	if traffic != nil {
		if err := r.prefundForRun(ctx, monCfg, metrics); err != nil {
			return nil, err
		}
	}

	// Start initial traffic rate
	initialRate := r.sim.TxRate.Start()
	if traffic != nil && initialRate > 0 {
		traffic.SetNumAccounts(r.sim.Accounts)
		traffic.Start(initialRate)
		if r.sim.TxRateAdaptive {
			r.logger.Info("Traffic started (adaptive max-pressure mode)",
				"seed_rate", initialRate,
				"target_pending", r.sim.TxRateTargetPending,
				"max_rate", r.sim.TxRateMaxRate,
				"accounts", r.sim.Accounts,
			)
		} else {
			r.logger.Info("Traffic started", "rate", initialRate, "accounts", r.sim.Accounts)
		}
	}

	// Start ERC20 traffic
	initialERC20Rate := r.sim.ERC20TxRate.Start()
	if erc20Traffic != nil && initialERC20Rate > 0 {
		erc20Traffic.SetNumAccounts(r.sim.Accounts)
		erc20Traffic.Start(initialERC20Rate)
		r.logger.Info("ERC20 traffic started", "rate", initialERC20Rate, "accounts", r.sim.Accounts)
	}

	// Start swap traffic
	initialSwapRate := r.sim.SwapTxRate.Start()
	if swapTraffic != nil && initialSwapRate > 0 {
		swapTraffic.SetNumAccounts(r.sim.Accounts)
		swapTraffic.Start(initialSwapRate)
		r.logger.Info("Swap traffic started", "rate", initialSwapRate, "accounts", r.sim.Accounts)
	}

	rowCount := 0
	var lastRate float64
	var lastERC20Rate float64
	var lastSwapRate float64

	// Determine effective duration for curve interpolation progress.
	// If any condition is time-based, use the shortest time duration.
	// Otherwise fall back to a generous upper bound.
	effectiveDuration := 24 * time.Hour
	for _, cond := range r.sim.Length {
		if cond.Mode == LengthTime && cond.Duration < effectiveDuration {
			effectiveDuration = cond.Duration
		}
	}

	startL1Block := uint64(0)
	startL2Block := uint64(0)
	startBatcherPosts := uint64(0)
	startBatcherBlocks := uint64(0)

	// Funding prompt state: tracks when funding was last resolved so the
	// grace period resets, and limits prompts to avoid infinite loops.
	var lastFundingTime time.Time
	var fundingAttempts int
	const maxFundingAttempts = 3

	// Track MetricsStore log position so we can relay new entries to the
	// experiment Action Log (makes [traffic] errors visible in the TUI).
	lastLogIdx := metrics.LogLen()

	// Latest knob values for CSV emission (updated each internal tick).
	var latestTxValue float64
	var latestDepositRBTCRate, latestDepositRBTCValue float64
	var latestDepositERC20Rate, latestDepositERC20Value float64
	var latestWithdrawRBTCRate, latestWithdrawRBTCValue float64
	var latestWithdrawERC20Rate, latestWithdrawERC20Value float64

	var combinedWriter *csv.Writer

	// emitSample builds an ExperimentSample from the latest state and writes it to CSV.
	emitSample := func(mv map[string]float64) {
		elapsed := time.Since(startTime)

		totalBatcherCost := mv["total_batcher_cost_rbtc"]
		totalL2Txs := mv["total_l2_txs"]
		amortizedL1Cost := float64(0)
		if totalL2Txs >= 1 && totalBatcherCost > 0 {
			amortizedL1Cost = totalBatcherCost / totalL2Txs
		}

		totalBatcherDataBytes := mv["total_batcher_data_bytes"]
		compressionRatio := mv["compression_ratio_delta"]
		var compressedPerTxKB, rawPerTxKB float64
		if totalL2Txs >= 1 && totalBatcherDataBytes > 0 && compressionRatio > 0 {
			compressedPerTxKB = (totalBatcherDataBytes / totalL2Txs) / 1024.0
			rawPerTxKB = compressedPerTxKB / compressionRatio
		}

		normalizedTPS := 0.0
		if lastRate >= 1.0 {
			normalizedTPS = mv["l2_to_l1_throughput"] / lastRate
		}

		dataThroughputBytesPerS := 0.0
		if elapsedS := elapsed.Seconds(); elapsedS > 0 && totalBatcherDataBytes > 0 && totalL2Txs >= 1 {
			dataThroughputBytesPerS = totalBatcherDataBytes / elapsedS
		}

		sample := ExperimentSample{
			RunID:     r.runID,
			TSeconds:  elapsed.Seconds(),
			Timestamp: time.Now(),
			Knobs: SampleKnobs{
				TxRate:                lastRate,
				TxValue:               latestTxValue,
				Accounts:              r.sim.Accounts,
				DepositRBTCRateS:      latestDepositRBTCRate,
				DepositRBTCValue:      latestDepositRBTCValue,
				DepositERC20RateS:     latestDepositERC20Rate,
				DepositERC20Value:     latestDepositERC20Value,
				WithdrawRBTCRateS:     latestWithdrawRBTCRate,
				WithdrawRBTCValue:     latestWithdrawRBTCValue,
				WithdrawERC20RateS:    latestWithdrawERC20Rate,
				WithdrawERC20Value:    latestWithdrawERC20Value,
				MaxL1TxSize:           r.batcher.MaxL1TxSize,
				MaxChannelDuration:    r.batcher.MaxChannelDuration,
				MaxBlocksPerSpanBatch: r.batcher.MaxBlocksPerSpanBatch,
				MaxPendingTx:          r.batcher.MaxPendingTx,
				GasLimit:              r.batcher.GasLimit,
				BlockTime:             resolvedBlockTime,
				CleanStart:            r.cleanStart,
				StartUnsafeBlocks:     r.startUnsafeBlocks,
			},
			Derived: DerivedMetrics{
				AmortizedL1CostPerL2Tx:  amortizedL1Cost,
				TotalTxCostRBTC:         mv["l2_tx_cost_rbtc"] + amortizedL1Cost,
				TotalTxCostMinRBTC:      mv["l2_tx_cost_min_rbtc"] + amortizedL1Cost,
				TotalTxCostMaxRBTC:      mv["l2_tx_cost_max_rbtc"] + amortizedL1Cost,
				CompressedDataPerTxKB:   compressedPerTxKB,
				RawDataPerTxKB:          rawPerTxKB,
				NormalizedTPS:           normalizedTPS,
				DataThroughputBytesPerS: dataThroughputBytesPerS,
			},
		}
		sample.FromMetricValues(mv)

		row := sample.ToCSVRow()
		if err := csvWriter.Write(row); err != nil {
			r.logger.Error("Failed to write CSV row", "err", err)
		}
		csvWriter.Flush()
		if combinedWriter != nil {
			if err := combinedWriter.Write(row); err != nil {
				r.logger.Error("Failed to append combined.csv row", "err", err)
			} else {
				combinedWriter.Flush()
			}
		}
		rowCount++

		// Send sample to live analysis TUI if active.
		if r.sampleCh != nil {
			select {
			case r.sampleCh <- sample:
			default:
			}
		}

		// Log progress periodically
		if rowCount%30 == 0 {
			progress := float64(elapsed) / float64(effectiveDuration)
			logArgs := []any{
				"run_id", r.runID,
				"elapsed", elapsed.Round(time.Second),
				"progress", fmt.Sprintf("%.1f%%", progress*100),
				"rows", rowCount,
				"tps", fmt.Sprintf("%.1f", mv["l2_to_l1_throughput"]),
				"tx_rate", fmt.Sprintf("%g", lastRate),
			}
			r.logger.Info("Experiment progress", logArgs...)
		}
	}

	combinedPath := filepath.Join(r.outputDir, "combined.csv")
	combinedFile, needCombinedHeader, errCombined := openCombinedCSVForIncrementalAppend(combinedPath)
	if errCombined != nil {
		r.logger.Warn("combined.csv incremental append disabled", "path", combinedPath, "err", errCombined)
	} else {
		combinedWriter = csv.NewWriter(combinedFile)
		if needCombinedHeader {
			if err := combinedWriter.Write(ExperimentSampleCSVHeader()); err != nil {
				r.logger.Warn("combined.csv header write failed", "err", err)
			}
			combinedWriter.Flush()
		}
		defer func() {
			combinedWriter.Flush()
			combinedFile.Close()
		}()
	}

	// L1 block notification channel (nil if not in L1 block sample mode).
	var l1BlockCh <-chan struct{}
	if r.sim.SampleOnL1Block {
		l1BlockCh = metrics.L1BlockCh()
	}

	for {
		select {
		case <-ctx.Done():
			r.logger.Info("Experiment run cancelled", "run_id", r.runID)
			goto done

		case <-l1BlockCh:
			// In L1 block sample mode: emit a CSV row for each new L1 block.
			mv := metrics.MetricValues()
			emitSample(mv)

		case <-sampleTicker.C:
			elapsed := time.Since(startTime)

			// Collect current metric values (used by adaptive controller and CSV output).
			mv := metrics.MetricValues()

			// Evaluate simulation curves and apply dynamic values.
			// In adaptive mode, the AIMD controller overrides the curve.
			var currentRate float64
			if r.sim.TxRateAdaptive && traffic != nil {
				currentRate = r.adaptiveRate(lastRate, mv, traffic)
			} else {
				currentRate = r.sim.TxRate.AtTime(elapsed, effectiveDuration)
			}
			if traffic != nil && currentRate != lastRate {
				if currentRate > 0 && lastRate == 0 {
					traffic.Start(currentRate)
				} else if currentRate == 0 && lastRate > 0 {
					traffic.Stop()
				} else if currentRate > 0 {
					traffic.SetRate(currentRate)
				}
				lastRate = currentRate
			}

			// Evaluate tx value curve and apply to traffic simulator
			currentTxValue := r.sim.TxValue.AtTime(elapsed, effectiveDuration)
			if traffic != nil {
				traffic.SetTxValue(rbtcToWei(currentTxValue))
			}

			// Evaluate ERC20 traffic rate and value curves
			currentERC20Rate := r.sim.ERC20TxRate.AtTime(elapsed, effectiveDuration)
			if erc20Traffic != nil && currentERC20Rate != lastERC20Rate {
				if currentERC20Rate > 0 && lastERC20Rate == 0 {
					erc20Traffic.Start(currentERC20Rate)
				} else if currentERC20Rate == 0 && lastERC20Rate > 0 {
					erc20Traffic.Stop()
				} else if currentERC20Rate > 0 {
					erc20Traffic.SetRate(currentERC20Rate)
				}
				lastERC20Rate = currentERC20Rate
			}
			currentSwapRate := r.sim.SwapTxRate.AtTime(elapsed, effectiveDuration)
			if swapTraffic != nil && currentSwapRate != lastSwapRate {
				if currentSwapRate > 0 && lastSwapRate == 0 {
					swapTraffic.Start(currentSwapRate)
				} else if currentSwapRate == 0 && lastSwapRate > 0 {
					swapTraffic.Stop()
				} else {
					swapTraffic.SetRate(currentSwapRate)
				}
				lastSwapRate = currentSwapRate
			}
			currentSwapTxValue := r.sim.SwapTxValue.AtTime(elapsed, effectiveDuration)
			if swapTraffic != nil {
				swapTraffic.SetTxValue(big.NewInt(int64(currentSwapTxValue)))
			}

			currentERC20TxValue := r.sim.ERC20TxValue.AtTime(elapsed, effectiveDuration)
			if erc20Traffic != nil {
				erc20Traffic.SetTxValue(big.NewInt(int64(currentERC20TxValue)))
			}

			// Evaluate bridge rate and value curves (deposits)
			currentDepositRBTCRate := r.sim.DepositRBTCRate.AtTime(elapsed, effectiveDuration)
			currentDepositERC20Rate := r.sim.DepositERC20Rate.AtTime(elapsed, effectiveDuration)
			currentDepositRBTCValue := r.sim.DepositRBTCValue.AtTime(elapsed, effectiveDuration)
			currentDepositERC20Value := r.sim.DepositERC20Value.AtTime(elapsed, effectiveDuration)

			// Evaluate bridge rate and value curves (withdrawals)
			currentWithdrawRBTCRate := r.sim.WithdrawRBTCRate.AtTime(elapsed, effectiveDuration)
			currentWithdrawERC20Rate := r.sim.WithdrawERC20Rate.AtTime(elapsed, effectiveDuration)
			currentWithdrawRBTCValue := r.sim.WithdrawRBTCValue.AtTime(elapsed, effectiveDuration)
			currentWithdrawERC20Value := r.sim.WithdrawERC20Value.AtTime(elapsed, effectiveDuration)

			// Dynamic deposit scheduling via accumulator.
			// Each tick accumulates rate * tickDuration; fire operation when >= 1.0.
			tickSeconds := r.sim.SampleInterval.Seconds()
			if l1Sender != nil && currentDepositRBTCRate > 0 {
				l1Sender.SetBridgeAmountRBTC(rbtcStr(currentDepositRBTCValue))
				depositRBTCAccum += currentDepositRBTCRate * tickSeconds
				for depositRBTCAccum >= 1.0 {
					depositRBTCAccum -= 1.0
					go func() {
						r.logger.Info("Triggering RBTC deposit", "rate", currentDepositRBTCRate)
						l1Sender.Deposit()
					}()
				}
			}
			if l1Sender != nil && currentDepositERC20Rate > 0 {
				if currentDepositERC20Value > 0 {
					l1Sender.SetBridgeAmountUSDRIF(rbtcStr(currentDepositERC20Value))
				}
				depositERC20Accum += currentDepositERC20Rate * tickSeconds
				for depositERC20Accum >= 1.0 {
					depositERC20Accum -= 1.0
					go func() {
						r.logger.Info("Triggering ERC20 deposit", "rate", currentDepositERC20Rate)
						l1Sender.DepositUSDRIF()
					}()
				}
			}

			// Dynamic withdrawal scheduling via accumulator.
			if l1Sender != nil && currentWithdrawRBTCRate > 0 {
				l1Sender.SetBridgeAmountRBTC(rbtcStr(currentWithdrawRBTCValue))
				withdrawRBTCAccum += currentWithdrawRBTCRate * tickSeconds
				for withdrawRBTCAccum >= 1.0 {
					withdrawRBTCAccum -= 1.0
					go func() {
						r.logger.Info("Triggering RBTC withdrawal", "rate", currentWithdrawRBTCRate)
						l1Sender.Withdraw()
					}()
				}
			}
			if l1Sender != nil && currentWithdrawERC20Rate > 0 {
				if currentWithdrawERC20Value > 0 {
					l1Sender.SetBridgeAmountUSDRIF(rbtcStr(currentWithdrawERC20Value))
				}
				withdrawERC20Accum += currentWithdrawERC20Rate * tickSeconds
				for withdrawERC20Accum >= 1.0 {
					withdrawERC20Accum -= 1.0
					go func() {
						r.logger.Info("Triggering ERC20 withdrawal", "rate", currentWithdrawERC20Rate)
						l1Sender.WithdrawUSDRIF()
					}()
				}
			}

			// Track start values for length-based stopping
			if startL1Block == 0 && mv["latest_l1_block"] > 0 {
				startL1Block = uint64(mv["latest_l1_block"])
			}
			if startL2Block == 0 && mv["latest_l2_block"] > 0 {
				startL2Block = uint64(mv["latest_l2_block"])
				r.startUnsafeBlocks = startL2Block
			}
			if startBatcherPosts == 0 && mv["total_batcher_posts"] > 0 {
				startBatcherPosts = uint64(mv["total_batcher_posts"])
			}
			if startBatcherBlocks == 0 && mv["total_batcher_blocks"] > 0 {
				startBatcherBlocks = uint64(mv["total_batcher_blocks"])
			}

			// Update latest knob values for emitSample closure.
			latestTxValue = currentTxValue
			latestDepositRBTCRate = currentDepositRBTCRate
			latestDepositRBTCValue = currentDepositRBTCValue
			latestDepositERC20Rate = currentDepositERC20Rate
			latestDepositERC20Value = currentDepositERC20Value
			latestWithdrawRBTCRate = currentWithdrawRBTCRate
			latestWithdrawRBTCValue = currentWithdrawRBTCValue
			latestWithdrawERC20Rate = currentWithdrawERC20Rate
			latestWithdrawERC20Value = currentWithdrawERC20Value

			// Emit CSV sample (only on internal tick when not in L1 block mode).
			if !r.sim.SampleOnL1Block {
				emitSample(mv)
			}

			// Relay new MetricsStore log entries (e.g. [traffic] errors) to the Action Log.
			// Also detect chronic account depletion that signals master needs funding.
			masterInsufficientBalance := false
			if newLen := metrics.LogLen(); newLen > lastLogIdx {
				for _, entry := range metrics.LogEntriesSince(lastLogIdx) {
					r.logger.Info(entry.Message)
					if strings.Contains(entry.Message, "insufficient balance") && strings.Contains(entry.Message, "master") {
						masterInsufficientBalance = true
					}
				}
				lastLogIdx = newLen
			}

			// pauseAndFund stops traffic, runs the funding prompt, and restarts
			// traffic on success. Returns true when funding succeeded and the
			// experiment should continue from the next sample tick.
			pauseAndFund := func(reason string) (bool, error) {
				traffic.Stop()
				if swapTraffic != nil {
					swapTraffic.Stop()
				}
				if erc20Traffic != nil {
					erc20Traffic.Stop()
				}
				r.logger.Warn(reason, "l2_balance_rbtc", mv["l2_balance_rbtc"])

				funded, ferr := r.checkAndPromptFunding(ctx, monCfg, metrics)
				if ferr != nil {
					return false, ferr
				}
				if funded {
					cr := currentRate
					if !r.sim.TxRateAdaptive {
						cr = r.sim.TxRate.AtTime(elapsed, effectiveDuration)
					}
					if cr > 0 {
						traffic.SetNumAccounts(r.sim.Accounts)
						traffic.Start(cr)
						lastRate = cr
					}
					er := r.sim.ERC20TxRate.AtTime(elapsed, effectiveDuration)
					if erc20Traffic != nil && er > 0 {
						erc20Traffic.SetNumAccounts(r.sim.Accounts)
						erc20Traffic.Start(er)
					}
					sr := r.sim.SwapTxRate.AtTime(elapsed, effectiveDuration)
					if swapTraffic != nil && sr > 0 {
						swapTraffic.SetNumAccounts(r.sim.Accounts)
						swapTraffic.Start(sr)
						lastERC20Rate = er
					}
					lastFundingTime = time.Now()
					fundingAttempts++
				}
				return funded, nil
			}

			// Proactive funding: trigger when the master account can no longer
			// fund workers (logged by traffic simulator) or the L2 balance is
			// critically low while traffic is supposed to be running.
			l2BalLow := traffic != nil && currentRate > 0 && mv["l2_balance_rbtc"] < 0.01 && elapsed > 30*time.Second
			if (masterInsufficientBalance || l2BalLow) && r.fundingPrompt != nil && fundingAttempts < maxFundingAttempts {
				if ok, err := pauseAndFund("L2 account balance low — pausing experiment for funding"); err != nil {
					return nil, err
				} else if ok {
					continue
				}
			}

			// Abort early if traffic is supposed to be running but no L2 txs have landed.
			// Grace period scales with rate: for low rates like 0.042 tx/s we need
			// longer to accumulate visible transactions (wait for ~3 expected tx + confirmation).
			minGracePeriod := 60 * time.Second
			trafficGracePeriod := minGracePeriod
			if currentRate > 0 && currentRate < 1 {
				expectedTxTime := time.Duration(3/currentRate) * time.Second
				confirmationBuffer := 30 * time.Second
				trafficGracePeriod = expectedTxTime + confirmationBuffer
				if trafficGracePeriod < minGracePeriod {
					trafficGracePeriod = minGracePeriod
				}
			}
			graceStart := startTime
			if !lastFundingTime.IsZero() {
				graceStart = lastFundingTime
			}
			if traffic != nil && time.Since(graceStart) > trafficGracePeriod && mv["total_l2_txs"] == 0 {
				if r.fundingPrompt != nil && fundingAttempts < maxFundingAttempts {
					if ok, err := pauseAndFund("No L2 transactions after grace period — checking if account needs funding"); err != nil {
						return nil, err
					} else if ok {
						continue
					}
				}
				r.logger.Error("No L2 transactions observed after grace period",
					"grace_period", trafficGracePeriod,
					"tx_rate", currentRate,
				)
				return nil, fmt.Errorf("no L2 transactions after %s (traffic simulator running at rate=%g but tps=0; check L2 account funding and connectivity)", trafficGracePeriod, currentRate)
			}

			// Check stopping conditions (OR: any condition met triggers stop)
			if r.shouldStop(elapsed, rowCount, startL1Block, startL2Block, startBatcherPosts, startBatcherBlocks, mv) {
				r.logger.Info("Experiment run completed",
					"run_id", r.runID,
					"rows", rowCount,
					"elapsed", elapsed.Round(time.Second),
				)
				goto done
			}
		}

	}

done:
	// Stop traffic
	if traffic != nil {
		traffic.Stop()
	}
	if erc20Traffic != nil {
		erc20Traffic.Stop()
	}
	if swapTraffic != nil {
		swapTraffic.Stop()
	}

	finalMetrics := metrics.MetricValues()
	duration := time.Since(startTime)

	resolvedCfg := r.batcher
	resolvedCfg.BlockTime = resolvedBlockTime
	return &RunResult{
		RunID:        r.runID,
		BatcherCfg:   resolvedCfg,
		CSVPath:      csvPath,
		RowCount:     rowCount,
		Duration:     duration,
		FinalMetrics: finalMetrics,
	}, nil
}

// adaptiveRate implements the AIMD (additive increase, multiplicative decrease)
// rate controller for adaptive max-pressure mode. It reads txpool_pending from
// the current metrics and adjusts the send rate to keep the mempool near the
// configured target without flooding it.
//
// To avoid oscillations caused by the sample interval being much shorter than
// the block time, the AIMD decision is throttled to fire at most once per
// adaptiveDefaultDecisionInterval. Between decisions the previous rate is held.
// The raw pending signal is smoothed with an EMA to dampen stale-metric noise.
//
// When the traffic simulator is auto-paused due to underpriced errors, the
// controller backs off immediately (bypassing the throttle) and automatically
// resumes with a bumped gas price.
func (r *Runner) adaptiveRate(prevRate float64, mv map[string]float64, traffic *monitor.TrafficSimulator) float64 {
	target := float64(r.sim.TxRateTargetPending)
	maxRate := r.sim.TxRateMaxRate
	rawPending := mv["txpool_pending"]

	rate := prevRate
	if rate <= 0 {
		rate = adaptiveSeedRate
	}

	// Handle gas-pause immediately (bypasses throttle).
	if traffic != nil && traffic.IsPausedForGas() {
		rate = math.Max(rate*0.3, 1)
		mul := traffic.GasMultiplier()
		traffic.SetGasMultiplier(mul * 1.1)
		traffic.ResumeFromGasPause()
		r.logger.Info("Adaptive: auto-resumed from gas pause",
			"new_rate", fmt.Sprintf("%g", rate),
			"gas_multiplier", fmt.Sprintf("%.2f", mul*1.1),
		)
		return rate
	}

	// Update the EMA of pending on every tick (even when we skip the decision).
	if !r.adaptiveEMAInit {
		r.adaptivePendingEMA = rawPending
		r.adaptiveEMAInit = true
	} else {
		r.adaptivePendingEMA = adaptiveEMAlpha*rawPending + (1-adaptiveEMAlpha)*r.adaptivePendingEMA
	}

	// Throttle: only make an AIMD decision once per decision interval.
	now := time.Now()
	if !r.adaptiveLastDecision.IsZero() && now.Sub(r.adaptiveLastDecision) < adaptiveDefaultDecisionInterval {
		return rate
	}
	r.adaptiveLastDecision = now

	pending := r.adaptivePendingEMA

	switch {
	case pending < target*0.5:
		rate = math.Min(rate*1.3, maxRate)
	case pending < target:
		rate = math.Min(rate*1.1, maxRate)
	case pending > target*2:
		rate = math.Max(rate*0.5, 1)
	case pending > target:
		rate = math.Max(rate*0.8, 1)
	}

	return rate
}

// estimateDuration returns a conservative estimate of the run duration based
// on the configured stopping conditions. Used for pre-funding calculations.
func (r *Runner) estimateDuration() time.Duration {
	est := 2 * time.Hour
	for _, cond := range r.sim.Length {
		var d time.Duration
		switch cond.Mode {
		case LengthTime:
			d = cond.Duration
		case LengthDataPoints:
			d = time.Duration(cond.Count) * r.sim.SampleInterval
		case LengthL2Blocks:
			d = time.Duration(cond.Count) * 2 * time.Second
		case LengthL1Blocks:
			d = time.Duration(cond.Count) * 30 * time.Second
		case LengthBatcherPosts:
			d = time.Duration(cond.Count) * 2 * time.Minute
		case LengthBatcherBlocks:
			d = time.Duration(cond.Count) * 30 * time.Second
		}
		if d > 0 && d < est {
			est = d
		}
	}
	return est
}

// prefundForRun estimates how much RBTC the master L2 account needs for the
// full experiment run. If the current balance is insufficient, it invokes the
// funding prompt and deposits from L1 before traffic begins.
func (r *Runner) prefundForRun(ctx context.Context, monCfg *monitor.Config, metrics *monitor.MetricsStore) error {
	if r.fundingPrompt == nil || monCfg.PrivateKey == "" {
		return nil
	}

	pk, err := crypto.HexToECDSA(monCfg.PrivateKey)
	if err != nil {
		return nil
	}
	account := crypto.PubkeyToAddress(pk.PublicKey)

	l2Client, err := ethclient.DialContext(ctx, r.target.L2RPC)
	if err != nil {
		r.logger.Warn("Cannot check L2 balance for pre-funding", "err", err)
		return nil
	}
	defer l2Client.Close()

	l2Bal, err := l2Client.BalanceAt(ctx, account, nil)
	if err != nil {
		r.logger.Warn("Cannot query L2 balance", "err", err)
		return nil
	}

	gasPrice, err := l2Client.SuggestGasPrice(ctx)
	if err != nil {
		r.logger.Warn("Cannot query L2 gas price for estimation", "err", err)
		return nil
	}

	// Conservative estimate: peak_rate × duration × (gas_per_tx + tx_value) × 1.5 buffer.
	peakRate := r.sim.TxRate.PeakValue()
	if r.sim.TxRateAdaptive {
		peakRate = r.sim.TxRateMaxRate
	}
	if peakRate <= 0 {
		return nil
	}
	durationS := r.estimateDuration().Seconds()

	totalTxs := peakRate * durationS
	gasCostPerTx := new(big.Int).Mul(gasPrice, big.NewInt(21000))
	txValueWei := rbtcToWei(r.sim.TxValue.PeakValue())
	costPerTx := new(big.Int).Add(gasCostPerTx, txValueWei)

	totalCostF := new(big.Float).Mul(
		new(big.Float).SetFloat64(totalTxs),
		new(big.Float).SetInt(costPerTx),
	)
	totalCostF.Mul(totalCostF, new(big.Float).SetFloat64(1.5))
	required, _ := totalCostF.Int(nil)

	deficit := new(big.Int).Sub(required, l2Bal)
	if deficit.Sign() <= 0 {
		r.logger.Info("L2 balance sufficient for estimated run",
			"balance_rbtc", fmt.Sprintf("%.6f", weiToRBTCFloat(l2Bal)),
			"estimated_need_rbtc", fmt.Sprintf("%.6f", weiToRBTCFloat(required)),
		)
		return nil
	}

	estimatedNeed := weiToRBTCFloat(required)
	depositAmount := weiToRBTCFloat(deficit)
	if depositAmount < 0.01 {
		depositAmount = 0.01
	}

	r.logger.Warn("L2 balance insufficient for estimated run",
		"balance_rbtc", fmt.Sprintf("%.6f", weiToRBTCFloat(l2Bal)),
		"estimated_need_rbtc", fmt.Sprintf("%.6f", estimatedNeed),
		"deficit_rbtc", fmt.Sprintf("%.6f", depositAmount),
	)

	// Check L1 balance
	var l1Bal *big.Int
	if r.target.L1RPC != "" {
		if l1c, err := ethclient.DialContext(ctx, r.target.L1RPC); err == nil {
			defer l1c.Close()
			if bal, err := l1c.BalanceAt(ctx, account, nil); err == nil {
				l1Bal = bal
			}
		}
	}
	if l1Bal == nil {
		l1Bal = big.NewInt(0)
	}

	info := FundingInfo{
		Account:         account,
		L2BalanceWei:    l2Bal,
		L1BalanceWei:    l1Bal,
		SuggestedAmount: depositAmount,
		EstimatedNeed:   estimatedNeed,
	}

	action, amount := r.fundingPrompt(info)
	if action == FundingQuit {
		return fmt.Errorf("experiment stopped: L2 account %s needs %.4f RBTC (have %.4f)",
			account.Hex(), estimatedNeed, weiToRBTCFloat(l2Bal))
	}
	if amount <= 0 {
		amount = depositAmount
	}

	r.logger.Info("Depositing RBTC for experiment run", "amount_rbtc", amount)

	depositCfg := r.buildMonitorConfig()
	depositCfg.BridgeAmountRBTC = rbtcStr(amount)

	l1Sender, err := monitor.NewL1Sender(ctx, depositCfg, metrics)
	if err != nil {
		return fmt.Errorf("create L1 sender for pre-funding: %w", err)
	}

	l1Sender.Deposit()

	r.logger.Info("L1 deposit confirmed. Waiting for L2 balance to arrive...")
	balBefore := new(big.Int).Set(l2Bal)
	deadline := time.After(5 * time.Minute)
	pollTick := time.NewTicker(5 * time.Second)
	defer pollTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			r.logger.Warn("Timeout waiting for L2 deposit; continuing anyway")
			return nil
		case <-pollTick.C:
			newBal, err := l2Client.BalanceAt(ctx, account, nil)
			if err != nil {
				continue
			}
			if newBal.Cmp(balBefore) > 0 {
				r.logger.Info("L2 deposit arrived",
					"balance_rbtc", fmt.Sprintf("%.6f", weiToRBTCFloat(newBal)),
				)
				return nil
			}
		}
	}
}

// checkAndPromptFunding checks the L2 master account balance and, if it is
// below a funding threshold, invokes the configured fundingPrompt callback.
// On user confirmation it deposits RBTC from L1 and waits for the L2 balance
// to arrive. Returns (true, nil) when funding succeeded, (false, nil) when the
// issue is not funding-related, or (false, err) when the user chose to quit.
func (r *Runner) checkAndPromptFunding(ctx context.Context, monCfg *monitor.Config, metrics *monitor.MetricsStore) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}

	pk := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r.target.PrivateKey), "0x"))
	if pk == "" {
		return false, nil
	}
	key, err := crypto.HexToECDSA(pk)
	if err != nil {
		return false, nil
	}
	account := crypto.PubkeyToAddress(key.PublicKey)

	l2Client, err := ethclient.DialContext(ctx, r.target.L2RPC)
	if err != nil {
		r.logger.Warn("Cannot check L2 balance for funding prompt", "err", err)
		return false, nil
	}
	defer l2Client.Close()

	l2Bal, err := l2Client.BalanceAt(ctx, account, nil)
	if err != nil {
		r.logger.Warn("Cannot query L2 balance", "err", err)
		return false, nil
	}

	// 0.01 RBTC threshold — above this the issue is likely not funding.
	threshold := new(big.Int).Mul(big.NewInt(1e16), big.NewInt(1))
	if l2Bal.Cmp(threshold) >= 0 {
		return false, nil
	}

	var l1Bal *big.Int
	if r.target.L1RPC != "" {
		if l1c, err := ethclient.DialContext(ctx, r.target.L1RPC); err == nil {
			defer l1c.Close()
			if bal, err := l1c.BalanceAt(ctx, account, nil); err == nil {
				l1Bal = bal
			}
		}
	}
	if l1Bal == nil {
		l1Bal = big.NewInt(0)
	}

	suggestedAmount := 0.1
	l1RBTC := weiToRBTCFloat(l1Bal)
	if l1RBTC < suggestedAmount && l1RBTC > 0.001 {
		suggestedAmount = l1RBTC * 0.5
	}

	info := FundingInfo{
		Account:         account,
		L2BalanceWei:    l2Bal,
		L1BalanceWei:    l1Bal,
		SuggestedAmount: suggestedAmount,
	}

	action, amount := r.fundingPrompt(info)
	if action == FundingQuit {
		return false, fmt.Errorf("experiment stopped: L2 account %s needs funding", account.Hex())
	}
	if amount <= 0 {
		amount = suggestedAmount
	}

	r.logger.Info("Initiating L1 → L2 deposit", "amount_rbtc", amount, "account", account.Hex()[:10])

	depositCfg := r.buildMonitorConfig()
	depositCfg.BridgeAmountRBTC = rbtcStr(amount)

	l1Sender, err := monitor.NewL1Sender(ctx, depositCfg, metrics)
	if err != nil {
		r.logger.Error("Cannot create L1 sender for deposit", "err", err)
		return false, nil
	}

	r.logger.Info("Depositing on L1, waiting for confirmation...")
	l1Sender.Deposit()

	r.logger.Info("L1 deposit confirmed. Waiting for L2 balance to arrive...")
	balBefore := new(big.Int).Set(l2Bal)
	deadline := time.After(5 * time.Minute)
	pollTick := time.NewTicker(5 * time.Second)
	defer pollTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline:
			r.logger.Warn("Timeout waiting for L2 deposit; continuing anyway")
			return true, nil
		case <-pollTick.C:
			newBal, err := l2Client.BalanceAt(ctx, account, nil)
			if err != nil {
				continue
			}
			if newBal.Cmp(balBefore) > 0 {
				r.logger.Info("L2 deposit arrived",
					"balance_rbtc", fmt.Sprintf("%.6f", weiToRBTCFloat(newBal)),
				)
				return true, nil
			}
		}
	}
}

// TerminalFundingPrompt shows an interactive prompt on stderr/stdin asking the
// user whether to deposit RBTC from L1 to fund the L2 account.
func TerminalFundingPrompt(info FundingInfo) (FundingAction, float64) {
	l2Bal := weiToRBTCFloat(info.L2BalanceWei)
	l1Bal := weiToRBTCFloat(info.L1BalanceWei)

	fmt.Fprintf(os.Stderr, "\n\033[1;33m⚠  L2 Account Needs Funding\033[0m\n\n")
	fmt.Fprintf(os.Stderr, "  Account:      %s\n", info.Account.Hex())
	fmt.Fprintf(os.Stderr, "  L2 Balance:   %.6f RBTC\n", l2Bal)
	fmt.Fprintf(os.Stderr, "  L1 Balance:   %.6f RBTC\n", l1Bal)
	if info.EstimatedNeed > 0 {
		fmt.Fprintf(os.Stderr, "  Est. need:    %.6f RBTC\n", info.EstimatedNeed)
	}
	fmt.Fprintf(os.Stderr, "\n")

	if l1Bal < 0.001 {
		fmt.Fprintf(os.Stderr, "  \033[1;31mL1 balance too low for automatic deposit.\033[0m\n")
		fmt.Fprintf(os.Stderr, "  Fund the L1 account and restart, or press [q] to quit.\n\n")
		fmt.Fprintf(os.Stderr, "  [q] Quit experiment\n\n")
		fmt.Fprintf(os.Stderr, "  Choice: ")
		bufio.NewScanner(os.Stdin).Scan()
		return FundingQuit, 0
	}

	fmt.Fprintf(os.Stderr, "  \033[1m[Enter]\033[0m  Deposit %.4f RBTC (L1 → L2)\n", info.SuggestedAmount)
	fmt.Fprintf(os.Stderr, "  \033[1m[q]\033[0m      Quit experiment\n\n")
	fmt.Fprintf(os.Stderr, "  Choice: ")

	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		if strings.TrimSpace(strings.ToLower(scanner.Text())) == "q" {
			return FundingQuit, 0
		}
	}
	return FundingDeposit, info.SuggestedAmount
}

// pendingFunding tracks an L1→L2 deposit submitted during preflight. The L1
// transaction is awaited in a background goroutine; WaitL2Credit then polls
// the master's L2 balance until it exceeds the pre-deposit baseline.
type pendingFunding struct {
	account   common.Address
	balBefore *big.Int
	l1Done    <-chan error
}

// PreflightFunding performs the synchronous balance check and user prompt
// portion of L2 funding, kicking off the L1 deposit in a background goroutine
// so it can proceed concurrently with run-start-condition waits.
//
// Returns ErrPreflight (wrapped) when the user chooses to quit or the
// configuration is invalid. On success, may populate r.pendingFunding which
// Run will await before proceeding with work that needs master L2 balance.
//
// No-op when ERC20 traffic is not enabled or when a token address is already
// configured (no auto-deploy → no master funding needed).
func (r *Runner) PreflightFunding(ctx context.Context) error {
	erc20Enabled := r.sim.ERC20TxRate.Start() > 0 || r.sim.ERC20TxRate.IsRandom() || !r.sim.ERC20TxRate.IsConstant()
	if !erc20Enabled {
		return nil
	}

	monCfg := r.buildMonitorConfig()
	if monCfg.PrivateKey == "" {
		return nil
	}
	// Token already configured → no deploy → no master funding required.
	if monCfg.ERC20TrafficToken != "" || monCfg.USDRIFTokenL2 != "" {
		return nil
	}

	masterAddr, err := monitor.AddressFromKey(monCfg.PrivateKey)
	if err != nil {
		return fmt.Errorf("%w: invalid master private key: %w", ErrPreflight, err)
	}

	l2c, err := ethclient.DialContext(ctx, r.target.L2RPC)
	if err != nil {
		return fmt.Errorf("%w: L2 dial failed for preflight (%s): %w", ErrPreflight, r.target.L2RPC, err)
	}
	defer l2c.Close()

	l2Bal, err := l2c.BalanceAt(ctx, masterAddr, nil)
	if err != nil {
		return fmt.Errorf("%w: cannot read master L2 balance for %s: %w", ErrPreflight, masterAddr.Hex(), err)
	}

	// 0.01 RBTC threshold — comfortably enough for the deploy + a few funding txs.
	threshold := big.NewInt(1e16)
	if l2Bal.Cmp(threshold) >= 0 {
		r.logger.Info("Preflight: master L2 balance sufficient",
			"account", masterAddr.Hex(),
			"balance_rbtc", fmt.Sprintf("%.6f", weiToRBTCFloat(l2Bal)),
		)
		return nil
	}

	if r.fundingPrompt == nil {
		return fmt.Errorf("%w: ERC20 traffic enabled but master %s L2 balance %.6f RBTC is below threshold and no funding prompt is configured",
			ErrPreflight, masterAddr.Hex(), weiToRBTCFloat(l2Bal))
	}

	// Read L1 balance for the prompt UX.
	var l1Bal *big.Int
	if r.target.L1RPC != "" {
		if l1c, derr := ethclient.DialContext(ctx, r.target.L1RPC); derr == nil {
			if b, berr := l1c.BalanceAt(ctx, masterAddr, nil); berr == nil {
				l1Bal = b
			}
			l1c.Close()
		}
	}
	if l1Bal == nil {
		l1Bal = big.NewInt(0)
	}

	suggested := 0.1
	if l1RBTC := weiToRBTCFloat(l1Bal); l1RBTC < suggested && l1RBTC > 0.001 {
		suggested = l1RBTC * 0.5
	}

	info := FundingInfo{
		Account:         masterAddr,
		L2BalanceWei:    l2Bal,
		L1BalanceWei:    l1Bal,
		SuggestedAmount: suggested,
	}

	action, amount := r.fundingPrompt(info)
	if action == FundingQuit {
		return fmt.Errorf("%w: user chose to quit at funding prompt — master %s has %.6f RBTC on L2 and ERC20 traffic needs to deploy a token",
			ErrPreflight, masterAddr.Hex(), weiToRBTCFloat(l2Bal))
	}
	if amount <= 0 {
		amount = suggested
	}

	// Build L1 sender and kick off the deposit asynchronously. The L1 tx
	// confirmation runs concurrently with run-start-condition waits in
	// main.go. WaitL2Credit (called from Run) then awaits the L2 credit.
	depositCfg := r.buildMonitorConfig()
	depositCfg.BridgeAmountRBTC = rbtcStr(amount)
	tmpMetrics := monitor.NewMetricsStore(100)
	l1Sender, err := monitor.NewL1Sender(ctx, depositCfg, tmpMetrics)
	if err != nil {
		return fmt.Errorf("%w: create L1 sender for preflight deposit: %w", ErrPreflight, err)
	}

	done := make(chan error, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- fmt.Errorf("L1 deposit panicked: %v", rec)
			}
		}()
		r.logger.Info("Preflight: submitting L1 deposit",
			"amount_rbtc", fmt.Sprintf("%.6f", amount),
			"account", masterAddr.Hex(),
		)
		l1Sender.Deposit() // blocks until L1 confirmation
		r.logger.Info("Preflight: L1 deposit confirmed (awaiting L2 credit)")
		done <- nil
	}()

	r.pendingFunding = &pendingFunding{
		account:   masterAddr,
		balBefore: new(big.Int).Set(l2Bal),
		l1Done:    done,
	}
	return nil
}

// WaitL2Credit blocks until the L1 deposit has confirmed and the master's L2
// balance has grown past the pre-deposit baseline, or the timeout fires.
func (pf *pendingFunding) WaitL2Credit(ctx context.Context, l2 *ethclient.Client, timeout time.Duration) (*big.Int, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-pf.l1Done:
		if err != nil {
			return nil, fmt.Errorf("L1 deposit failed: %w", err)
		}
	}
	deadline := time.After(timeout)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("timeout waiting for L2 credit to land for %s after L1 deposit", pf.account.Hex())
		case <-tick.C:
			bal, err := l2.BalanceAt(ctx, pf.account, nil)
			if err != nil {
				continue
			}
			if bal.Cmp(pf.balBefore) > 0 {
				return bal, nil
			}
		}
	}
}

// AutoFundingPrompt automatically chooses to deposit. Used in --live mode where
// stdin is owned by the TUI.
func AutoFundingPrompt(info FundingInfo) (FundingAction, float64) {
	l1Bal := weiToRBTCFloat(info.L1BalanceWei)
	if l1Bal < 0.001 {
		return FundingQuit, 0
	}
	return FundingDeposit, info.SuggestedAmount
}

// shouldStop returns true if ANY configured stop condition is met.
func (r *Runner) shouldStop(elapsed time.Duration, rowCount int, startL1, startL2, startBatcherPosts, startBatcherBlocks uint64, mv map[string]float64) bool {
	for _, cond := range r.sim.Length {
		if r.conditionMet(cond, elapsed, rowCount, startL1, startL2, startBatcherPosts, startBatcherBlocks, mv) {
			return true
		}
	}
	return false
}

// conditionMet checks a single stop condition.
func (r *Runner) conditionMet(cond ParsedLength, elapsed time.Duration, rowCount int, startL1, startL2, startBatcherPosts, startBatcherBlocks uint64, mv map[string]float64) bool {
	switch cond.Mode {
	case LengthTime:
		return elapsed >= cond.Duration
	case LengthDataPoints:
		return rowCount >= cond.Count
	case LengthL1Blocks:
		if startL1 == 0 {
			return false
		}
		return uint64(mv["latest_l1_block"])-startL1 >= uint64(cond.Count)
	case LengthL2Blocks:
		if startL2 == 0 {
			return false
		}
		return uint64(mv["latest_l2_block"])-startL2 >= uint64(cond.Count)
	case LengthBatcherPosts:
		current := uint64(mv["total_batcher_posts"])
		if current == 0 {
			return false
		}
		return current-startBatcherPosts >= uint64(cond.Count)
	case LengthBatcherBlocks:
		current := uint64(mv["total_batcher_blocks"])
		if current == 0 {
			return false
		}
		return current-startBatcherBlocks >= uint64(cond.Count)
	}
	return false
}

func (r *Runner) batcherMetricsURL() string {
	if r.target.BatcherMetricsURL != "" {
		return r.target.BatcherMetricsURL
	}
	return "http://127.0.0.1:7300/metrics"
}

// buildMonitorConfig creates a monitor.Config from the experiment configuration,
// suitable for passing to the Collector and TrafficSimulator.
func (r *Runner) buildMonitorConfig() *monitor.Config {
	pk := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r.target.PrivateKey), "0x"))

	cfg := &monitor.Config{
		L1RPC:              r.target.L1RPC,
		L2RPC:              r.target.L2RPC,
		NodeRPC:            r.target.NodeRPC,
		WorkDir:            r.target.WorkDir,
		PrivateKey:         pk,
		PollInterval:       r.sim.SampleInterval,
		HistorySize:        10000,
		TrafficWorkers:     64,
		TrafficAccounts:    r.sim.Accounts,
		OnlyMonologues:     r.sim.OnlyMonologues,
		TxCalldataSize:     r.sim.TxCalldataSize,
		BridgeAmountRBTC:   rbtcStr(r.sim.DepositRBTCValue.Start()),
		BatcherMetricsURL:  r.batcherMetricsURL(),
		NodeMetricsURL:     r.target.NodeMetricsURL,
		ProposerMetricsURL: r.target.ProposerMetricsURL,
		NodePprofURL:       r.target.NodePprofURL,
		BatcherPprofURL:    r.target.BatcherPprofURL,
		ProposerPprofURL:   r.target.ProposerPprofURL,
	}

	erc20Start := r.sim.DepositERC20Value.Start()
	if erc20Start > 0 {
		cfg.BridgeAmountUSDRIF = rbtcStr(erc20Start)
	}
	if r.sim.ERC20ContractL1 != "" {
		cfg.USDRIFTokenL1 = r.sim.ERC20ContractL1
	}
	if r.sim.ERC20ContractL2 != "" {
		cfg.USDRIFTokenL2 = r.sim.ERC20ContractL2
	}

	// Load rollup config from workdir
	if cfg.WorkDir != "" {
		rollup, err := monitor.LoadRollupConfig(cfg.WorkDir)
		if err == nil {
			cfg.Rollup = rollup
		}
		addrs, err := monitor.LoadL1Contracts(cfg.WorkDir)
		if err == nil {
			cfg.L1Addrs = addrs
		}
	}

	return cfg
}
