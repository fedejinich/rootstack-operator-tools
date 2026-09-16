package remote

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// Config defines the remote infrastructure configuration.
type Config struct {
	Servers []ServerConfig `toml:"servers"`
}

// ServerConfig defines a single remote server.
type ServerConfig struct {
	Name     string   `toml:"name"`     // Display name (e.g., "sequencer")
	Host     string   `toml:"host"`     // SSH host (from ~/.ssh/config or user@host)
	Role     string   `toml:"role"`     // Role: "sequencer", "batcher", "experimenter"
	User     string   `toml:"user"`     // SSH user (optional, defaults to current user)
	Tunnels  []string `toml:"tunnels"`  // Expected tunnels for status checking
	Services []string `toml:"services"` // tmux session names to monitor
	LogPath  string   `toml:"log_path"` // Path to log file on remote server
}

// LoadConfig loads configuration from a TOML file.
func LoadConfig(path string) (*Config, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("config file not found: %s\n\nCreate one with:\n%s", path, DefaultConfigContent())
	}

	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("no servers defined in config")
	}

	for i, s := range cfg.Servers {
		if s.Name == "" {
			return nil, fmt.Errorf("server %d: name is required", i)
		}
		if s.Host == "" {
			return nil, fmt.Errorf("server %s: host is required", s.Name)
		}
	}

	return &cfg, nil
}

// DefaultConfigContent returns the default configuration file content.
func DefaultConfigContent() string {
	return `# rollup-remote.toml - Configuration for rollup-remote TUI

# Define your remote servers here. Each server should have:
#   - name: A friendly display name
#   - host: SSH host (from ~/.ssh/config or user@hostname)
#   - role: Server role (sequencer, batcher, experimenter)
#   - tunnels: List of expected SSH tunnels (for status display)
#   - services: List of tmux session names to monitor
#   - log_path: Path to the main log file on the remote server

[[servers]]
name = "sequencer"
host = "remote-sequencer"
role = "sequencer"
tunnels = ["4444:rskj1:4444"]
services = ["rollup-sequencer"]
log_path = "~/rollup/logs/sequencer.log"

[[servers]]
name = "batcher"
host = "remote-batcher"
role = "batcher"
tunnels = ["4444:rskj2:4444", "8545:remote-sequencer:8545", "9545:remote-sequencer:9545"]
services = ["rollup-batcher"]
log_path = "~/rollup/logs/batcher.log"

[[servers]]
name = "experimenter"
host = "remote-experimenter"
role = "experimenter"
tunnels = ["8545:remote-sequencer:8545", "9545:remote-sequencer:9545", "7300:remote-batcher:7300", "8548:remote-batcher:8548"]
services = ["rollup-experimenter"]
log_path = "~/rollup/logs/experimenter.log"
`
}

// FindServer finds a server by name in the config.
func (c *Config) FindServer(name string) (*ServerConfig, error) {
	for i := range c.Servers {
		if c.Servers[i].Name == name {
			return &c.Servers[i], nil
		}
	}
	return nil, fmt.Errorf("server not found: %s", name)
}
