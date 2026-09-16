package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GenerateReport creates a markdown report summarizing all experiment runs.
func GenerateReport(results []*RunResult, outputPath string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create report file: %w", err)
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	defer w.Flush()

	fmt.Fprintf(w, "# Experiment Report\n\n")
	fmt.Fprintf(w, "Generated: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(w, "Total runs: %d\n\n", len(results))

	// Summary comparison table
	fmt.Fprintf(w, "## Summary Comparison\n\n")
	fmt.Fprintf(w, "| Metric |")
	for _, r := range results {
		fmt.Fprintf(w, " Run %d |", r.RunID)
	}
	fmt.Fprintf(w, "\n|--------|")
	for range results {
		fmt.Fprintf(w, "--------|")
	}
	fmt.Fprintf(w, "\n")

	// Batcher config rows
	writeCompRow(w, "Max L1 Tx Size", results, func(r *RunResult) string {
		return fmt.Sprintf("%d", r.BatcherCfg.MaxL1TxSize)
	})
	writeCompRow(w, "Max Channel Duration", results, func(r *RunResult) string {
		return fmt.Sprintf("%d", r.BatcherCfg.MaxChannelDuration)
	})
	writeCompRow(w, "Max Blocks/Span Batch", results, func(r *RunResult) string {
		return fmt.Sprintf("%d", r.BatcherCfg.MaxBlocksPerSpanBatch)
	})
	writeCompRow(w, "Max Pending Tx", results, func(r *RunResult) string {
		return fmt.Sprintf("%d", r.BatcherCfg.MaxPendingTx)
	})
	writeCompRow(w, "Gas Limit", results, func(r *RunResult) string {
		if r.BatcherCfg.GasLimit == 0 {
			return "default"
		}
		return fmt.Sprintf("%d", r.BatcherCfg.GasLimit)
	})
	writeCompRow(w, "Block Time", results, func(r *RunResult) string {
		if r.BatcherCfg.BlockTime == 0 {
			return "default"
		}
		return fmt.Sprintf("%ds", r.BatcherCfg.BlockTime)
	})
	writeCompRow(w, "Duration", results, func(r *RunResult) string {
		return r.Duration.Round(time.Second).String()
	})
	writeCompRow(w, "Data Points", results, func(r *RunResult) string {
		return strconv.Itoa(r.RowCount)
	})
	fmt.Fprintf(w, "\n")

	// Per-run CSV analysis
	for _, r := range results {
		fmt.Fprintf(w, "---\n\n")
		fmt.Fprintf(w, "## Run %d\n\n", r.RunID)
		fmt.Fprintf(w, "**Batcher Config:** max_l1_tx_size=%d, max_channel_duration=%d, max_blocks_per_span_batch=%d, max_pending_tx=%d, gas_limit=%d, block_time=%d\n\n",
			r.BatcherCfg.MaxL1TxSize, r.BatcherCfg.MaxChannelDuration,
			r.BatcherCfg.MaxBlocksPerSpanBatch, r.BatcherCfg.MaxPendingTx,
			r.BatcherCfg.GasLimit, r.BatcherCfg.BlockTime)
		jsonlPath := filepath.Join(filepath.Dir(outputPath), "gas_limit_verification.jsonl")
		if summary, ok := formatGasLimitVerificationSummary(jsonlPath, r.RunID); ok {
			fmt.Fprintf(w, "%s\n\n", summary)
		}
		fmt.Fprintf(w, "**Duration:** %s | **Data Points:** %d | **CSV:** `%s`\n\n",
			r.Duration.Round(time.Second), r.RowCount, r.CSVPath)

		// Analyze the CSV for this run
		stats, err := analyzeCSV(r.CSVPath)
		if err != nil {
			fmt.Fprintf(w, "Error analyzing CSV: %v\n\n", err)
			continue
		}

		fmt.Fprintf(w, "### Metrics Summary\n\n")
		fmt.Fprintf(w, "| Metric | Min (non-zero) | Average (non-zero) | Peak | Peak At | Samples |\n")
		fmt.Fprintf(w, "|--------|----------------|--------------------|---------|---------|---------|\n")
		for _, m := range stats {
			fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %d |\n", m.Name, m.MinStr, m.AvgStr, m.PeakStr, m.PeakAtStr, m.NonZeroCount)
		}
		fmt.Fprintf(w, "\n")
	}

	// Final metrics comparison
	fmt.Fprintf(w, "---\n\n")
	fmt.Fprintf(w, "## Final Metrics Comparison\n\n")
	fmt.Fprintf(w, "| Metric |")
	for _, r := range results {
		fmt.Fprintf(w, " Run %d |", r.RunID)
	}
	fmt.Fprintf(w, "\n|--------|")
	for range results {
		fmt.Fprintf(w, "--------|")
	}
	fmt.Fprintf(w, "\n")

	metricKeys := []string{
		"tps", "normalized_tps", "l2_tx_cost_rbtc", "l2_tx_count",
		"batcher_post_cost_rbtc", "batcher_l1_gas_spend_wei", "batcher_l1_gas_price_wei",
		"batcher_data_size_bytes", "compression_ratio",
		"total_l2_txs", "total_deposits", "total_withdrawals",
		"safe_head_lag",
	}
	for _, key := range metricKeys {
		writeCompRow(w, key, results, func(r *RunResult) string {
			if v, ok := r.FinalMetrics[key]; ok {
				if v == 0 {
					return "0"
				}
				if v >= 1000 {
					return fmt.Sprintf("%.0f", v)
				}
				if v >= 1 {
					return fmt.Sprintf("%.4f", v)
				}
				return fmt.Sprintf("%.12f", v)
			}
			return "N/A"
		})
	}
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "> RBTC columns are in RBTC; `*_wei` columns are whole wei.\n")
	return nil
}

func writeCompRow(w *bufio.Writer, label string, results []*RunResult, fn func(*RunResult) string) {
	fmt.Fprintf(w, "| %s |", label)
	for _, r := range results {
		fmt.Fprintf(w, " %s |", fn(r))
	}
	fmt.Fprintf(w, "\n")
}

// formatGasLimitVerificationSummary returns the last JSONL record for runID, if any.
func formatGasLimitVerificationSummary(jsonlPath string, runID int) (string, bool) {
	f, err := os.Open(jsonlPath)
	if err != nil {
		return "", false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Default max token size may truncate long lines; raise for safety.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var last *GasLimitRunRecord
	for scanner.Scan() {
		var rec GasLimitRunRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue
		}
		if rec.RunID == runID {
			r := rec
			last = &r
		}
	}
	if last == nil {
		return "", false
	}

	tx := "none"
	if last.SetGasLimitTxHash != "" {
		tx = last.SetGasLimitTxHash
	}
	l1After := "n/a"
	if last.TxSubmitted {
		l1After = fmt.Sprintf("%d", last.L1GasLimitAfter)
	}
	s := fmt.Sprintf("**Gas limit verification:** target=%d, l1_before=%d, l2_before=%d, tx_submitted=%v, set_gas_limit_tx=%s, l1_after=%s, l2_after=%d, l2_block_when_matched=%d, wait=%s",
		last.TargetGasLimit,
		last.L1GasLimitBefore,
		last.L2GasLimitBefore,
		last.TxSubmitted,
		tx,
		l1After,
		last.L2GasLimitAfter,
		last.L2BlockWhenMatched,
		last.WaitDuration,
	)
	if last.Error != "" {
		s += fmt.Sprintf(", error=%q", last.Error)
	}
	s += fmt.Sprintf(" (recorded_at=%s)", last.RecordedAt)
	return s, true
}

// metricStat holds aggregated statistics for one metric column from a CSV.
type metricStat struct {
	Name         string
	Peak         float64
	PeakStr      string
	PeakAtT      float64 // seconds into the run when peak occurred
	PeakAtStr    string
	Min          float64
	MinStr       string
	Sum          float64
	NonZeroSum   float64
	Count        int
	NonZeroCount int
	AvgStr       string
}

// analyzeCSV reads an experiment CSV and computes per-metric statistics.
func analyzeCSV(csvPath string) ([]metricStat, error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := csv.NewReader(f)
	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) < 2 {
		return nil, fmt.Errorf("CSV has no data rows")
	}

	header := records[0]
	data := records[1:]

	metricsOfInterest := []string{
		"tps", "normalized_tps",
		"l2_tx_cost_rbtc", "l2_tx_cost_min_rbtc", "l2_tx_cost_max_rbtc",
		"l2_tx_speed_s", "l2_tx_count",
		"posting_freq_s", "batcher_post_cost_rbtc",
		"batcher_l1_gas_spend_wei", "batcher_l1_gas_price_wei",
		"batcher_data_size_bytes",
		"compression_ratio", "amortized_l1_cost_per_l2_tx",
		"total_tx_cost_rbtc", "total_tx_cost_min_rbtc", "total_tx_cost_max_rbtc",
		"compressed_data_per_tx_kb", "raw_data_per_tx_kb",
		"data_throughput_bytes_per_s",
	}

	// Aggregate stats
	stats := make(map[string]*metricStat)
	for _, name := range metricsOfInterest {
		stats[name] = &metricStat{Name: name, Min: math.MaxFloat64}
	}

	for _, row := range data {
		var sample ExperimentSample
		if err := sample.FromCSVRow(header, row); err != nil {
			continue
		}

		for _, name := range metricsOfInterest {
			v := sample.MetricValue(name)
			s := stats[name]
			s.Count++
			s.Sum += v
			if v > s.Peak {
				s.Peak = v
				s.PeakAtT = sample.TSeconds
			}
			if v != 0 {
				s.NonZeroSum += v
				s.NonZeroCount++
				if v < s.Min {
					s.Min = v
				}
			}
		}
	}

	// Format results
	var result []metricStat
	for _, name := range metricsOfInterest {
		s := stats[name]
		s.PeakStr = formatMetricValue(name, s.Peak)
		s.PeakAtStr = formatElapsedTime(s.PeakAtT)
		if s.NonZeroCount > 0 {
			s.AvgStr = formatMetricValue(name, s.NonZeroSum/float64(s.NonZeroCount))
			s.MinStr = formatMetricValue(name, s.Min)
		} else {
			s.AvgStr = "N/A"
			s.MinStr = "N/A"
		}
		result = append(result, *s)
	}

	return result, nil
}

func formatElapsedTime(seconds float64) string {
	totalSecs := int(seconds)
	h := totalSecs / 3600
	m := (totalSecs % 3600) / 60
	s := totalSecs % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func formatMetricValue(name string, v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "N/A"
	}
	switch {
	case strings.Contains(name, "cost") || strings.Contains(name, "rbtc"):
		return fmt.Sprintf("%.12f", v)
	case strings.Contains(name, "_wei"):
		return fmt.Sprintf("%.0f", v)
	case strings.Contains(name, "bytes") || strings.Contains(name, "count"):
		return fmt.Sprintf("%.0f", v)
	case strings.Contains(name, "ratio"):
		return fmt.Sprintf("%.4f", v)
	default:
		return fmt.Sprintf("%.4f", v)
	}
}

// CombineCSVs merges all per-run CSVs into a single combined CSV file.
func CombineCSVs(results []*RunResult, outputPath string) error {
	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create combined CSV: %w", err)
	}
	defer out.Close()

	w := csv.NewWriter(out)
	defer w.Flush()

	headerWritten := false
	for _, r := range results {
		f, err := os.Open(r.CSVPath)
		if err != nil {
			continue
		}

		reader := csv.NewReader(f)
		records, err := reader.ReadAll()
		f.Close()
		if err != nil || len(records) < 2 {
			continue
		}

		// Write header only once
		if !headerWritten {
			if err := w.Write(records[0]); err != nil {
				return err
			}
			headerWritten = true
		}

		// Write data rows
		for _, row := range records[1:] {
			if err := w.Write(row); err != nil {
				return err
			}
		}
	}

	return nil
}

// openCombinedCSVForIncrementalAppend opens combined.csv for append. If the file
// is missing or empty, needHeader is true so the caller should write the CSV
// header once before appending data rows.
func openCombinedCSVForIncrementalAppend(path string) (*os.File, bool, error) {
	info, err := os.Stat(path)
	needHeader := os.IsNotExist(err) || (err == nil && info.Size() == 0)
	if err != nil && !os.IsNotExist(err) {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, false, err
	}
	return f, needHeader, nil
}

// RunResultFromCSV reconstructs a RunResult from an experiment CSV file.
// Knob values are taken from the first data row; final metrics from the last.
func RunResultFromCSV(csvPath string, runID int) (*RunResult, error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read CSV %s: %w", csvPath, err)
	}
	if len(records) < 2 {
		return nil, fmt.Errorf("CSV %s has no data rows", csvPath)
	}

	header := records[0]
	data := records[1:]

	var firstSample, lastSample ExperimentSample
	if err := firstSample.FromCSVRow(header, data[0]); err != nil {
		return nil, fmt.Errorf("parse first row: %w", err)
	}
	if err := lastSample.FromCSVRow(header, data[len(data)-1]); err != nil {
		return nil, fmt.Errorf("parse last row: %w", err)
	}

	finalMetricKeys := []string{
		"tps", "normalized_tps", "l2_tx_cost_rbtc", "l2_tx_count",
		"batcher_post_cost_rbtc", "batcher_l1_gas_spend_wei", "batcher_l1_gas_price_wei",
		"batcher_data_size_bytes", "compression_ratio",
		"total_l2_txs", "total_deposits", "total_withdrawals",
		"safe_head_lag",
	}
	finalMetrics := make(map[string]float64, len(finalMetricKeys))
	for _, key := range finalMetricKeys {
		finalMetrics[key] = lastSample.MetricValue(key)
	}

	return &RunResult{
		RunID: runID,
		BatcherCfg: BatcherRunConfig{
			MaxL1TxSize:           firstSample.Knobs.MaxL1TxSize,
			MaxChannelDuration:    firstSample.Knobs.MaxChannelDuration,
			MaxBlocksPerSpanBatch: firstSample.Knobs.MaxBlocksPerSpanBatch,
			MaxPendingTx:          firstSample.Knobs.MaxPendingTx,
			GasLimit:              firstSample.Knobs.GasLimit,
			BlockTime:             firstSample.Knobs.BlockTime,
		},
		CSVPath:      csvPath,
		RowCount:     len(data),
		Duration:     time.Duration(lastSample.TSeconds * float64(time.Second)),
		FinalMetrics: finalMetrics,
	}, nil
}

// CollectRunResults scans a directory for experiment_run_*.csv files and
// reconstructs a RunResult for each. Results are sorted by run ID.
func CollectRunResults(dir string) ([]*RunResult, error) {
	pattern := filepath.Join(dir, "experiment_run_*.csv")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob experiment CSVs: %w", err)
	}
	sort.Strings(matches)

	var results []*RunResult
	for _, csvPath := range matches {
		runID, err := RunIDFromCSVPath(csvPath)
		if err != nil {
			continue
		}
		result, err := RunResultFromCSV(csvPath, runID)
		if err != nil {
			continue
		}
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].RunID < results[j].RunID
	})
	return results, nil
}

// MaxRunIDInDir returns the highest run ID found among experiment_run_*.csv
// files in the given directory, or 0 if none exist.
func MaxRunIDInDir(dir string) int {
	pattern := filepath.Join(dir, "experiment_run_*.csv")
	matches, _ := filepath.Glob(pattern)
	maxID := 0
	for _, m := range matches {
		if id, err := RunIDFromCSVPath(m); err == nil && id > maxID {
			maxID = id
		}
	}
	return maxID
}

// NextRunIDInDir returns the next run ID to use based on the given strategy.
// Strategies:
//   - "max" (default): returns MaxRunIDInDir(dir) + 1, continuing past the highest existing ID
//   - "min": returns the smallest unused positive integer (fills gaps)
func NextRunIDInDir(dir string, strategy string) int {
	pattern := filepath.Join(dir, "experiment_run_*.csv")
	matches, _ := filepath.Glob(pattern)

	if len(matches) == 0 {
		return 1
	}

	// Collect existing IDs
	existing := make(map[int]bool)
	maxID := 0
	for _, m := range matches {
		if id, err := RunIDFromCSVPath(m); err == nil {
			existing[id] = true
			if id > maxID {
				maxID = id
			}
		}
	}

	switch strategy {
	case "min":
		// Find smallest unused ID starting from 1
		for i := 1; ; i++ {
			if !existing[i] {
				return i
			}
		}
	default: // "max" or unrecognized
		return maxID + 1
	}
}
