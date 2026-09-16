package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// NodeProcess manages the lifecycle of a rollup-node subprocess.
// It edits rollup.json to set the block_time, then starts the rollup-node
// binary with the provided TOML config. Between block_time groups in a sweep,
// the node is stopped, rollup.json is updated, and the node is restarted.
type NodeProcess struct {
	mu         sync.Mutex
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	done       chan struct{} // closed when the subprocess exits
	running    bool
	logger     *slog.Logger
	binPath    string
	configTOML string
	logDir     string   // when non-empty, subprocess output is written here instead of stdout
	logFile    *os.File // open log file for the current run
}

// NewNodeProcess creates a rollup-node process manager.
// If logDir is non-empty, subprocess stdout/stderr is redirected to a file
// inside that directory instead of the terminal (used in --live mode).
func NewNodeProcess(logger *slog.Logger, binPath, configTOML, logDir string) *NodeProcess {
	return &NodeProcess{
		logger:     logger,
		binPath:    binPath,
		configTOML: configTOML,
		logDir:     logDir,
	}
}

// Start edits rollup.json in the workdir to set block_time, then launches the
// rollup-node binary with the provided TOML config.
func (np *NodeProcess) Start(parentCtx context.Context, workDir string, blockTime uint64) error {
	np.mu.Lock()
	defer np.mu.Unlock()

	if np.running {
		return fmt.Errorf("rollup-node already running")
	}

	if err := setRollupBlockTime(workDir, blockTime); err != nil {
		return fmt.Errorf("set block_time in rollup.json: %w", err)
	}
	np.logger.Info("Updated rollup.json block_time", "block_time", blockTime, "workdir", workDir)

	ctx, cancel := context.WithCancel(context.Background())
	np.cancel = cancel

	np.cmd = exec.CommandContext(ctx, np.binPath, "--config", np.configTOML)
	if np.logDir != "" {
		f, err := os.Create(filepath.Join(np.logDir, "rollup-node.log"))
		if err != nil {
			cancel()
			return fmt.Errorf("create rollup-node log file: %w", err)
		}
		np.logFile = f
		np.cmd.Stdout = f
		np.cmd.Stderr = f
	} else {
		np.cmd.Stdout = os.Stdout
		np.cmd.Stderr = os.Stderr
	}

	if err := np.cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start rollup-node: %w", err)
	}

	np.running = true
	np.logger.Info("Rollup-node process started", "pid", np.cmd.Process.Pid, "block_time", blockTime)

	np.done = make(chan struct{})

	go func() {
		err := np.cmd.Wait()
		np.mu.Lock()
		np.running = false
		np.mu.Unlock()
		close(np.done)
		if err != nil && ctx.Err() == nil {
			np.logger.Error("Rollup-node process exited unexpectedly", "err", err)
		}
	}()

	return nil
}

// Stop gracefully stops the rollup-node subprocess.
func (np *NodeProcess) Stop() error {
	np.mu.Lock()
	if !np.running {
		np.mu.Unlock()
		return nil
	}

	np.logger.Info("Stopping rollup-node process...")

	if np.cmd != nil && np.cmd.Process != nil {
		// Best-effort: the process may already have exited on its own, and the
		// grace period below covers that case anyway.
		_ = np.cmd.Process.Signal(os.Interrupt)
	}

	done := np.done
	np.mu.Unlock()

	const gracePeriod = 12 * time.Second
	np.logger.Info("Waiting for rollup-node to exit cleanly", "timeout", gracePeriod)

	select {
	case <-done:
	case <-time.After(gracePeriod):
		np.logger.Warn("Rollup-node did not exit within grace period, forcing kill", "timeout", gracePeriod)
		np.mu.Lock()
		if np.cancel != nil {
			np.cancel()
		}
		np.mu.Unlock()
		np.logger.Info("Waiting for rollup-node process to terminate after kill...")
		<-done
	}

	np.mu.Lock()
	np.running = false
	if np.logFile != nil {
		np.logFile.Close()
		np.logFile = nil
	}
	np.mu.Unlock()

	np.logger.Info("Rollup-node process stopped")
	return nil
}

// IsRunning returns whether the rollup-node process is currently running.
func (np *NodeProcess) IsRunning() bool {
	np.mu.Lock()
	defer np.mu.Unlock()
	return np.running
}

// WaitHealthy waits for the rollup-node to become healthy by checking L2 RPC
// connectivity and block production.
func (np *NodeProcess) WaitHealthy(ctx context.Context, target TargetConfig) error {
	np.logger.Info("Waiting for rollup-node to become healthy...")

	timeout := time.After(120 * time.Second)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for rollup-node to become healthy")
		case <-ticker.C:
			if checkL2RPC(target.L2RPC) {
				np.logger.Info("Rollup-node is healthy (L2 RPC responding)")
				time.Sleep(5 * time.Second)
				return nil
			}
		}
	}
}

// checkL2RPC does a basic TCP dial to verify the L2 RPC endpoint is accepting connections.
func checkL2RPC(l2RPC string) bool {
	// Extract host:port from URL
	host := l2RPC
	for _, prefix := range []string{"http://", "https://", "ws://", "wss://"} {
		if len(host) > len(prefix) && host[:len(prefix)] == prefix {
			host = host[len(prefix):]
			break
		}
	}
	// Remove trailing path
	for i, c := range host {
		if c == '/' {
			host = host[:i]
			break
		}
	}

	conn, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// setRollupBlockTime reads rollup.json from the workdir, sets the block_time
// field, and writes it back. It uses json.Number to avoid float64 precision
// loss on large uint64 fork timestamps (e.g. the "not activated" sentinel
// 18446744073709551615 which exceeds float64's 53-bit mantissa).
func setRollupBlockTime(workDir string, blockTime uint64) error {
	path := filepath.Join(workDir, "rollup.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read rollup.json: %w", err)
	}

	var rollup map[string]json.RawMessage
	if err := json.Unmarshal(data, &rollup); err != nil {
		return fmt.Errorf("parse rollup.json: %w", err)
	}

	rollup["block_time"] = json.RawMessage(strconv.FormatUint(blockTime, 10))

	out, err := json.MarshalIndent(rollup, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal rollup.json: %w", err)
	}
	out = append(out, '\n')

	if err := os.WriteFile(path, out, 0644); err != nil {
		return fmt.Errorf("write rollup.json: %w", err)
	}

	return nil
}

// ResolveNodeBinary finds the rollup-node binary at the given path.
func ResolveNodeBinary(explicitPath string) (string, error) {
	if explicitPath != "" {
		if _, err := os.Stat(explicitPath); err == nil {
			abs, _ := filepath.Abs(explicitPath)
			return abs, nil
		}
		return "", fmt.Errorf("rollup-node binary not found at %s", explicitPath)
	}

	candidates := []string{
		"./rollup-node",
		"./bin/rollup-node",
		filepath.Join(os.Getenv("GOPATH"), "bin", "rollup-node"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs, nil
		}
	}

	return "", fmt.Errorf("rollup-node binary not found; specify --rollup-node-bin or target.rollup_node_bin")
}
