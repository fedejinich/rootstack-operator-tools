package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/rpc"
	"github.com/fedejinich/rootstack-operator-tools/cmd/rollup-monitor/monitor"
)

// waitForRunStartConditions blocks until all enabled run start conditions are met.
// It polls batcher metrics (pending_blocks) and/or op-node sync status (safe_lag)
// until the values drop below the configured thresholds.
//
// If PauseSequencer is enabled, the sequencer is stopped at the start and restarted
// when conditions are met. This prevents new blocks from being added while draining.
//
// If FlushBatcher is enabled, admin_flushBatcher is called periodically to force
// the batcher to post pending data immediately.
func waitForRunStartConditions(ctx context.Context, logger *slog.Logger, target TargetConfig, cond ResolvedRunStartConditions) error {
	if !cond.HasConditions() {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, cond.Timeout)
	defer cancel()

	// Stop sequencer if configured (prevents new blocks during drain)
	var unsafeHeadHash string
	if cond.PauseSequencer && target.NodeRPC != "" {
		hash, err := StopSequencer(ctx, target.NodeRPC)
		if err != nil {
			logger.Warn("Failed to stop sequencer", "err", err)
		} else {
			unsafeHeadHash = hash
			logger.Info("Sequencer paused for drain", "unsafe_head", hash)
			defer func() {
				if unsafeHeadHash != "" {
					if err := StartSequencer(context.Background(), target.NodeRPC, unsafeHeadHash); err != nil {
						logger.Error("Failed to restart sequencer", "err", err)
					} else {
						logger.Info("Sequencer resumed")
					}
				}
			}()
		}
	}

	// Initial flush if configured
	batcherRPC := target.BatcherRPCURL
	if batcherRPC == "" {
		batcherRPC = "http://127.0.0.1:8548"
	}
	if cond.FlushBatcher {
		if err := flushBatcher(ctx, batcherRPC); err != nil {
			logger.Debug("Failed to flush batcher", "err", err)
		} else {
			logger.Info("Batcher flush initiated")
		}
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	logTicker := time.NewTicker(10 * time.Second)
	defer logTicker.Stop()

	flushTicker := time.NewTicker(30 * time.Second)
	defer flushTicker.Stop()

	logger.Info("Waiting for run start conditions",
		"pending_blocks_target", formatThreshold(cond.PendingBlocks, cond.PendingBlocksAny),
		"safe_lag_target", formatThreshold(cond.SafeLag, cond.SafeLagAny),
		"timeout", cond.Timeout,
		"pause_sequencer", cond.PauseSequencer,
		"flush_batcher", cond.FlushBatcher,
	)

	for {
		pendingOK := cond.PendingBlocksAny
		safeLagOK := cond.SafeLagAny

		var pendingBlocks, safeLag int

		if !cond.PendingBlocksAny && target.BatcherMetricsURL != "" {
			pb, err := getBatcherPendingBlocks(ctx, target.BatcherMetricsURL)
			if err != nil {
				logger.Debug("Failed to get batcher pending blocks", "err", err)
			} else {
				pendingBlocks = pb
				pendingOK = pb <= cond.PendingBlocks
			}
		}

		if !cond.SafeLagAny && target.NodeRPC != "" {
			lag, err := getSafeHeadLag(ctx, target.NodeRPC)
			if err != nil {
				logger.Debug("Failed to get safe head lag", "err", err)
			} else {
				safeLag = lag
				safeLagOK = lag <= cond.SafeLag
			}
		}

		if pendingOK && safeLagOK {
			logger.Info("Run start conditions met",
				"pending_blocks", pendingBlocks,
				"safe_lag", safeLag,
			)
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for run start conditions: pending_blocks=%d (target=%s), safe_lag=%d (target=%s)",
				pendingBlocks, formatThreshold(cond.PendingBlocks, cond.PendingBlocksAny),
				safeLag, formatThreshold(cond.SafeLag, cond.SafeLagAny))
		case <-flushTicker.C:
			if cond.FlushBatcher {
				if err := flushBatcher(ctx, batcherRPC); err != nil {
					logger.Debug("Failed to flush batcher", "err", err)
				}
			}
		case <-logTicker.C:
			logger.Info("Waiting for run start conditions...",
				"pending_blocks", pendingBlocks,
				"pending_blocks_ok", pendingOK,
				"safe_lag", safeLag,
				"safe_lag_ok", safeLagOK,
			)
		case <-ticker.C:
		}
	}
}

// formatThreshold returns a human-readable string for a threshold value.
func formatThreshold(value int, disabled bool) string {
	if disabled {
		return "any"
	}
	return fmt.Sprintf("<=%d", value)
}

// getBatcherPendingBlocks fetches live pending block count from batcher Prometheus metrics
// (pending_blocks_count{stage="added"}, with fallback for unlabeled exporters).
func getBatcherPendingBlocks(ctx context.Context, metricsURL string) (int, error) {
	result, err := monitor.ScrapePrometheus(ctx, metricsURL)
	if err != nil {
		return 0, fmt.Errorf("scrape batcher metrics: %w", err)
	}

	pendingBlocks, ok := result.BatcherPendingBlocksCount()
	if !ok {
		return 0, fmt.Errorf("pending_blocks_count metric not found")
	}

	return int(pendingBlocks), nil
}

// getBatcherDrainMetrics fetches all metrics needed to determine if the batcher is fully drained.
// Returns pending blocks count, pending bytes, and blocks added to current channel.
func getBatcherDrainMetrics(ctx context.Context, metricsURL string) (pendingBlocks, pendingBytes, blocksAdded int, err error) {
	result, err := monitor.ScrapePrometheus(ctx, metricsURL)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("scrape batcher metrics: %w", err)
	}

	pb, _ := result.BatcherPendingBlocksCount()
	pby, _ := result.Get("pending_blocks_bytes_current")
	ba, _ := result.Get("blocks_added_count")

	return int(pb), int(pby), int(ba), nil
}

// WaitForBatcherDrain waits for the batcher to drain before starting a new run.
//
// If sequencerPaused is true (no new blocks arriving), it checks all three conditions:
//   - pending_blocks_count = 0 (no blocks waiting to be added to channel)
//   - pending_blocks_bytes_current = 0 (no bytes in pending stage)
//   - blocks_added_count = 0 (no blocks in current channel being built)
//
// If sequencerPaused is false (blocks still arriving), it only checks:
//   - blocks_added_count = 0 (current channel has been posted)
//
// This ensures data from previous runs is posted before starting the next run.
// It flushes the batcher periodically to speed up draining.
func WaitForBatcherDrain(ctx context.Context, logger *slog.Logger, batcherMetricsURL, batcherRPCURL string, sequencerPaused bool, timeout time.Duration) error {
	if batcherMetricsURL == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Initial flush to start draining
	if batcherRPCURL != "" {
		if err := flushBatcher(ctx, batcherRPCURL); err != nil {
			logger.Debug("Initial flush failed", "err", err)
		}
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	flushTicker := time.NewTicker(10 * time.Second)
	defer flushTicker.Stop()

	var lastPendingBlocks, lastPendingBytes, lastBlocksAdded int

	for {
		pendingBlocks, pendingBytes, blocksAdded, err := getBatcherDrainMetrics(ctx, batcherMetricsURL)
		if err != nil {
			logger.Debug("Failed to get drain metrics", "err", err)
		} else if sequencerPaused && pendingBlocks == 0 && pendingBytes == 0 && blocksAdded == 0 {
			// Full drain: sequencer paused, all metrics at zero
			logger.Info("Batcher fully drained",
				"pending_blocks", 0,
				"pending_bytes", 0,
				"blocks_added", 0)
			return nil
		} else if !sequencerPaused && blocksAdded == 0 {
			// Partial drain: sequencer running, just ensure channel is posted
			logger.Info("Batcher channel drained",
				"blocks_added", 0,
				"pending_blocks", pendingBlocks,
				"pending_bytes", pendingBytes)
			return nil
		} else {
			logger.Debug("Waiting for batcher drain",
				"sequencer_paused", sequencerPaused,
				"pending_blocks", pendingBlocks,
				"pending_bytes", pendingBytes,
				"blocks_added", blocksAdded)
			lastPendingBlocks = pendingBlocks
			lastPendingBytes = pendingBytes
			lastBlocksAdded = blocksAdded
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for batcher drain: pending_blocks=%d, pending_bytes=%d, blocks_added=%d",
				lastPendingBlocks, lastPendingBytes, lastBlocksAdded)
		case <-flushTicker.C:
			if batcherRPCURL != "" {
				if err := flushBatcher(ctx, batcherRPCURL); err != nil {
					logger.Debug("Flush failed", "err", err)
				}
			}
		case <-ticker.C:
		}
	}
}

// syncStatusResult holds the relevant fields from optimism_syncStatus response.
type syncStatusResult struct {
	UnsafeL2 struct {
		Number uint64 `json:"number"`
	} `json:"unsafe_l2"`
	SafeL2 struct {
		Number uint64 `json:"number"`
	} `json:"safe_l2"`
}

// getSafeHeadLag fetches the gap between unsafe and safe L2 heads from op-node.
func getSafeHeadLag(ctx context.Context, nodeRPC string) (int, error) {
	client, err := rpc.DialContext(ctx, nodeRPC)
	if err != nil {
		return 0, fmt.Errorf("dial op-node: %w", err)
	}
	defer client.Close()

	var raw json.RawMessage
	err = client.CallContext(ctx, &raw, "optimism_syncStatus")
	if err != nil {
		return 0, fmt.Errorf("call optimism_syncStatus: %w", err)
	}

	var status syncStatusResult
	if err := json.Unmarshal(raw, &status); err != nil {
		return 0, fmt.Errorf("parse syncStatus: %w", err)
	}

	lag := 0
	if status.UnsafeL2.Number > status.SafeL2.Number {
		lag = int(status.UnsafeL2.Number - status.SafeL2.Number)
	}

	return lag, nil
}

// StopSequencer calls admin_stopSequencer on the op-node to pause block production.
// Returns the unsafe head hash needed to restart the sequencer.
// If the sequencer is already stopped, it returns the current unsafe head hash without error.
// After stopping, it verifies no new blocks were produced during the stop operation.
func StopSequencer(ctx context.Context, nodeRPC string) (string, error) {
	client, err := rpc.DialContext(ctx, nodeRPC)
	if err != nil {
		return "", fmt.Errorf("dial op-node: %w", err)
	}
	defer client.Close()

	var hash string
	err = client.CallContext(ctx, &hash, "admin_stopSequencer")
	if err != nil {
		// If sequencer is already stopped, get the current unsafe head hash instead
		if strings.Contains(err.Error(), "sequencer not running") {
			return getUnsafeHeadHash(ctx, client)
		}
		return "", fmt.Errorf("call admin_stopSequencer: %w", err)
	}

	// Verify sequencer actually stopped by checking hash hasn't changed
	// Wait briefly for any in-flight block to complete
	time.Sleep(200 * time.Millisecond)
	verifyHash, err := getUnsafeHeadHash(ctx, client)
	if err != nil {
		// Can't verify, but sequencer should be stopped
		return hash, nil
	}
	if verifyHash != hash {
		// A block was produced during the stop - use the new hash
		return verifyHash, nil
	}

	return hash, nil
}

// getUnsafeHeadHash retrieves the current unsafe L2 head hash from sync status.
func getUnsafeHeadHash(ctx context.Context, client *rpc.Client) (string, error) {
	var status struct {
		UnsafeL2 struct {
			Hash string `json:"hash"`
		} `json:"unsafe_l2"`
	}
	if err := client.CallContext(ctx, &status, "optimism_syncStatus"); err != nil {
		return "", fmt.Errorf("get sync status: %w", err)
	}
	return status.UnsafeL2.Hash, nil
}

// StartSequencer calls admin_startSequencer on the op-node to resume block production.
// The unsafeHeadHash should be the value returned by StopSequencer.
//
// Even while the sequencer is stopped, the op-node updates its internal latestHead on every
// forkchoice update from the execution engine (e.g. P2P gossip catchup). This means the hash
// we have can become stale by the time we call Start. We therefore retry in a loop: each
// "block hash does not match" error embeds the correct current hash, which we extract and use
// for the next attempt, until the head stabilises and the call succeeds.
// A 30s deadline is applied so the loop cannot spin indefinitely.
func StartSequencer(ctx context.Context, nodeRPC string, unsafeHeadHash string) error {
	client, err := rpc.DialContext(ctx, nodeRPC)
	if err != nil {
		return fmt.Errorf("dial op-node: %w", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	hash := unsafeHeadHash
	for {
		callErr := client.CallContext(ctx, nil, "admin_startSequencer", hash)
		if callErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("call admin_startSequencer: %w", callErr)
		}
		// The engine keeps updating latestHead via forkchoice events even while stopped.
		// The error tells us the current latestHead; use it for the next attempt.
		if strings.Contains(callErr.Error(), "block hash does not match") {
			if next := extractHeadHashFromMismatchError(callErr.Error()); next != "" {
				hash = next
				continue
			}
		}
		return fmt.Errorf("call admin_startSequencer: %w", callErr)
	}
}

// extractHeadHashFromMismatchError parses the correct hash from an error of the form:
// "block hash does not match: head <hash>:<blockNum>, received <hash>"
func extractHeadHashFromMismatchError(msg string) string {
	const prefix = "head "
	idx := strings.Index(msg, prefix)
	if idx < 0 {
		return ""
	}
	rest := msg[idx+len(prefix):]
	// rest is "<hash>:<blockNum>, received ..."
	colonIdx := strings.Index(rest, ":")
	if colonIdx < 0 {
		return ""
	}
	return rest[:colonIdx]
}

// flushBatcher calls admin_flushBatcher on the batcher to force immediate posting.
func flushBatcher(ctx context.Context, batcherRPCURL string) error {
	reqBody := `{"jsonrpc":"2.0","method":"admin_flushBatcher","params":[],"id":1}`
	req, err := http.NewRequestWithContext(ctx, "POST", batcherRPCURL, strings.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	var rpcResp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &rpcResp); err == nil && rpcResp.Error != nil {
		return fmt.Errorf("%s (code %d)", rpcResp.Error.Message, rpcResp.Error.Code)
	}

	return nil
}
