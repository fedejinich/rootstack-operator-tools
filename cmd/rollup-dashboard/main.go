package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fedejinich/rootstack-operator-tools/cmd/rollup-dashboard/dashboard"
	"github.com/fedejinich/rootstack-operator-tools/rsk/chainattach"
	"github.com/urfave/cli/v2"
)

func main() {
	app := &cli.App{
		Name:  "rollup-dashboard",
		Usage: "Interactive TUI for running and managing a rollup node",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Usage:   "Path to rollup-node TOML config file",
				EnvVars: []string{"ROLLUP_NODE_CONFIG"},
			},
			&cli.StringFlag{
				Name:    "rollup-node-bin",
				Usage:   "Path to rollup-node binary",
				Value:   "rollup-node",
				EnvVars: []string{"ROLLUP_NODE_BIN"},
			},
			&cli.BoolFlag{
				Name:    "attach",
				Aliases: []string{"a"},
				Usage:   "Attach to an already-running rollup-node (don't start a subprocess)",
				EnvVars: []string{"ROLLUP_DASHBOARD_ATTACH"},
			},
			// Explicit endpoint overrides for --attach mode (point at a remote node).
			// Suffixed env names (matching rollup-monitor) to avoid colliding with
			// the dev stack's pervasively-exported L1_RPC/L2_RPC (which point at
			// container-internal hosts unreachable from a host-side TUI).
			&cli.StringFlag{Name: "l1-rpc", Usage: "L1 RPC URL (attach mode; overrides config)", EnvVars: []string{"L1_RPC_URL"}},
			&cli.StringFlag{Name: "l2-rpc", Usage: "L2 execution RPC URL (attach mode; overrides config)", EnvVars: []string{"L2_RPC_URL"}},
			&cli.StringFlag{Name: "node-rpc", Usage: "op-node RPC URL (attach mode; overrides config)", EnvVars: []string{"NODE_RPC_URL"}},
			&cli.StringFlag{Name: "batcher-rpc", Usage: "op-batcher admin RPC URL (attach mode; overrides config)", EnvVars: []string{"BATCHER_RPC_URL"}},
			&cli.StringFlag{Name: "node-metrics", Usage: "op-node Prometheus metrics URL (overrides config)"},
			&cli.StringFlag{Name: "batcher-metrics", Usage: "op-batcher Prometheus metrics URL (overrides config)"},
			&cli.StringFlag{Name: "proposer-metrics", Usage: "op-proposer Prometheus metrics URL (overrides config)"},
			&cli.IntFlag{
				Name:  "log-buffer",
				Usage: "Log ring buffer capacity",
				Value: 10000,
			},
		},
		Action: runDashboard,
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runDashboard(cliCtx *cli.Context) error {
	configPath := cliCtx.String("config")
	binPath := cliCtx.String("rollup-node-bin")

	// Resolve config path to absolute
	if configPath != "" {
		absPath, err := filepath.Abs(configPath)
		if err != nil {
			return fmt.Errorf("resolve config path: %w", err)
		}
		configPath = absPath
	}

	// Auto-discover config if not specified
	if configPath == "" {
		candidates := []string{
			"rollup-node.toml",
			"rollup-node_*.toml",
		}
		for _, pattern := range candidates {
			matches, _ := filepath.Glob(pattern)
			if len(matches) > 0 {
				absPath, _ := filepath.Abs(matches[0])
				configPath = absPath
				break
			}
		}
	}

	// Load configuration from rollup-node TOML
	cfg, err := dashboard.LoadConfig(binPath, configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if cliCtx.IsSet("log-buffer") {
		cfg.LogBufferSize = cliCtx.Int("log-buffer")
	}

	attachMode := cliCtx.Bool("attach")

	// Endpoint/metrics overrides make --attach first-class: an operator
	// monitoring an already-running (possibly remote) node points every endpoint
	// at it (the metrics URLs especially, since a remote node's local TOML won't
	// have [*.metrics].enabled reflected here). These apply in ATTACH MODE ONLY —
	// in supervised mode the endpoints come from the rollup-node TOML, and
	// applying a routinely-exported env var here would silently break the health
	// panes.
	if attachMode {
		for _, o := range []struct {
			flag string
			dst  *string
		}{
			{"l1-rpc", &cfg.L1RPC},
			{"l2-rpc", &cfg.L2RPC},
			{"node-rpc", &cfg.NodeRPC},
			{"batcher-rpc", &cfg.BatcherRPC},
			{"node-metrics", &cfg.NodeMetricsURL},
			{"batcher-metrics", &cfg.BatcherMetricsURL},
			{"proposer-metrics", &cfg.ProposerMetricsURL},
		} {
			if cliCtx.IsSet(o.flag) {
				*o.dst = cliCtx.String(o.flag)
			}
		}
		if cfg.L1RPC == "" {
			cfg.L1RPC = chainattach.ResolveEndpoint("L1 RPC", "", "", chainattach.LocalDefaults.L1RPC, warnf)
		}
		warnIfLocalDefault(cliCtx, "l2-rpc", "L2 RPC", cfg.L2RPC)
		warnIfLocalDefault(cliCtx, "node-rpc", "op-node RPC", cfg.NodeRPC)
	}

	// Load current batcher settings from config
	batcherSettings, err := dashboard.LoadBatcherSettings(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not load batcher settings: %v (using defaults)\n", err)
		batcherSettings.SetDefaults()
	}

	// Use a plain cancellable context. Bubble Tea handles Ctrl+C / SIGINT
	// natively as a key event, so we must not steal it with signal.NotifyContext.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create the TUI model
	model := dashboard.NewModel(ctx, cfg, batcherSettings, attachMode)

	// Run the Bubble Tea program
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	return nil
}

func warnf(f string, a ...any) { fmt.Fprintf(os.Stderr, f, a...) }

// warnIfLocalDefault emits one stderr warning when an attach-mode endpoint
// resolved to a loopback address without the user setting its flag/env, so
// attaching to the local stack is never silent.
func warnIfLocalDefault(c *cli.Context, flag, name, val string) {
	if val == "" || c.IsSet(flag) {
		return
	}
	if strings.Contains(val, "127.0.0.1") || strings.Contains(val, "localhost") {
		fmt.Fprintf(os.Stderr, "WARN: %s resolved to local default %s; pass --%s to attach to a remote node\n", name, val, flag)
	}
}
