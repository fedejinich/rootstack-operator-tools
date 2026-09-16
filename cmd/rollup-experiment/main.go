package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fedejinich/rootstack-operator-tools/cmd/rollup-monitor/monitor"
	"github.com/urfave/cli/v2"
)

func main() {
	app := &cli.App{
		Name:  "rollup-experiment",
		Usage: "Run automated experiments against a running OP Stack L2 rollup",
		Description: `Drives traffic with dynamic curves, manages batcher configurations,
collects enriched metrics, and produces analysis-ready CSV and markdown reports.

The experiment is configured via a TOML file that defines:
  - target: the running L2 endpoints to connect to
  - simulation: dynamic traffic parameters (rates, accounts, bridge ops)
  - batcher: knobs to sweep across runs (array values = multiple runs)
  - simulation.simple_tx rate_runs: optional list of tx/s rates, one experiment run per value (× batcher sweep)
  - measurements: which metrics to collect`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Usage:   "Path to experiment TOML config file",
				EnvVars: []string{"EXPERIMENT_CONFIG"},
			},
			&cli.StringFlag{
				Name:    "output",
				Aliases: []string{"o"},
				Usage:   "Output directory for CSV and reports",
				Value:   "",
				EnvVars: []string{"EXPERIMENT_OUTPUT"},
			},
			&cli.StringFlag{
				Name:    "batcher-bin",
				Usage:   "Path to compiled batcher binary (overrides TOML target.batcher_bin)",
				EnvVars: []string{"BATCHER_BIN"},
			},
			&cli.StringFlag{
				Name:    "rollup-node-bin",
				Usage:   "Path to compiled rollup-node binary (overrides TOML target.rollup_node_bin)",
				EnvVars: []string{"ROLLUP_NODE_BIN"},
			},
			&cli.StringFlag{
				Name:    "rollup-node-config",
				Usage:   "Path to rollup-node TOML config (overrides TOML target.rollup_node_config)",
				EnvVars: []string{"ROLLUP_NODE_CONFIG"},
			},
			&cli.BoolFlag{
				Name:  "no-batcher",
				Usage: "Skip batcher management (assume batcher is already running)",
				Value: false,
			},
			&cli.StringFlag{
				Name:    "log-level",
				Usage:   "Log level: debug, info, warn, error",
				Value:   "info",
				EnvVars: []string{"LOG_LEVEL"},
			},
			&cli.StringFlag{
				Name:    "log-file",
				Usage:   "Write logs to this file instead of stdout",
				EnvVars: []string{"EXPERIMENT_LOG_FILE"},
			},
			&cli.BoolFlag{
				Name:  "live",
				Usage: "Show real-time analysis TUI while the experiment runs",
				Value: false,
			},
		},
		Action: runExperiment,
		Commands: []*cli.Command{
			{
				Name:  "drain",
				Usage: "Sweep remaining balances from derived traffic accounts back to the master",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "config",
						Aliases:  []string{"c"},
						Usage:    "Path to experiment TOML config file",
						Required: true,
						EnvVars:  []string{"EXPERIMENT_CONFIG"},
					},
					&cli.IntFlag{
						Name:    "accounts",
						Aliases: []string{"n"},
						Usage:   "Number of derived accounts to drain (overrides TOML simulation.accounts)",
					},
					&cli.StringFlag{
						Name:    "log-level",
						Usage:   "Log level: debug, info, warn, error",
						Value:   "info",
						EnvVars: []string{"LOG_LEVEL"},
					},
					&cli.StringFlag{
						Name:    "log-file",
						Usage:   "Write logs to this file instead of stdout",
						EnvVars: []string{"EXPERIMENT_LOG_FILE"},
					},
				},
				Action: runDrain,
			},
			{
				Name:      "remove-runs",
				Usage:     "Remove data for specific experiment runs and regenerate report",
				ArgsUsage: "<run_ids...>",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "dir",
						Aliases:  []string{"d"},
						Usage:    "Experiment output directory containing CSV files",
						Required: true,
					},
					&cli.BoolFlag{
						Name:    "dry-run",
						Aliases: []string{"n"},
						Usage:   "Show what would be deleted without actually deleting",
					},
				},
				Action: runRemoveRuns,
			},
			{
				Name:  "verify-posts",
				Usage: "Confirm the number of batcher posts on L1 for each experiment run",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "dir",
						Aliases:  []string{"d"},
						Usage:    "Experiment output directory containing CSV files",
						Required: true,
					},
					&cli.StringFlag{
						Name:     "workdir",
						Aliases:  []string{"w"},
						Usage:    "Rollup workdir containing rollup.json",
						Required: true,
					},
					&cli.StringFlag{
						Name:     "l1-rpc",
						Usage:    "L1 RPC endpoint",
						Required: true,
					},
					&cli.IntFlag{
						Name:  "run",
						Usage: "Verify only this run ID (default: all runs)",
					},
					&cli.StringFlag{
						Name:  "log-level",
						Usage: "Log level: debug, info, warn, error",
						Value: "info",
					},
				},
				Action: runVerifyPosts,
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runExperiment(cliCtx *cli.Context) error {
	// Setup logger
	logLevel := slog.LevelInfo
	switch cliCtx.String("log-level") {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}
	// Load experiment config
	configPath := cliCtx.String("config")
	if configPath == "" {
		return fmt.Errorf("--config / -c is required")
	}
	cfg, err := LoadExperimentConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Resolve live TUI config (needed before logger setup for log buffer size).
	liveCfg := ResolveLiveConfig(cfg.Live)

	// In live mode, route logs to the TUI action log panel instead of
	// stdout/stderr to avoid display corruption on the altscreen.
	// Determine log output destination.
	logOutput := os.Stdout
	if logFile := cliCtx.String("log-file"); logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("open log file: %w", err)
		}
		defer f.Close()
		logOutput = f
	}

	var actionLog *experimentLog
	var logger *slog.Logger
	if cliCtx.Bool("live") {
		actionLog = newExperimentLog(liveCfg.LogBufferSize)
		logger = slog.New(newTUILogHandler(actionLog, logLevel))
	} else {
		logger = slog.New(slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: logLevel}))
	}
	logger.Info("Loaded experiment config", "path", configPath)

	// Resolve simulation config
	simCfg, err := ResolveSimulation(cfg.Simulation)
	if err != nil {
		return fmt.Errorf("resolve simulation: %w", err)
	}
	logger.Info("Simulation config resolved",
		"length", simCfg.Length,
		"accounts", simCfg.Accounts,
		"sample_interval", simCfg.SampleInterval,
		"tx_rate", simCfg.TxRate.Describe(),
		"tx_value", simCfg.TxValue.Describe(),
		"deposit_rbtc_value", simCfg.DepositRBTCValue.Describe(),
		"deposit_erc20_value", simCfg.DepositERC20Value.Describe(),
	)

	// Resolve run start conditions
	runStartCond := ResolveRunStartConditions(cfg.RunStart)
	if runStartCond.HasConditions() {
		logger.Info("Run start conditions configured",
			"pending_blocks", formatThreshold(runStartCond.PendingBlocks, runStartCond.PendingBlocksAny),
			"safe_lag", formatThreshold(runStartCond.SafeLag, runStartCond.SafeLagAny),
			"timeout", runStartCond.Timeout,
			"on_first_run", runStartCond.OnFirstRun,
		)
	}

	// When the experiment doesn't manage the batcher (--no-batcher or no
	// batcher binary), fill unconfigured batcher knobs from the rollup-node
	// TOML so the experiment records the actual running batcher's parameters
	// instead of hardcoded defaults.
	fillBatcherFromNodeConfig(cliCtx, cfg, logger)

	// Expand full sweep (batcher + chain knobs)
	batcherRuns := ExpandSweep(cfg.Batcher, cfg.Chain)
	logger.Info("Sweep expanded", "runs", len(batcherRuns))
	for i, run := range batcherRuns {
		logger.Info("  Run config",
			"run", i+1,
			"max_l1_tx_size", run.MaxL1TxSize,
			"max_channel_duration", run.MaxChannelDuration,
			"max_blocks_per_span_batch", run.MaxBlocksPerSpanBatch,
			"max_pending_tx", run.MaxPendingTx,
			"gas_limit", run.GasLimit,
			"block_time", run.BlockTime,
		)
	}

	// Check if block_time sweep requires rollup-node management
	hasBlockTimeSweep := cfg.Chain.BlockTime.IsSweep()
	hasGasLimitSweep := cfg.Chain.GasLimit.IsSweep() || (len(cfg.Chain.GasLimit.Values) == 1 && cfg.Chain.GasLimit.Values[0] > 0)

	// Validate batcher runs against rollup fork schedule
	if cfg.Target.WorkDir != "" {
		rollupCfg, err := monitor.LoadRollupConfig(cfg.Target.WorkDir)
		if err != nil {
			logger.Warn("Could not load rollup.json for validation; skipping fork checks", "err", err)
		} else {
			before := len(batcherRuns)
			batcherRuns = FilterBatcherRuns(batcherRuns, rollupCfg, logger)
			if skipped := before - len(batcherRuns); skipped > 0 {
				logger.Warn("Filtered out unsupported batcher configurations",
					"skipped", skipped,
					"remaining", len(batcherRuns),
				)
			}
		}
	}

	if len(batcherRuns) == 0 {
		return fmt.Errorf("no valid batcher configurations remain after validation; check rollup.json fork schedule")
	}

	experimentRuns := ExpandExperimentRuns(batcherRuns, cfg.Simulation.SimpleTx.RateRuns)
	logger.Info("Experiment runs",
		"count", len(experimentRuns),
		"simple_tx_rate_runs", len(cfg.Simulation.SimpleTx.RateRuns),
	)
	if len(cfg.Simulation.SimpleTx.RateRuns) > 0 {
		logger.Info("Per-run simple_tx rates", "rate_runs", cfg.Simulation.SimpleTx.RateRuns)
	}

	// Prepare output directory
	outputDir := cliCtx.String("output")
	if outputDir == "" {
		outputDir = fmt.Sprintf("experiment_%s", time.Now().Format("20060102_150405"))
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	absOutput, _ := filepath.Abs(outputDir)
	logger.Info("Output directory", "path", absOutput)

	// Copy experiment config to output dir for reproducibility. Preserve every
	// distinct config that has been run into this dir: if an existing
	// experiment*.toml already matches, skip; otherwise write a new numbered
	// variant so prior runs' configs are never overwritten.
	if cfgData, err := os.ReadFile(configPath); err == nil && len(cfgData) > 0 {
		if dst, wrote, err := preserveExperimentConfig(outputDir, cfgData); err != nil {
			logger.Warn("Could not preserve experiment config", "err", err)
		} else if wrote {
			logger.Info("Saved experiment config", "path", dst)
		} else {
			logger.Info("Experiment config already preserved", "path", dst)
		}
	}

	// Resolve binary paths: CLI flags override TOML values
	batcherBin := cliCtx.String("batcher-bin")
	if batcherBin == "" {
		batcherBin = cfg.Target.BatcherBin
	}
	rollupNodeBin := cliCtx.String("rollup-node-bin")
	if rollupNodeBin == "" {
		rollupNodeBin = cfg.Target.RollupNodeBin
	}
	rollupNodeConfig := cliCtx.String("rollup-node-config")
	if rollupNodeConfig == "" {
		rollupNodeConfig = cfg.Target.RollupNodeConfig
	}

	// Validate that rollup-node binary/config are available when block_time is swept
	if hasBlockTimeSweep {
		if rollupNodeBin == "" {
			return fmt.Errorf("block_time sweep requires --rollup-node-bin or target.rollup_node_bin in TOML")
		}
		if rollupNodeConfig == "" {
			return fmt.Errorf("block_time sweep requires --rollup-node-config or target.rollup_node_config in TOML")
		}
	}

	// Validate that workdir is set when gas_limit sweep is active
	if hasGasLimitSweep && cfg.Target.WorkDir == "" {
		return fmt.Errorf("gas_limit sweep requires target.workdir to be set (for SystemConfig contract address)")
	}

	// In live mode, redirect subprocess output to log files so it doesn't
	// corrupt the TUI alt screen. In non-live mode, keep terminal output.
	subprocLogDir := ""
	if cliCtx.Bool("live") {
		subprocLogDir = outputDir
	}

	// Setup batcher controller (subprocess or RPC mode)
	noBatcher := cliCtx.Bool("no-batcher")
	var batcherCtrl BatcherController
	if !noBatcher {
		if batcherBin != "" {
			// Subprocess mode: spawn batcher binary
			resolved, err := ResolveBatcherBinary(batcherBin)
			if err != nil {
				return fmt.Errorf("resolve batcher binary: %w", err)
			}
			batcherCtrl = NewBatcherProcess(logger, resolved, subprocLogDir, cfg.Target.BatcherMetricsURL, defaultBatcherRPCURL)
			logger.Info("Batcher process manager initialized (subprocess mode)",
				"binary", resolved,
				"metrics_url", cfg.Target.BatcherMetricsURL,
			)
		} else if cfg.Target.BatcherRPCURL != "" {
			// RPC mode: control existing batcher via admin RPC
			batcherCtrl = NewBatcherRPC(logger, cfg.Target.BatcherRPCURL, cfg.Target.BatcherConfigPath)
			logger.Info("Batcher RPC controller initialized",
				"rpc_url", cfg.Target.BatcherRPCURL,
				"config_path", cfg.Target.BatcherConfigPath,
			)
		} else {
			logger.Info("No batcher binary or RPC URL specified; batcher management disabled (use --batcher-bin, target.batcher_bin, target.batcher_rpc_url, or --no-batcher)")
		}
	}

	// Setup rollup-node process manager for block_time sweep
	var nodeProc *NodeProcess
	if hasBlockTimeSweep {
		resolved, err := ResolveNodeBinary(rollupNodeBin)
		if err != nil {
			return fmt.Errorf("resolve rollup-node binary: %w", err)
		}
		nodeProc = NewNodeProcess(logger, resolved, rollupNodeConfig, subprocLogDir)
		logger.Info("Rollup-node process manager initialized", "binary", resolved, "config", rollupNodeConfig)
	}

	// Setup gas limit changer for gas_limit sweep
	var gasChanger *GasLimitChanger
	if hasGasLimitSweep {
		gc, err := NewGasLimitChanger(logger, cfg.Target, rollupNodeConfig)
		if err != nil {
			return fmt.Errorf("init gas limit changer: %w", err)
		}
		gasChanger = gc

		if cur, err := gc.CurrentGasLimit(context.Background(), cfg.Target.L2RPC); err != nil {
			logger.Warn("Could not query L2 head gas limit at startup", "err", err)
		} else {
			logger.Info("Gas limit changer initialized", "l2_head_gas_limit", cur)
		}
	}

	// Setup signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Live analysis TUI setup (before signal handler so we can quit TUI on signal)
	liveMode := cliCtx.Bool("live")
	var sampleCh chan ExperimentSample
	var waitTUI func() error
	var quitTUI func()
	if liveMode {
		sampleCh = make(chan ExperimentSample, 256)
		watcher := NewCSVWatcherWithChannel(sampleCh)
		go watcher.Run()
		waitTUI, quitTUI = RunAnalysisTUIAsync(watcher, outputDir, simCfg.SampleInterval, actionLog, &liveCfg, cancel)
		logger.Info("Live analysis TUI started")
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("Received signal, stopping experiment gracefully...", "signal", sig)
		cancel()
		if quitTUI != nil {
			quitTUI()
		}

		sig = <-sigCh
		fmt.Fprintf(os.Stderr, "\nReceived second %v — forcing exit\n", sig)
		os.Exit(1)
	}()

	// Scan output dir for existing CSVs to support incremental runs.
	// Determine run ID strategy: "max" continues past highest, "min" fills gaps.
	runStrategy := cfg.NextRunStrategy
	if runStrategy == "" {
		runStrategy = "max"
	}
	existingMaxRunID := MaxRunIDInDir(outputDir)
	if existingMaxRunID > 0 {
		logger.Info("Found existing experiment data in output dir",
			"max_run_id", existingMaxRunID,
			"next_run_strategy", runStrategy)
	}

	// Run experiments
	var results []*RunResult
	var lastBlockTime uint64
	var sequencerPausedHash string // Track if sequencer is paused between runs
	cleanStart := true             // First run is clean (fresh batcher start)
	for i, er := range experimentRuns {
		batcherCfg := er.Batcher
		// Determine run ID based on strategy
		var runID int
		if runStrategy == "min" {
			runID = NextRunIDInDir(outputDir, "min")
		} else {
			runID = existingMaxRunID + i + 1
		}
		logArgs := []any{
			"run", runID,
			"total_runs", len(experimentRuns),
		}
		if er.SimpleTxRate != nil {
			logArgs = append(logArgs, "simple_tx_rate", *er.SimpleTxRate)
		}
		logger.Info("=== Starting experiment run ===", logArgs...)

		// Handle block_time change (requires rollup-node restart)
		if nodeProc != nil && batcherCfg.BlockTime > 0 && batcherCfg.BlockTime != lastBlockTime {
			logger.Info("Block time changed, restarting rollup-node",
				"old", lastBlockTime,
				"new", batcherCfg.BlockTime,
			)
			if err := nodeProc.Stop(); err != nil {
				logger.Warn("Error stopping rollup-node", "err", err)
			}
			if err := nodeProc.Start(ctx, cfg.Target.WorkDir, batcherCfg.BlockTime); err != nil {
				return fmt.Errorf("restart rollup-node for block_time=%d: %w", batcherCfg.BlockTime, err)
			}
			if err := nodeProc.WaitHealthy(ctx, cfg.Target); err != nil {
				return fmt.Errorf("rollup-node not healthy after restart: %w", err)
			}
			lastBlockTime = batcherCfg.BlockTime
		}

		// Align L2 gas limit with experiment target (L1 SystemConfig + wait for L2 head)
		if gasChanger != nil && batcherCfg.GasLimit > 0 {
			rec, err := gasChanger.EnsureGasLimit(ctx, cfg.Target.L2RPC, batcherCfg.GasLimit)
			if rec != nil {
				rec.RunID = runID
				if err != nil {
					rec.Error = err.Error()
				}
				vpath := filepath.Join(outputDir, "gas_limit_verification.jsonl")
				if aerr := AppendGasLimitVerificationRecord(vpath, rec); aerr != nil {
					logger.Warn("Failed to append gas limit verification record", "path", vpath, "err", aerr)
				}
			}
			if err != nil {
				return fmt.Errorf("ensure gas limit %d for run %d: %w", batcherCfg.GasLimit, runID, err)
			}
		}

		// Build the runner up front so preflight can run synchronously here
		// (interactive prompt) while the slow start-conditions wait and any
		// pending L1 deposit confirmation proceed in parallel.
		simForRun := simCfg
		if er.SimpleTxRate != nil {
			simForRun = simCfg.WithSimpleTxRate(*er.SimpleTxRate)
		}
		runner := NewRunner(runID, logger, cfg.Target, simForRun, batcherCfg, outputDir, cleanStart)
		if sampleCh != nil {
			runner.sampleCh = sampleCh
		}
		if liveMode {
			runner.fundingPrompt = AutoFundingPrompt
		} else {
			runner.fundingPrompt = TerminalFundingPrompt
		}

		// Preflight: synchronous balance check + prompt. If the user chooses
		// to quit, abort here before paying the cost of waitForRunStartConditions
		// (up to 5 min) and the sequencer/batcher restart dance. If the user
		// chooses to deposit, the L1 tx is submitted on a background goroutine
		// and runs concurrently with the wait loop below.
		if err := runner.PreflightFunding(ctx); err != nil {
			if errors.Is(err, ErrPreflight) {
				logger.Error("Experiment aborted: preflight failed", "run", runID, "err", err)
				return err
			}
			logger.Warn("Preflight encountered a non-fatal error", "err", err)
		}

		// Wait for run start conditions (external batcher case: batcher keeps running)
		// For managed batcher, this is done before stopping at end of previous run.
		if batcherCtrl == nil && (i > 0 || runStartCond.OnFirstRun) {
			if err := waitForRunStartConditions(ctx, logger, cfg.Target, runStartCond); err != nil {
				logger.Warn("Run start conditions not met", "err", err)
			}
		}

		// Start batcher with this run's config (includes health check)
		if batcherCtrl != nil {
			if err := batcherCtrl.Start(ctx, cfg.Target, batcherCfg); err != nil {
				logger.Error("Failed to start batcher", "err", err)
				return fmt.Errorf("start batcher for run %d: %w", runID, err)
			}
		}

		// Resume sequencer if it was paused between runs. Any preflight L1
		// deposit submitted above only becomes visible on L2 after the
		// sequencer starts producing blocks again.
		if sequencerPausedHash != "" && cfg.Target.NodeRPC != "" {
			if err := StartSequencer(ctx, cfg.Target.NodeRPC, sequencerPausedHash); err != nil {
				logger.Warn("Failed to resume sequencer", "err", err)
			} else {
				logger.Info("Sequencer resumed after batcher restart")
			}
			sequencerPausedHash = ""
		}

		result, err := runner.Run(ctx)
		if err != nil {
			if errors.Is(err, ErrPreflight) {
				logger.Error("Experiment aborted: preflight failed", "run", runID, "err", err)
				return err
			}
			logger.Error("Experiment run failed", "run", runID, "err", err)
		} else {
			results = append(results, result)
			logger.Info("Experiment run completed",
				"run", runID,
				"rows", result.RowCount,
				"duration", result.Duration.Round(time.Second),
				"csv", result.CSVPath,
			)
		}

		// Between runs: pause sequencer, fully drain batcher, stop batcher
		// This ensures Prometheus counters reset and no data carries over
		if batcherCtrl != nil && i < len(experimentRuns)-1 {
			// Pause sequencer to prevent new blocks during drain
			sequencerPaused := false
			if cfg.Target.NodeRPC != "" {
				hash, err := StopSequencer(ctx, cfg.Target.NodeRPC)
				if err != nil {
					logger.Warn("Failed to pause sequencer for batcher restart", "err", err)
				} else {
					sequencerPausedHash = hash
					sequencerPaused = true
					logger.Info("Sequencer paused for batcher restart", "unsafe_head", hash)
				}
			}

			// Flush pending data if batcher supports it
			if flusher, ok := batcherCtrl.(BatcherFlusher); ok {
				if err := flusher.Flush(ctx); err != nil {
					logger.Warn("Error flushing batcher", "err", err)
				}
			}

			// Wait for drain: full drain if sequencer paused, channel drain otherwise
			batcherRPC := cfg.Target.BatcherRPCURL
			if batcherRPC == "" {
				batcherRPC = "http://127.0.0.1:8548"
			}
			drainErr := WaitForBatcherDrain(ctx, logger, cfg.Target.BatcherMetricsURL, batcherRPC, sequencerPaused, 5*time.Minute)
			if drainErr != nil {
				logger.Warn("Batcher not fully drained before restart", "err", drainErr)
			}

			// Next run is clean only if sequencer was paused AND drain succeeded
			cleanStart = sequencerPaused && drainErr == nil
		}

		// Stop batcher between runs (will restart with fresh Prometheus counters)
		if batcherCtrl != nil {
			if err := batcherCtrl.Stop(); err != nil {
				logger.Warn("Error stopping batcher", "err", err)
			}
			if i < len(experimentRuns)-1 {
				logger.Info("Cooling down between runs...", "cooldown", "5s")
				select {
				case <-ctx.Done():
				case <-time.After(5 * time.Second):
				}
			}
		}

		// Check if cancelled
		select {
		case <-ctx.Done():
			logger.Info("Experiment cancelled, stopping after run", "run", runID)
			goto generateReport
		default:
		}
	}

generateReport:
	// Clean up subprocesses on all exit paths (including Ctrl+C cancellation).
	if batcherCtrl != nil {
		if err := batcherCtrl.Stop(); err != nil {
			logger.Warn("Error stopping batcher", "err", err)
		}
		batcherCtrl.RestartEmbedded()
	}
	if nodeProc != nil {
		if err := nodeProc.Stop(); err != nil {
			logger.Warn("Error stopping rollup-node", "err", err)
		}
	}

	// Close sample channel so the live TUI watcher stops.
	if sampleCh != nil {
		close(sampleCh)
	}

	// Drain derived accounts back to master.
	if simCfg.DrainOnExit && cfg.Target.PrivateKey != "" && simCfg.Accounts > 1 {
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		drainResult, drainErr := drainDerivedAccounts(drainCtx, logger, DrainConfig{
			L2RPC:          cfg.Target.L2RPC,
			PrivateKeyHex:  cfg.Target.PrivateKey,
			NumAccounts:    simCfg.Accounts,
			ERC20TokenAddr: simCfg.ERC20ContractL2,
		})
		drainCancel()
		if drainErr != nil {
			logger.Warn("Failed to drain derived accounts", "err", drainErr)
		} else {
			logger.Info("Derived accounts drained",
				"rbtc_recovered", weiToRBTCFloat(drainResult.RBTCRecovered),
				"erc20_recovered", drainResult.ERC20Recovered,
				"drained", drainResult.AccountsDrained,
				"skipped", drainResult.AccountsSkipped,
			)
		}
	}

	// Generate report from ALL CSVs in the output dir (current + previous runs).
	// This allows incremental manual sweeps: each invocation adds a run and the
	// report always reflects the full set.
	allResults, err := CollectRunResults(outputDir)
	if err != nil {
		logger.Error("Failed to collect run results from CSVs", "err", err)
	}
	if len(allResults) > 0 {
		reportPath := filepath.Join(outputDir, "report.md")
		if err := GenerateReport(allResults, reportPath); err != nil {
			logger.Error("Failed to generate report", "err", err)
		} else {
			logger.Info("Report generated", "path", reportPath, "runs", len(allResults))
		}

		// Generate combined CSV
		combinedPath := filepath.Join(outputDir, "combined.csv")
		if err := CombineCSVs(allResults, combinedPath); err != nil {
			logger.Error("Failed to combine CSVs", "err", err)
		} else {
			logger.Info("Combined CSV generated", "path", combinedPath)
		}
	}

	// Wait for the live TUI to exit (user presses q), or quit immediately
	// if auto_quit is enabled in the [live] config.
	if waitTUI != nil {
		if liveCfg.AutoQuit {
			logger.Info("Experiment complete. auto_quit enabled — exiting.")
			if quitTUI != nil {
				quitTUI()
			}
		} else {
			logger.Info("Experiment complete. Live analysis TUI still running — press q to exit.")
		}
		// Always wait for TUI to exit cleanly
		if err := waitTUI(); err != nil {
			logger.Warn("Analysis TUI error", "err", err)
		}
	}

	logger.Info("Experiment complete",
		"new_runs", len(results),
		"total_runs", len(allResults),
		"output", absOutput,
	)
	return nil
}

func runDrain(cliCtx *cli.Context) error {
	logLevel := slog.LevelInfo
	switch cliCtx.String("log-level") {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}
	logOutput := os.Stdout
	if logFile := cliCtx.String("log-file"); logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("open log file: %w", err)
		}
		defer f.Close()
		logOutput = f
	}
	logger := slog.New(slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: logLevel}))

	configPath := cliCtx.String("config")
	cfg, err := LoadExperimentConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	simCfg, err := ResolveSimulation(cfg.Simulation)
	if err != nil {
		return fmt.Errorf("resolve simulation: %w", err)
	}

	numAccounts := simCfg.Accounts
	if cliCtx.IsSet("accounts") {
		numAccounts = cliCtx.Int("accounts")
	}

	if cfg.Target.PrivateKey == "" {
		return fmt.Errorf("target.private_key is required for drain")
	}
	if numAccounts <= 1 {
		logger.Info("Nothing to drain (accounts <= 1)")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	result, err := drainDerivedAccounts(ctx, logger, DrainConfig{
		L2RPC:          cfg.Target.L2RPC,
		PrivateKeyHex:  cfg.Target.PrivateKey,
		NumAccounts:    numAccounts,
		ERC20TokenAddr: simCfg.ERC20ContractL2,
	})
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}

	logger.Info("Drain complete",
		"rbtc_recovered_rbtc", weiToRBTCFloat(result.RBTCRecovered),
		"erc20_recovered", result.ERC20Recovered,
		"drained", result.AccountsDrained,
		"skipped", result.AccountsSkipped,
	)
	return nil
}

// fillBatcherFromNodeConfig reads batcher parameters from the rollup-node
// TOML config and uses them as defaults for any unconfigured experiment
// batcher knobs. This is only done when the experiment is not managing the
// batcher (--no-batcher or no batcher binary specified), so that the sweep
// reflects the externally running batcher's actual configuration.
func fillBatcherFromNodeConfig(cliCtx *cli.Context, cfg *ExperimentConfig, logger *slog.Logger) {
	bb := cliCtx.String("batcher-bin")
	if bb == "" {
		bb = cfg.Target.BatcherBin
	}
	if !cliCtx.Bool("no-batcher") && bb != "" {
		return
	}
	rnc := cliCtx.String("rollup-node-config")
	if rnc == "" {
		rnc = cfg.Target.RollupNodeConfig
	}
	if rnc == "" {
		return
	}
	nodeKnobs, err := ReadBatcherFromNodeConfig(rnc)
	if err != nil {
		logger.Warn("Could not read batcher config from rollup-node TOML; using defaults", "err", err)
		return
	}
	cfg.Batcher.FillFromNodeConfig(nodeKnobs)
	logger.Info("Batcher knobs filled from running batcher config", "path", rnc)
}

// runRemoveRuns handles the remove-runs subcommand.
func runRemoveRuns(cliCtx *cli.Context) error {
	dir := cliCtx.String("dir")
	dryRun := cliCtx.Bool("dry-run")

	if cliCtx.NArg() == 0 {
		return fmt.Errorf("no run IDs specified; usage: remove-runs --dir <dir> <run_id> [run_id...]")
	}

	// Parse run IDs from arguments
	var runIDs []int
	for _, arg := range cliCtx.Args().Slice() {
		var id int
		if _, err := fmt.Sscanf(arg, "%d", &id); err != nil {
			return fmt.Errorf("invalid run ID %q: must be a number", arg)
		}
		runIDs = append(runIDs, id)
	}

	// Find CSV files to delete
	var toDelete []string
	for _, id := range runIDs {
		csvPath := filepath.Join(dir, fmt.Sprintf("experiment_run_%d.csv", id))
		if _, err := os.Stat(csvPath); err == nil {
			toDelete = append(toDelete, csvPath)
		} else {
			fmt.Printf("Warning: run %d not found (%s)\n", id, csvPath)
		}
	}

	if len(toDelete) == 0 {
		fmt.Println("No matching run files found.")
		return nil
	}

	// Show what will be deleted
	fmt.Printf("Will delete %d run file(s):\n", len(toDelete))
	for _, f := range toDelete {
		fmt.Printf("  - %s\n", f)
	}

	if dryRun {
		fmt.Println("\nDry run - no files deleted.")
		return nil
	}

	// Delete the files
	for _, f := range toDelete {
		if err := os.Remove(f); err != nil {
			fmt.Printf("Error deleting %s: %v\n", f, err)
		} else {
			fmt.Printf("Deleted: %s\n", f)
		}
	}

	// Regenerate combined.csv and report.md
	fmt.Println("\nRegenerating combined.csv and report.md...")

	results, err := CollectRunResults(dir)
	if err != nil {
		return fmt.Errorf("collect remaining runs: %w", err)
	}

	if len(results) == 0 {
		fmt.Println("No runs remaining. Removing combined.csv and report.md.")
		os.Remove(filepath.Join(dir, "combined.csv"))
		os.Remove(filepath.Join(dir, "report.md"))
		return nil
	}

	combinedPath := filepath.Join(dir, "combined.csv")
	if err := CombineCSVs(results, combinedPath); err != nil {
		return fmt.Errorf("regenerate combined.csv: %w", err)
	}
	fmt.Printf("Regenerated: %s (%d runs)\n", combinedPath, len(results))

	reportPath := filepath.Join(dir, "report.md")
	if err := GenerateReport(results, reportPath); err != nil {
		return fmt.Errorf("regenerate report: %w", err)
	}
	fmt.Printf("Regenerated: %s\n", reportPath)

	return nil
}

// preserveExperimentConfig stores cfgData under outputDir without overwriting
// any prior distinct config. Returns the path the config is at (existing or
// newly written) and whether a new file was written.
//
// Rules:
//   - If no experiment*.toml exists yet, writes to "experiment.toml".
//   - If any existing experiment*.toml has byte-identical content, returns
//     that path with wrote=false (deduplication).
//   - Otherwise, writes to the lowest-numbered free slot among
//     "experiment_2.toml", "experiment_3.toml", … so each distinct config
//     that has been run into this dir is preserved.
func preserveExperimentConfig(outputDir string, cfgData []byte) (string, bool, error) {
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return "", false, fmt.Errorf("read output dir: %w", err)
	}

	type existing struct {
		name string
		idx  int // 1 for "experiment.toml", N for "experiment_N.toml"
	}
	var found []existing
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".toml") {
			continue
		}
		switch {
		case n == "experiment.toml":
			found = append(found, existing{name: n, idx: 1})
		case strings.HasPrefix(n, "experiment_") && !strings.HasPrefix(n, "experiment_run_"):
			mid := strings.TrimSuffix(strings.TrimPrefix(n, "experiment_"), ".toml")
			if idx, err := strconv.Atoi(mid); err == nil && idx >= 2 {
				found = append(found, existing{name: n, idx: idx})
			}
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].idx < found[j].idx })

	// Dedup: return early if any existing file matches the new content.
	for _, f := range found {
		p := filepath.Join(outputDir, f.name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if bytes.Equal(data, cfgData) {
			return p, false, nil
		}
	}

	// Pick next free slot.
	used := make(map[int]bool, len(found))
	for _, f := range found {
		used[f.idx] = true
	}
	var name string
	if !used[1] {
		name = "experiment.toml"
	} else {
		for i := 2; ; i++ {
			if !used[i] {
				name = fmt.Sprintf("experiment_%d.toml", i)
				break
			}
		}
	}
	dst := filepath.Join(outputDir, name)
	if err := os.WriteFile(dst, cfgData, 0644); err != nil {
		return "", false, fmt.Errorf("write %s: %w", dst, err)
	}
	return dst, true, nil
}
