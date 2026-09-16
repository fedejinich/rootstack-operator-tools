package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultBatcherRPCURL  = "http://127.0.0.1:8548"
	standaloneBatcherPort = "8549"
)

// BatcherProcess manages the lifecycle of a standalone batcher subprocess.
// It writes a temporary TOML configuration, starts the batcher binary,
// and provides clean stop/restart semantics between experiment runs.
//
// When an embedded batcher is running inside the rollup-node, the process
// stops it via the admin RPC before starting the standalone subprocess,
// and restarts it when the experiment is done.
type BatcherProcess struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	done    chan struct{} // closed when the subprocess exits
	tmpTOML string        // path to the temp TOML config file
	running bool
	logger  *slog.Logger
	binPath string   // path to the batcher binary
	logDir  string   // when non-empty, subprocess output is written here instead of stdout
	logFile *os.File // open log file for the current run

	metricsURL      string // batcher Prometheus metrics URL (parsed for --metrics.addr/port)
	embeddedRPCURL  string // admin RPC URL for the embedded batcher inside rollup-node
	stoppedEmbedded bool   // true if we stopped the embedded batcher and should restart it
}

// NewBatcherProcess creates a batcher process manager.
// If logDir is non-empty, subprocess stdout/stderr is redirected to a file
// inside that directory instead of the terminal (used in --live mode).
//
// metricsURL is the Prometheus endpoint the batcher should expose (e.g.
// "http://127.0.0.1:7303/metrics"). The addr and port are extracted and
// passed as --metrics.* flags to the subprocess.
//
// embeddedRPCURL is the admin RPC of a batcher already running inside the
// rollup-node. Before starting the standalone subprocess, the embedded
// batcher is stopped via admin_stopBatcher so the two don't conflict.
func NewBatcherProcess(logger *slog.Logger, binPath, logDir, metricsURL, embeddedRPCURL string) *BatcherProcess {
	if embeddedRPCURL == "" {
		embeddedRPCURL = defaultBatcherRPCURL
	}
	return &BatcherProcess{
		logger:         logger,
		binPath:        binPath,
		logDir:         logDir,
		metricsURL:     metricsURL,
		embeddedRPCURL: embeddedRPCURL,
	}
}

// Start launches the batcher subprocess with the given configuration.
// If an embedded batcher is running inside the rollup-node, it is stopped
// via admin RPC first.
func (bp *BatcherProcess) Start(parentCtx context.Context, target TargetConfig, run BatcherRunConfig) error {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	if bp.running {
		return fmt.Errorf("batcher already running")
	}

	// Stop any embedded batcher running inside the rollup-node so ports
	// don't conflict and we control the batcher configuration.
	if bp.embeddedRPCURL != "" {
		if err := stopBatcherRPC(parentCtx, bp.embeddedRPCURL); err != nil {
			bp.logger.Debug("Could not stop embedded batcher via RPC (may not be running)", "err", err)
		} else {
			bp.logger.Info("Stopped embedded batcher via admin RPC", "url", bp.embeddedRPCURL)
			bp.stoppedEmbedded = true
			time.Sleep(1 * time.Second)
		}
	}

	// Write temporary TOML config for the batcher
	tmpFile, err := os.CreateTemp("", "experiment-batcher-*.toml")
	if err != nil {
		return fmt.Errorf("create temp batcher config: %w", err)
	}
	bp.tmpTOML = tmpFile.Name()

	tomlContent := fmt.Sprintf(`# Auto-generated batcher config for experiment run
[batcher]
max_channel_duration = %d
max_l1_tx_size = %d
max_blocks_per_span_batch = %d
max_pending_transactions = %d
batch_type = %d
compression_algo = "%s"
sub_safety_margin = %d
data_availability_type = "calldata"
compressor = "shadow"
approx_compr_ratio = 0.4
target_num_frames = 1
poll_interval = "2s"

[batcher.rpc]
listen_addr = "127.0.0.1"
listen_port = %s
enable_admin = true

[batcher.throttle]
controller_type = "step"
lower_threshold = 0
upper_threshold = 0
`,
		run.MaxChannelDuration,
		run.MaxL1TxSize,
		run.MaxBlocksPerSpanBatch,
		run.MaxPendingTx,
		run.BatchType,
		run.CompressionAlgo,
		run.SubSafetyMargin,
		standaloneBatcherPort,
	)

	if _, err := tmpFile.WriteString(tomlContent); err != nil {
		tmpFile.Close()
		os.Remove(bp.tmpTOML)
		return fmt.Errorf("write batcher config: %w", err)
	}
	tmpFile.Close()

	bp.logger.Info("Batcher config written",
		"path", bp.tmpTOML,
		"max_l1_tx_size", run.MaxL1TxSize,
		"max_channel_duration", run.MaxChannelDuration,
		"max_blocks_per_span_batch", run.MaxBlocksPerSpanBatch,
		"max_pending_tx", run.MaxPendingTx,
	)

	// Resolve private key
	privateKey := target.PrivateKey
	if privateKey != "" && privateKey[0] != '0' {
		privateKey = "0x" + privateKey
	}

	// Parse metrics addr/port from the configured URL.
	metricsAddr, metricsPort := parseMetricsHostPort(bp.metricsURL)

	// Build command args using upstream op-batcher flag names.
	args := []string{
		"--l1-eth-rpc", target.L1RPC,
		"--l2-eth-rpc", target.L2RPC,
		"--rollup-rpc", target.NodeRPC,
		"--private-key", privateKey,
		"--max-l1-tx-size-bytes", fmt.Sprintf("%d", run.MaxL1TxSize),
		"--max-channel-duration", fmt.Sprintf("%d", run.MaxChannelDuration),
		"--max-blocks-per-span-batch", fmt.Sprintf("%d", run.MaxBlocksPerSpanBatch),
		"--max-pending-tx", fmt.Sprintf("%d", run.MaxPendingTx),
		"--batch-type", fmt.Sprintf("%d", run.BatchType),
		"--compression-algo", run.CompressionAlgo,
		"--sub-safety-margin", fmt.Sprintf("%d", run.SubSafetyMargin),
		"--data-availability-type", "calldata",
		"--rpc.addr", "127.0.0.1",
		"--rpc.port", standaloneBatcherPort,
		"--rpc.enable-admin",
		"--metrics.enabled",
		"--metrics.addr", metricsAddr,
		"--metrics.port", metricsPort,
		"--throttle.unsafe-da-bytes-lower-threshold", "0",
		"--txmgr.use-legacy-tx",
	}

	ctx, cancel := context.WithCancel(context.Background())
	bp.cancel = cancel

	bp.cmd = exec.CommandContext(ctx, bp.binPath, args...)
	if bp.logDir != "" {
		f, err := os.Create(filepath.Join(bp.logDir, "batcher.log"))
		if err != nil {
			cancel()
			os.Remove(bp.tmpTOML)
			return fmt.Errorf("create batcher log file: %w", err)
		}
		bp.logFile = f
		bp.cmd.Stdout = f
		bp.cmd.Stderr = f
	} else {
		bp.cmd.Stdout = os.Stdout
		bp.cmd.Stderr = os.Stderr
	}

	if err := bp.cmd.Start(); err != nil {
		cancel()
		os.Remove(bp.tmpTOML)
		return fmt.Errorf("start batcher: %w", err)
	}

	bp.running = true
	bp.logger.Info("Batcher process started",
		"pid", bp.cmd.Process.Pid,
		"metrics", fmt.Sprintf("%s:%s", metricsAddr, metricsPort),
		"rpc_port", standaloneBatcherPort,
	)

	bp.done = make(chan struct{})

	go func() {
		err := bp.cmd.Wait()
		bp.mu.Lock()
		bp.running = false
		bp.mu.Unlock()
		close(bp.done)
		if err != nil && ctx.Err() == nil {
			bp.logger.Error("Batcher process exited unexpectedly", "err", err)
		}
	}()

	// Release the lock so the background goroutine can update bp.running
	// if the process exits during the health check.
	bp.mu.Unlock()
	err = bp.waitHealthy(parentCtx)
	bp.mu.Lock()
	if err != nil {
		// Process failed health check; clean up. The health-check error is what
		// the caller needs, so a stop failure is only logged.
		bp.mu.Unlock()
		if serr := bp.Stop(); serr != nil {
			bp.logger.Warn("Error stopping batcher after failed health check", "err", serr)
		}
		bp.mu.Lock()
		return err
	}

	return nil
}

// waitHealthy blocks until the batcher process is confirmed healthy.
// It first checks that the process hasn't exited immediately (catches port
// conflicts and similar startup crashes), then probes the metrics endpoint.
func (bp *BatcherProcess) waitHealthy(ctx context.Context) error {
	time.Sleep(2 * time.Second)
	if !bp.IsRunning() {
		logHint := ""
		if bp.logDir != "" {
			logHint = fmt.Sprintf(" (check %s for details)", filepath.Join(bp.logDir, "batcher.log"))
		}
		return fmt.Errorf("batcher process exited immediately%s", logHint)
	}

	if bp.metricsURL == "" {
		return nil
	}

	for attempt := 0; attempt < 3; attempt++ {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		req, err := http.NewRequestWithContext(probeCtx, "GET", bp.metricsURL, nil)
		if err != nil {
			cancel()
			time.Sleep(1 * time.Second)
			continue
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				cancel()
				bp.logger.Info("Batcher health check passed", "metrics_url", bp.metricsURL)
				return nil
			}
		}
		cancel()
		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf("batcher started but metrics endpoint %s not responsive after retries", bp.metricsURL)
}

// Stop gracefully stops the batcher subprocess.
func (bp *BatcherProcess) Stop() error {
	bp.mu.Lock()
	if !bp.running {
		bp.mu.Unlock()
		return nil
	}

	bp.logger.Info("Stopping batcher process...")

	if bp.cmd != nil && bp.cmd.Process != nil {
		// Best-effort: the process may already have exited on its own, and the
		// grace period below covers that case anyway.
		_ = bp.cmd.Process.Signal(os.Interrupt)
	}

	done := bp.done
	bp.mu.Unlock()

	const gracePeriod = 6 * time.Second
	bp.logger.Info("Waiting for batcher to exit cleanly", "timeout", gracePeriod)

	select {
	case <-done:
	case <-time.After(gracePeriod):
		bp.logger.Warn("Batcher did not exit within grace period, forcing kill", "timeout", gracePeriod)
		bp.mu.Lock()
		if bp.cancel != nil {
			bp.cancel()
		}
		bp.mu.Unlock()
		bp.logger.Info("Waiting for batcher process to terminate after kill...")
		<-done
	}

	bp.mu.Lock()
	if bp.tmpTOML != "" {
		os.Remove(bp.tmpTOML)
		bp.tmpTOML = ""
	}
	if bp.logFile != nil {
		bp.logFile.Close()
		bp.logFile = nil
	}
	bp.running = false
	bp.mu.Unlock()

	bp.logger.Info("Batcher process stopped")
	return nil
}

// RestartEmbedded restarts the embedded batcher inside the rollup-node via
// admin RPC, if we stopped it earlier. This should be called after all
// experiment runs are done so the rollup-node resumes normal batching.
func (bp *BatcherProcess) RestartEmbedded() {
	bp.mu.Lock()
	stopped := bp.stoppedEmbedded
	rpcURL := bp.embeddedRPCURL
	bp.mu.Unlock()

	if !stopped || rpcURL == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := startBatcherRPC(ctx, rpcURL); err != nil {
		bp.logger.Warn("Could not restart embedded batcher via RPC", "err", err)
	} else {
		bp.logger.Info("Restarted embedded batcher via admin RPC")
		bp.mu.Lock()
		bp.stoppedEmbedded = false
		bp.mu.Unlock()
	}
}

// IsRunning returns whether the batcher process is currently running.
func (bp *BatcherProcess) IsRunning() bool {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return bp.running
}

// ResolveBatcherBinary finds or builds the batcher binary.
// It first checks if the binary exists at the given path, then tries common locations.
func ResolveBatcherBinary(explicitPath string) (string, error) {
	if explicitPath != "" {
		if _, err := os.Stat(explicitPath); err == nil {
			return explicitPath, nil
		}
		return "", fmt.Errorf("batcher binary not found at %s", explicitPath)
	}

	// Try common locations relative to workspace. The legacy vanilla
	// cmd/batcher was removed; this local-only experiment harness now resolves
	// the production oprsk-batcher (or an explicit --batcher-bin).
	candidates := []string{
		"./batcher",
		"./oprsk-batcher",
		"./cmd/oprsk-batcher/oprsk-batcher",
		filepath.Join(os.Getenv("GOPATH"), "bin", "oprsk-batcher"),
		filepath.Join(os.Getenv("GOPATH"), "bin", "batcher"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs, nil
		}
	}

	return "", fmt.Errorf("batcher binary not found; build it in rootstack with 'go build -o batcher ./cmd/oprsk-batcher' and pass --batcher-bin, or set target.batcher_bin")
}

// parseMetricsHostPort extracts the listen address and port from a Prometheus
// metrics URL like "http://127.0.0.1:7303/metrics".
func parseMetricsHostPort(metricsURL string) (addr, port string) {
	if metricsURL == "" {
		return "0.0.0.0", "7300"
	}
	u, err := url.Parse(metricsURL)
	if err != nil {
		return "0.0.0.0", "7300"
	}
	addr = u.Hostname()
	port = u.Port()
	if addr == "" {
		addr = "0.0.0.0"
	}
	if port == "" {
		port = "7300"
	}
	return
}

// stopBatcherRPC calls admin_stopBatcher on the batcher's JSON-RPC endpoint.
// This gracefully pauses the batch submitter without killing the host process
// (useful when the batcher runs inside the rollup-node).
func stopBatcherRPC(ctx context.Context, rpcURL string) error {
	return batcherAdminRPC(ctx, rpcURL, "admin_stopBatcher")
}

// startBatcherRPC calls admin_startBatcher on the batcher's JSON-RPC endpoint.
func startBatcherRPC(ctx context.Context, rpcURL string) error {
	return batcherAdminRPC(ctx, rpcURL, "admin_startBatcher")
}

// batcherAdminRPC sends a JSON-RPC request to the batcher admin API.
func batcherAdminRPC(ctx context.Context, rpcURL, method string) error {
	reqBody := fmt.Sprintf(`{"jsonrpc":"2.0","method":"%s","params":[],"id":1}`, method)
	req, err := http.NewRequestWithContext(ctx, "POST", rpcURL, strings.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned HTTP %d: %s", method, resp.StatusCode, body)
	}

	var rpcResp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &rpcResp); err == nil && rpcResp.Error != nil {
		return fmt.Errorf("%s: %s (code %d)", method, rpcResp.Error.Message, rpcResp.Error.Code)
	}
	return nil
}
