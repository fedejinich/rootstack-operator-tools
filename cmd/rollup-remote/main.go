package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fedejinich/rootstack-operator-tools/cmd/rollup-remote/remote"
	"github.com/urfave/cli/v2"
)

const defaultConfigPath = "rollup-remote.toml"

func main() {
	app := &cli.App{
		Name:  "rollup-remote",
		Usage: "TUI control panel for distributed rollup infrastructure",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Usage:   "Path to TOML config file",
				Value:   defaultConfigPath,
				EnvVars: []string{"ROLLUP_REMOTE_CONFIG"},
			},
		},
		Commands: []*cli.Command{
			{
				Name:   "status",
				Usage:  "Show status of all servers (non-interactive)",
				Action: runStatus,
			},
			{
				Name:      "start",
				Usage:     "Start services on a server",
				ArgsUsage: "<server>",
				Action:    runStart,
			},
			{
				Name:      "stop",
				Usage:     "Stop services on a server",
				ArgsUsage: "<server>",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:  "all",
						Usage: "Stop services on all servers",
					},
				},
				Action: runStop,
			},
			{
				Name:  "logs",
				Usage: "Tail logs from servers",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:    "follow",
						Aliases: []string{"f"},
						Usage:   "Follow log output",
					},
					&cli.StringFlag{
						Name:  "server",
						Usage: "Server to tail logs from (default: all)",
					},
				},
				Action: runLogs,
			},
			{
				Name:      "exec",
				Usage:     "Execute command on a server",
				ArgsUsage: "<server> <command>",
				Action:    runExec,
			},
		},
		Action: runTUI,
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runTUI(c *cli.Context) error {
	configPath := c.String("config")

	cfg, err := remote.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	m := remote.NewModel(cfg)

	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	return nil
}

func runStatus(c *cli.Context) error {
	configPath := c.String("config")

	cfg, err := remote.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	return remote.PrintStatus(cfg)
}

func runStart(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("usage: rollup-remote start <server>")
	}

	configPath := c.String("config")
	serverName := c.Args().Get(0)

	cfg, err := remote.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	return remote.StartServer(cfg, serverName)
}

func runStop(c *cli.Context) error {
	configPath := c.String("config")

	cfg, err := remote.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if c.Bool("all") {
		return remote.StopAllServers(cfg)
	}

	if c.NArg() < 1 {
		return fmt.Errorf("usage: rollup-remote stop <server> or --all")
	}

	serverName := c.Args().Get(0)
	return remote.StopServer(cfg, serverName)
}

func runLogs(c *cli.Context) error {
	configPath := c.String("config")

	cfg, err := remote.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	return remote.TailLogs(cfg, c.String("server"), c.Bool("follow"))
}

func runExec(c *cli.Context) error {
	if c.NArg() < 2 {
		return fmt.Errorf("usage: rollup-remote exec <server> <command>")
	}

	configPath := c.String("config")
	serverName := c.Args().Get(0)
	command := c.Args().Get(1)

	cfg, err := remote.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	return remote.ExecCommand(cfg, serverName, command)
}
