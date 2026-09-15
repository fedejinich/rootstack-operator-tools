# rollup-remote TUI Architecture

A terminal-based control panel for managing distributed rollup infrastructure across isolated servers.

## Repository ownership

This command is built in `rootstack-operator-tools`. It is an SSH client for services that are built and deployed in the rootstack repository; it does not build, deploy, or replace those services. The experiment runner's rollup-node lifecycle code is separate legacy/local-only functionality, not multi-host support.

## Overview

The diagrams and design sketches below are conceptual; the extracted file list and implementation are authoritative.

`rollup-remote` provides a unified interface to:
- Connect to all rollup servers via SSH (one-time YubiKey auth at startup)
- Monitor status of tunnels and services across all nodes
- Start/stop configured tmux services remotely
- Fetch recent logs from configured remote paths
- Execute commands on a specific server

## Core Design

### SSH Connection Management

```text
┌─────────────────────────────────────────────────────────────────┐
│                     rollup-remote TUI                            │
│                                                                  │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │                  SSH Agent (YubiKey)                      │   │
│  │                                                           │   │
│  │  Authenticates once at startup, maintains connections     │   │
│  └──────────────────────────────────────────────────────────┘   │
│                              │                                   │
│         ┌────────────────────┼────────────────────┐             │
│         │                    │                    │             │
│         ▼                    ▼                    ▼             │
│  ┌─────────────┐     ┌─────────────┐     ┌─────────────┐        │
│  │  Sequencer  │     │   Batcher   │     │ Experimenter│        │
│  │  Connection │     │  Connection │     │  Connection │        │
│  │             │     │             │     │             │        │
│  │ - Status    │     │ - Status    │     │ - Status    │        │
│  │ - Logs      │     │ - Logs      │     │ - Logs      │        │
│  │ - Commands  │     │ - Commands  │     │ - Commands  │        │
│  └─────────────┘     └─────────────┘     └─────────────┘        │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

### UI Layout

```text
┌─────────────────────────────────────────────────────────────────┐
│  rollup-remote                                     [q]uit [?]help│
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  ┌─ Servers ────────────────────────────────────────────────┐   │
│  │  [1] sequencer    ● RUNNING   tunnels: 1/1   cpu: 45%    │   │
│  │  [2] batcher      ● RUNNING   tunnels: 2/2   cpu: 12%    │   │
│  │  [3] experimenter ○ STOPPED   tunnels: 0/2   cpu: --     │   │
│  └──────────────────────────────────────────────────────────┘   │
│                                                                  │
│  ┌─ Services (sequencer) ───────────────────────────────────┐   │
│  │  rollup-node      ● RUNNING   uptime: 2d 4h   mem: 1.2GB │   │
│  │  autossh-rskj1    ● RUNNING   port: 4444                  │   │
│  └──────────────────────────────────────────────────────────┘   │
│                                                                  │
│  ┌─ Logs (sequencer) ───────────────────────────────────────┐   │
│  │  14:32:01 [INFO] Derived block #1234 from L1 #5678       │   │
│  │  14:32:03 [INFO] Sequenced block #1235                   │   │
│  │  14:32:05 [INFO] Derived block #1235 from L1 #5679       │   │
│  │  ...                                                      │   │
│  └──────────────────────────────────────────────────────────┘   │
│                                                                  │
│  Commands: [s]tart  s[t]op  [r]efresh  [l]ogs  [?]help  [q]uit   │
└─────────────────────────────────────────────────────────────────┘
```

### Component Architecture

The extracted command currently consists of these files:

```text
cmd/rollup-remote/
├── main.go                  # CLI entrypoint and subcommands
├── ARCHITECTURE.md          # This document
└── remote/
    ├── commands.go          # status, start, stop, logs and exec operations
    ├── config.go            # TOML configuration loading
    ├── model.go             # Bubble Tea TUI model and views
    ├── server.go            # Remote server/service status
    └── ssh.go               # SSH agent connections and command execution
```

Tunnel and service operations are implemented by `server.go` and `commands.go`; the design does not claim separate `tunnel.go`, `service.go`, `logs.go`, or `views/` packages. The remote binaries and tmux services themselves are built by rootstack or the deployment environment, not this repository.

## Key Types

```go
// Config defines the remote infrastructure configuration
type Config struct {
    Servers []ServerConfig `toml:"servers"`
}

type ServerConfig struct {
    Name     string   `toml:"name"`      // Display name
    Host     string   `toml:"host"`      // SSH host and optional port
    Role     string   `toml:"role"`      // Remote launcher role
    User     string   `toml:"user"`      // Optional SSH user
    Tunnels  []string `toml:"tunnels"`   // Expected tunnels for status checks
    Services []string `toml:"services"`  // tmux session names to monitor
    LogPath  string   `toml:"log_path"`  // Remote log file for tailing
}

// Server represents a connected remote server
type Server struct {
    Config   *ServerConfig
    SSH      *SSHClient
    Status   ServerStatus
    Error    string
    Tunnels  []TunnelStatus
    Services []ServiceStatus
    CPU      float64
}

type ServerStatus int
const (
    StatusDisconnected ServerStatus = iota
    StatusConnecting
    StatusConnected
    StatusError
)

type TunnelStatus struct {
    Spec    string // e.g. "4444:rskj1:4444"
    Running bool
}

type ServiceStatus struct {
    Name    string
    Running bool
    Windows int
}
```

## Features

### Phase 1: Core Functionality
- [x] SSH connection management with agent forwarding
- [x] Server status overview
- [x] Tunnel status monitoring
- [x] tmux session status
- [x] Basic start/stop commands

### Phase 2: Log Aggregation
- [x] Fetch recent log lines from a configured remote log path
- [ ] Real-time log streaming from all servers (`logs --follow` currently reports that it is not implemented)
- [ ] Log filtering and search
- [ ] Log level highlighting
- [ ] Export logs to file

### Phase 3: Advanced Features
- [ ] Metrics dashboard (integrate with Prometheus endpoints)
- [ ] Alerts for service failures
- [ ] Configuration editing
- [ ] Deployment automation

## Configuration File

```toml
# rollup-remote.toml

[[servers]]
name = "sequencer"
host = "remote-sequencer"
role = "sequencer"
tunnels = ["4444:rskj1:4444"]
services = ["rollup-sequencer"]

[[servers]]
name = "batcher"
host = "remote-batcher"
role = "batcher"
tunnels = ["4444:rskj2:4444", "8545:sequencer:8545", "9545:sequencer:9545"]
services = ["rollup-batcher"]

[[servers]]
name = "experimenter"
host = "remote-experimenter"
role = "experimenter"
tunnels = ["8545:sequencer:8545", "9545:sequencer:9545", "7300:batcher:7300", "8548:batcher:8548"]
services = ["rollup-experimenter"]
```

## Usage

```bash
# Start the TUI (connects to all servers)
rollup-remote

# Connect with custom config
rollup-remote --config /path/to/rollup-remote.toml

# List servers and their status (non-interactive)
rollup-remote status

# Start a specific service
rollup-remote start sequencer

# Stop all services
rollup-remote stop --all

# Tail logs from all servers
rollup-remote logs --follow

# Execute command on a server
rollup-remote exec sequencer "tmux list-sessions"
```

## Implementation Notes

### SSH Agent Integration

The tool uses Go's `x/crypto/ssh` package with SSH agent forwarding:

```go
// Connect using SSH agent (supports YubiKey)
func connectWithAgent(host string) (*ssh.Client, error) {
    socket := os.Getenv("SSH_AUTH_SOCK")
    conn, err := net.Dial("unix", socket)
    if err != nil {
        return nil, fmt.Errorf("SSH agent not available: %w", err)
    }

    agentClient := agent.NewClient(conn)

    config := &ssh.ClientConfig{
        User: username,
        Auth: []ssh.AuthMethod{
            ssh.PublicKeysCallback(agentClient.Signers),
        },
        HostKeyCallback: ssh.InsecureIgnoreHostKey(), // or use known_hosts
    }

    return ssh.Dial("tcp", host+":22", config)
}
```

### Tunnel Status Detection

Check if tunnels are running by looking for autossh processes:

```go
func (s *Server) CheckTunnels() error {
    session, err := s.Client.NewSession()
    if err != nil {
        return err
    }
    defer session.Close()

    output, err := session.CombinedOutput("pgrep -af autossh")
    // Parse output to determine which tunnels are active
    ...
}
```

### tmux Session Management

```go
func (s *Server) ListSessions() ([]string, error) {
    output, err := s.RunCommand("tmux list-sessions -F '#{session_name}'")
    ...
}

func (s *Server) AttachSession(name string) error {
    // Create PTY session for interactive attach
    ...
}
```
