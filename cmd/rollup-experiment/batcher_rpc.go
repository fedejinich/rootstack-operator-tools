package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// BatcherController is the interface for controlling the batcher between runs.
// It is implemented by BatcherProcess (subprocess mode) and BatcherRPC (RPC mode).
type BatcherController interface {
	Start(ctx context.Context, target TargetConfig, run BatcherRunConfig) error
	Stop() error
	IsRunning() bool
	RestartEmbedded()
}

// BatcherFlusher is an optional interface for batchers that support flushing.
type BatcherFlusher interface {
	Flush(ctx context.Context) error
}

// BatcherRPC controls an already-running batcher via JSON-RPC admin API.
// This is an alternative to BatcherProcess when the batcher runs embedded
// in the rollup-node and should be controlled via RPC rather than subprocess.
type BatcherRPC struct {
	logger     *slog.Logger
	rpcURL     string
	configPath string
	running    bool
}

// NewBatcherRPC creates a batcher RPC controller.
// rpcURL is the admin RPC endpoint (e.g. "http://127.0.0.1:8548").
// configPath is optional - if set, batcher config is written there before starting.
func NewBatcherRPC(logger *slog.Logger, rpcURL, configPath string) *BatcherRPC {
	if rpcURL == "" {
		rpcURL = defaultBatcherRPCURL
	}
	return &BatcherRPC{
		logger:     logger,
		rpcURL:     rpcURL,
		configPath: configPath,
	}
}

// Start starts the batcher via admin_startBatcher RPC.
// If configPath is set, the batcher config is written before starting.
func (br *BatcherRPC) Start(ctx context.Context, target TargetConfig, run BatcherRunConfig) error {
	// Write config to file if path is set
	if br.configPath != "" {
		if err := br.writeConfig(run); err != nil {
			return fmt.Errorf("write batcher config: %w", err)
		}
		br.logger.Info("Batcher config written",
			"path", br.configPath,
			"max_l1_tx_size", run.MaxL1TxSize,
			"max_channel_duration", run.MaxChannelDuration,
		)
	}

	br.logger.Info("Starting batcher via RPC", "url", br.rpcURL)

	if err := br.callAdmin(ctx, "admin_startBatcher"); err != nil {
		return fmt.Errorf("admin_startBatcher: %w", err)
	}

	br.running = true
	br.logger.Info("Batcher started via RPC")

	// Brief delay to let batcher initialize
	time.Sleep(1 * time.Second)
	return nil
}

// writeConfig writes the batcher configuration to the config file.
// It reads the existing file, updates the [batcher] section, and writes it back.
func (br *BatcherRPC) writeConfig(run BatcherRunConfig) error {
	// Read existing config if it exists
	existingContent := ""
	if data, err := os.ReadFile(br.configPath); err == nil {
		existingContent = string(data)
	}

	// Build the new [batcher] section
	batcherSection := fmt.Sprintf(`[batcher]
max_channel_duration = %d
max_l1_tx_size = %d
max_blocks_per_span_batch = %d
max_pending_transactions = %d
batch_type = %d
compression_algo = "%s"
sub_safety_margin = %d
`,
		run.MaxChannelDuration,
		run.MaxL1TxSize,
		run.MaxBlocksPerSpanBatch,
		run.MaxPendingTx,
		run.BatchType,
		run.CompressionAlgo,
		run.SubSafetyMargin,
	)

	var newContent string
	if existingContent == "" {
		// No existing file, just write the batcher section
		newContent = batcherSection
	} else {
		// Replace or append the [batcher] section
		newContent = replaceTOMLSection(existingContent, "batcher", batcherSection)
	}

	return os.WriteFile(br.configPath, []byte(newContent), 0644)
}

// replaceTOMLSection replaces a [section] in TOML content with new content.
// If the section doesn't exist, appends it at the end.
func replaceTOMLSection(content, sectionName, newSection string) string {
	lines := strings.Split(content, "\n")
	var result []string
	inSection := false
	sectionFound := false
	sectionHeader := "[" + sectionName + "]"

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Check if we're entering a new section
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if trimmed == sectionHeader {
				// Found our section, skip it and its contents
				inSection = true
				sectionFound = true
				continue
			} else if inSection {
				// We were in our section, now entering a new one
				// Insert our new section content before this new section
				result = append(result, strings.TrimSuffix(newSection, "\n"))
				inSection = false
			}
		}

		if !inSection {
			result = append(result, line)
		}
	}

	// If we were still in the section at EOF, or section wasn't found
	if inSection || !sectionFound {
		result = append(result, "")
		result = append(result, strings.TrimSuffix(newSection, "\n"))
	}

	return strings.Join(result, "\n")
}

// Stop stops the batcher via admin_stopBatcher RPC.
func (br *BatcherRPC) Stop() error {
	if !br.running {
		return nil
	}

	br.logger.Info("Stopping batcher via RPC", "url", br.rpcURL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := br.callAdmin(ctx, "admin_stopBatcher"); err != nil {
		return fmt.Errorf("admin_stopBatcher: %w", err)
	}

	br.running = false
	br.logger.Info("Batcher stopped via RPC")
	return nil
}

// Flush forces the batcher to post pending data via admin_flushBatcher RPC.
// This is useful for ensuring all data is posted before stopping.
func (br *BatcherRPC) Flush(ctx context.Context) error {
	br.logger.Info("Flushing batcher via RPC", "url", br.rpcURL)

	if err := br.callAdmin(ctx, "admin_flushBatcher"); err != nil {
		return fmt.Errorf("admin_flushBatcher: %w", err)
	}

	br.logger.Info("Batcher flush initiated")
	return nil
}

// IsRunning returns whether the batcher is believed to be running.
func (br *BatcherRPC) IsRunning() bool {
	return br.running
}

// RestartEmbedded is a no-op for RPC mode (batcher stays in rollup-node).
func (br *BatcherRPC) RestartEmbedded() {}

// callAdmin sends a JSON-RPC admin request to the batcher.
func (br *BatcherRPC) callAdmin(ctx context.Context, method string) error {
	reqBody := fmt.Sprintf(`{"jsonrpc":"2.0","method":"%s","params":[],"id":1}`, method)
	req, err := http.NewRequestWithContext(ctx, "POST", br.rpcURL, strings.NewReader(reqBody))
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
