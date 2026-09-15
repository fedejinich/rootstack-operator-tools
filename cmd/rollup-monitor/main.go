package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fedejinich/rootstack-operator-tools/cmd/rollup-monitor/monitor"
	"github.com/fedejinich/rootstack-operator-tools/rsk/chainattach"
	"github.com/fedejinich/rootstack-operator-tools/rsk/opdeployer/redact"
	"github.com/urfave/cli/v2"
)

const defaultConfigPath = "rollup-monitor.toml"

func main() {
	app := &cli.App{
		Name:  "rollup-monitor",
		Usage: "Real-time TUI dashboard for OP Stack rollup metrics",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Usage:   "Path to TOML config file",
				Value:   defaultConfigPath,
				EnvVars: []string{"ROLLUP_MONITOR_CONFIG"},
			},
			&cli.StringFlag{
				Name:    "l1-rpc",
				Usage:   "L1 (RSK) RPC URL (overrides config)",
				EnvVars: []string{"L1_RPC_URL"},
			},
			&cli.StringFlag{
				Name:    "l2-rpc",
				Usage:   "L2 execution RPC URL (overrides config)",
				EnvVars: []string{"L2_RPC_URL"},
			},
			&cli.StringFlag{
				Name:    "node-rpc",
				Usage:   "op-node RPC URL for sync status (overrides config)",
				EnvVars: []string{"NODE_RPC_URL"},
			},
			&cli.StringFlag{
				Name:    "workdir",
				Usage:   "Working directory containing rollup.json and l1.json",
				EnvVars: []string{"WORKDIR"},
			},
			&cli.StringFlag{
				Name:    "addresses",
				Usage:   "L1 address book source: path to l1.json, an http(s):// URL, or '-' for stdin (alternative to --workdir for attaching to a remote chain)",
				EnvVars: []string{"ROLLUP_MONITOR_ADDRESSES", "L1_ADDRESSES"},
			},
			&cli.StringFlag{
				Name:    "private-key",
				Usage:   "Private key (hex) for sending txs; env: ROLLUP_STATS_PRIVATE_KEY",
				EnvVars: []string{"ROLLUP_STATS_PRIVATE_KEY"},
			},
			&cli.IntFlag{
				Name:    "history-blocks",
				Usage:   "Number of L2 blocks to replay from chain on startup (0 = disabled)",
				EnvVars: []string{"HISTORY_BLOCKS"},
			},
			&cli.StringFlag{
				Name:    "load-stats",
				Usage:   "Path to CSV stats file to load on startup (auto-detected from workdir if not set)",
				EnvVars: []string{"LOAD_STATS"},
			},
			&cli.BoolFlag{
				Name:    "headless",
				Usage:   "Run without TUI (log-only mode, for use in scripts/CI)",
				EnvVars: []string{"HEADLESS"},
			},
		},
		Action: run,
	}
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(c *cli.Context) error {
	configPath := c.String("config")
	workdir := c.String("workdir")

	// If --workdir is given but --config was not explicitly set,
	// look for the config inside the workdir.
	if workdir != "" && !c.IsSet("config") {
		configPath = filepath.Join(workdir, "rollup-monitor.toml")
	}

	// Create default config file if it doesn't exist
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		dir := filepath.Dir(configPath)
		if dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("create config dir: %w", err)
			}
		}
		if err := os.WriteFile(configPath, []byte(monitor.DefaultConfigFileContent()), 0644); err != nil {
			return fmt.Errorf("create default config: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Created default config at %s\n", configPath)
	}

	// Load TOML config
	file, err := monitor.LoadConfigFile(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Resolve config with CLI overrides
	pk := strings.TrimSpace(strings.TrimPrefix(c.String("private-key"), "0x"))
	cfg, err := monitor.ResolveConfig(file, c.String("l1-rpc"), c.String("l2-rpc"), c.String("node-rpc"), workdir, pk, c.String("load-stats"), c.Int("history-blocks"))
	if err != nil {
		return fmt.Errorf("resolve config: %w", err)
	}

	// Store config path so the TUI can save prompted values back to the TOML file.
	cfg.ConfigPath = configPath

	// --addresses: load the L1 address book from a file path / http(s) URL / "-"
	// (stdin), as an alternative to --workdir, so the monitor can attach to a
	// remote chain whose deployer-output isn't on this host.
	if src := c.String("addresses"); src != "" {
		addrs, lerr := chainattach.LoadAddresses(chainattach.AddressSource(src), chainattach.Overrides{})
		if lerr != nil {
			return fmt.Errorf("load --addresses: %w", lerr)
		}
		l1 := monitor.L1Contracts{}
		if addrs.HasPortal() {
			l1["OptimismPortalProxy"] = addrs.OptimismPortal.Hex()
		}
		if addrs.HasDGF() {
			l1["DisputeGameFactoryProxy"] = addrs.DisputeGameFactory.Hex()
		}
		if addrs.HasL1StandardBridge() {
			l1["L1StandardBridgeProxy"] = addrs.L1StandardBridge.Hex()
		}
		cfg.L1Addrs = l1
	}

	// Warn loudly when an endpoint silently fell back to a local-dev default
	// (neither the flag/env nor TOML pointed it elsewhere) so a run never quietly
	// targets localhost when a remote chain was intended.
	warnIfLocalDefault(c, "l2-rpc", "L2 RPC", cfg.L2RPC)
	warnIfLocalDefault(c, "node-rpc", "op-node RPC", cfg.NodeRPC)

	// Create metrics store
	metrics := monitor.NewMetricsStore(cfg.HistorySize)

	// Open log file for persistent logging under <workdir>/logs/
	logDir := "."
	if cfg.WorkDir != "" {
		logDir = filepath.Join(cfg.WorkDir, "logs")
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return fmt.Errorf("create logs dir: %w", err)
		}
	}
	logPath := filepath.Join(logDir, "rollup-monitor.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not open log file %s: %v\n", logPath, err)
	} else {
		defer logFile.Close()
		metrics.SetLogFile(logFile)
		metrics.AppendLog("--- rollup-monitor started (L1=%s L2=%s workdir=%s) ---", redact.URL(cfg.L1RPC), redact.URL(cfg.L2RPC), cfg.WorkDir)
	}

	// Determine stats CSV path (also under <workdir>/logs/)
	statsPath := filepath.Join(logDir, "rollup-monitor-stats.csv")

	// Load historical data from CSV if available
	var csvLastL2, csvLastL1 uint64
	csvLoaded := false
	loadPath := cfg.LoadStatsPath
	if loadPath == "" {
		// Auto-detect: use the stats CSV from workdir if it exists
		if _, serr := os.Stat(statsPath); serr == nil {
			loadPath = statsPath
		}
	}
	if loadPath != "" {
		if csvFile, oerr := os.Open(loadPath); oerr == nil {
			l2, l1, rows, lerr := metrics.LoadFromCSV(csvFile)
			csvFile.Close()
			if lerr != nil {
				metrics.AppendLog("[startup] CSV load error: %v", lerr)
			} else if rows > 0 {
				csvLastL2 = l2
				csvLastL1 = l1
				csvLoaded = true
				metrics.AppendLog("[startup] loaded %d data points from %s (L2 #%d, L1 #%d)", rows, loadPath, l2, l1)
			}
		}
	}

	// Open a temporary file for stats CSV export during the session.
	// If a permanent CSV already exists, copy it into the temp file first
	// so that saving on :quit preserves the full history.
	// On ctrl+c the temp file is cleaned up automatically.
	tmpStatsFile, err := os.CreateTemp("", "rollup-monitor-stats-*.csv")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not create temp stats file: %v\n", err)
	} else {
		defer func() {
			tmpStatsFile.Close()
			// Clean up temp file if it still exists (not moved).
			os.Remove(tmpStatsFile.Name())
		}()
		// Copy existing permanent CSV into the temp file so history is preserved.
		if existing, oerr := os.Open(statsPath); oerr == nil {
			if _, cerr := io.Copy(tmpStatsFile, existing); cerr != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not copy existing stats to temp: %v\n", cerr)
			}
			existing.Close()
			// Existing file already has a header; append new rows without a second header.
			metrics.SetStatsFileNoHeader(tmpStatsFile)
		} else {
			// No existing file; write header for a fresh CSV.
			metrics.SetStatsFile(tmpStatsFile)
		}
	}

	// Create context for background operations
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create TUI model
	m := monitor.NewModel(ctx, cfg, metrics, statsPath)

	collector, err := monitor.NewCollector(ctx, cfg, metrics)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: collector init failed: %v (charts will show no data until connection is available)\n", err)
	} else {
		// Configure backfill. history_blocks takes priority so the user
		// always gets the requested replay depth. When a CSV is also
		// loaded, both are set and backfill() uses the wider range.
		if cfg.HistoryBlocks > 0 {
			collector.SetHistoryBlocks(cfg.HistoryBlocks)
		}
		if csvLoaded {
			collector.SetStartBlocks(csvLastL2, csvLastL1)
		}
		m.SetCollector(collector)
		go collector.Run()
	}

	// Create and attach traffic simulator
	trafficSim, err := monitor.NewTrafficSimulator(ctx, cfg, metrics)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: traffic simulator init failed: %v\n", err)
	} else {
		m.SetTrafficSimulator(trafficSim)
	}

	// Create and attach L1 sender
	l1Sender, err := monitor.NewL1Sender(ctx, cfg, metrics)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: L1 sender init failed: %v\n", err)
	} else {
		m.SetL1Sender(l1Sender)
	}

	// Run headless or TUI mode
	if c.Bool("headless") {
		// Headless mode: block until signal
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		metrics.AppendLog("[headless] running without TUI, send SIGINT/SIGTERM to stop")

		// Trigger a single withdrawal for testing
		if l1Sender != nil {
			metrics.AppendLog("[headless] triggering L2 withdrawal...")
			go l1Sender.Withdraw()
		}

		<-sigCh
		metrics.AppendLog("[headless] shutting down...")
		cancel()
		return nil
	}

	// Run the TUI
	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	// If the user chose to save the CSV via :quit, move the temp file
	// to the permanent location.
	if fm, ok := finalModel.(monitor.Model); ok && fm.SaveCSV() && tmpStatsFile != nil {
		tmpName := tmpStatsFile.Name()
		tmpStatsFile.Close()
		if rerr := os.Rename(tmpName, statsPath); rerr != nil {
			// Rename may fail across filesystems; fall back to copy.
			if cerr := copyFile(tmpName, statsPath); cerr != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not save stats CSV to %s: %v\n", statsPath, cerr)
			} else {
				os.Remove(tmpName)
				fmt.Fprintf(os.Stderr, "Stats CSV saved to %s\n", statsPath)
			}
		} else {
			fmt.Fprintf(os.Stderr, "Stats CSV saved to %s\n", statsPath)
		}
	}

	cancel()
	return nil
}

// warnIfLocalDefault emits one stderr warning when an endpoint resolved to a
// loopback (local dev) address without the user explicitly setting its flag/env
// — so attaching to the local stack is never silent. Explicitly-set values
// (including an intentional localhost) are left unwarned.
func warnIfLocalDefault(c *cli.Context, flag, name, val string) {
	if val == "" || c.IsSet(flag) {
		return
	}
	if strings.Contains(val, "127.0.0.1") || strings.Contains(val, "localhost") {
		fmt.Fprintf(os.Stderr, "WARN: %s resolved to local default %s; pass --%s (or --addresses) to attach to a remote chain\n", name, val, flag)
	}
}

// copyFile copies the contents of src to dst, creating or truncating dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
