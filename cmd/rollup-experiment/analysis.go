package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NimbleMarkets/ntcharts/linechart/timeserieslinechart"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fedejinich/rootstack-operator-tools/cmd/rollup-monitor/monitor"
)

// ---------------------------------------------------------------------------
// Styles (mirrors monitor TUI for visual consistency)
// ---------------------------------------------------------------------------

var (
	anTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("205"))
	anStatusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))
	anChartBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("62")).
				Padding(0, 0)
	anSelectedChartBorderStyle = lipgloss.NewStyle().
					Border(lipgloss.RoundedBorder()).
					BorderForeground(lipgloss.Color("205")).
					Padding(0, 0)
	anChartLabelStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("99"))
	anPeakStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))

	anLogBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("240")).
				Padding(0, 1)
	anLogLabelStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("214"))
	anLogTimeStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))
	anLogMsgStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))
	anLogErrorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196"))
)

// ---------------------------------------------------------------------------
// experimentLog -- concurrent-safe log buffer for the TUI action log
// ---------------------------------------------------------------------------

type experimentLog struct {
	mu      sync.RWMutex
	entries []monitor.LogEntry
	cap     int
}

func newExperimentLog(cap int) *experimentLog {
	return &experimentLog{cap: cap}
}

func (l *experimentLog) Add(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, monitor.LogEntry{Time: time.Now(), Message: msg})
	if len(l.entries) > l.cap {
		l.entries = l.entries[len(l.entries)-l.cap:]
	}
}

func (l *experimentLog) Entries() []monitor.LogEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]monitor.LogEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// filterErrorEntries returns only log entries that contain error-related keywords.
func filterErrorEntries(entries []monitor.LogEntry) []monitor.LogEntry {
	var errors []monitor.LogEntry
	for _, e := range entries {
		msg := strings.ToLower(e.Message)
		if strings.Contains(msg, "error") || strings.Contains(msg, "failed") ||
			strings.Contains(msg, "auto-paused") {
			errors = append(errors, e)
		}
	}
	return errors
}

// tuiLogHandler implements slog.Handler, routing log records to the TUI action log.
type tuiLogHandler struct {
	log   *experimentLog
	level slog.Level
}

func newTUILogHandler(log *experimentLog, level slog.Level) *tuiLogHandler {
	return &tuiLogHandler{log: log, level: level}
}

func (h *tuiLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *tuiLogHandler) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		sb.WriteString(" ")
		sb.WriteString(a.Key)
		sb.WriteString("=")
		sb.WriteString(a.Value.String())
		return true
	})
	h.log.Add(sb.String())
	return nil
}

func (h *tuiLogHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *tuiLogHandler) WithGroup(_ string) slog.Handler      { return h }

// analysisEpoch is a fixed reference time used as the origin for elapsed-time
// charts. All runs' timestamps are remapped so that t=0 corresponds to this
// epoch, allowing multiple runs to overlay on the same time axis.
var analysisEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// anElapsedTimeFormatter returns an X-axis label formatter that displays
// elapsed time (M:SS or H:MM:SS) relative to analysisEpoch.
func anElapsedTimeFormatter() func(int, float64) string {
	return func(_ int, v float64) string {
		t := time.Unix(int64(v), 0).UTC()
		elapsed := t.Sub(analysisEpoch)
		if elapsed < 0 {
			elapsed = 0
		}
		totalSecs := int(elapsed.Seconds())
		h := totalSecs / 3600
		m := (totalSecs % 3600) / 60
		s := totalSecs % 60
		if h > 0 {
			return fmt.Sprintf("%d:%02d:%02d", h, m, s)
		}
		return fmt.Sprintf("%d:%02d", m, s)
	}
}

// Per-run colors for chart datasets.
var runColors = []lipgloss.Style{
	lipgloss.NewStyle().Foreground(lipgloss.Color("82")),  // green
	lipgloss.NewStyle().Foreground(lipgloss.Color("205")), // magenta
	lipgloss.NewStyle().Foreground(lipgloss.Color("39")),  // blue
	lipgloss.NewStyle().Foreground(lipgloss.Color("214")), // orange
	lipgloss.NewStyle().Foreground(lipgloss.Color("159")), // cyan
	lipgloss.NewStyle().Foreground(lipgloss.Color("196")), // red
	lipgloss.NewStyle().Foreground(lipgloss.Color("226")), // yellow
	lipgloss.NewStyle().Foreground(lipgloss.Color("141")), // purple
}

func runColor(runID int) lipgloss.Style {
	idx := (runID - 1) % len(runColors)
	if idx < 0 {
		idx = 0
	}
	return runColors[idx]
}

func runLabel(runID int) string {
	return fmt.Sprintf("Run %d", runID)
}

// ---------------------------------------------------------------------------
// Chart helpers (adapted from monitor TUI)
// ---------------------------------------------------------------------------

type anChartScale struct {
	yZoom      float64
	timeWin    time.Duration
	normalized bool
}

// isMultiDatasetPanel returns true for panel keys that plot more than one data series.
func isMultiDatasetPanel(key string) bool {
	switch key {
	case "deposits_withdrawals", "batcher_pipeline", "l1_fee_pressure":
		return true
	}
	return false
}

// anNormalizeTimeSeries scales every value in data to [0, 1] by dividing by the series maximum.
func anNormalizeTimeSeries(data []monitor.TimeValue) []monitor.TimeValue {
	var maxVal float64
	for _, tv := range data {
		if tv.Value > maxVal {
			maxVal = tv.Value
		}
	}
	if maxVal == 0 {
		return data
	}
	out := make([]monitor.TimeValue, len(data))
	for i, tv := range data {
		out[i] = monitor.TimeValue{Time: tv.Time, Value: tv.Value / maxVal}
	}
	return out
}

func anAdaptiveYFormatter(_ int, v float64) string {
	abs := v
	if abs < 0 {
		abs = -abs
	}
	switch {
	case v == 0:
		return "0"
	case abs < 0.01:
		return fmt.Sprintf("%.4f", v)
	case abs < 1:
		return fmt.Sprintf("%.2f", v)
	case abs < 10:
		return fmt.Sprintf("%.1f", v)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}

func anUnitYFormatter(unit string) func(int, float64) string {
	return func(i int, v float64) string {
		return anAdaptiveYFormatter(i, v) + " " + unit
	}
}

func anNormalizedYFormatter(_ int, v float64) string {
	pct := v * 100
	switch {
	case pct == 0:
		return "0%"
	case pct < 1:
		return fmt.Sprintf("%.1f%%", pct)
	default:
		return fmt.Sprintf("%.0f%%", pct)
	}
}

func anLastValueStr(data []monitor.TimeValue, format string) string {
	if len(data) == 0 {
		return "[-]"
	}
	return fmt.Sprintf("["+format+"]", data[len(data)-1].Value)
}

func anFmtSummaryVal(v float64, format string) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf(format, v)
}

func anZoomIndicator(s anChartScale, defaultTimeWin time.Duration) string {
	yDefault := s.yZoom == 1.0
	tDefault := s.timeWin == defaultTimeWin
	if yDefault && tDefault {
		return ""
	}
	var parts []string
	if !yDefault {
		parts = append(parts, fmt.Sprintf("y:%.1fx", s.yZoom))
	}
	if !tDefault {
		parts = append(parts, fmt.Sprintf("t:%s", anFmtTimeWin(s.timeWin)))
	}
	return strings.Join(parts, " ")
}

func anFmtTimeWin(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	if d >= time.Minute {
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		if s > 0 {
			return fmt.Sprintf("%dm%ds", m, s)
		}
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func anDataYRange(data []monitor.TimeValue) (float64, float64) {
	if len(data) == 0 {
		return 0, 0
	}
	minY, maxY := data[0].Value, data[0].Value
	for _, tv := range data[1:] {
		if tv.Value < minY {
			minY = tv.Value
		}
		if tv.Value > maxY {
			maxY = tv.Value
		}
	}
	return minY, maxY
}

// anDataRange returns the Y-value range and the time range across all data points.
func anDataRange(data []monitor.TimeValue) (minY, maxY float64, earliest, latest time.Time) {
	if len(data) == 0 {
		return
	}
	minY, maxY = data[0].Value, data[0].Value
	earliest, latest = data[0].Time, data[0].Time
	for _, tv := range data[1:] {
		if tv.Value < minY {
			minY = tv.Value
		}
		if tv.Value > maxY {
			maxY = tv.Value
		}
		if tv.Time.Before(earliest) {
			earliest = tv.Time
		}
		if tv.Time.After(latest) {
			latest = tv.Time
		}
	}
	return
}

// ---------------------------------------------------------------------------
// CSVWatcher -- polls experiment CSV files for new rows
// ---------------------------------------------------------------------------

// CSVWatcher monitors an experiment output directory for CSV files and feeds
// new rows into per-run MetricsStores.
type CSVWatcher struct {
	dir          string
	pollInterval time.Duration
	stores       map[int]*monitor.MetricsStore
	mu           sync.RWMutex
	offsets      map[string]int64 // file path -> last read offset
	totalRows    int
	runIDs       []int // sorted
	stopCh       chan struct{}

	// Optional channel to receive samples (for --live mode).
	sampleCh <-chan ExperimentSample
}

// NewCSVWatcher creates a watcher for experiment CSVs in the given directory.
func NewCSVWatcher(dir string, pollInterval time.Duration) *CSVWatcher {
	return &CSVWatcher{
		dir:          dir,
		pollInterval: pollInterval,
		stores:       make(map[int]*monitor.MetricsStore),
		offsets:      make(map[string]int64),
		stopCh:       make(chan struct{}),
	}
}

// NewCSVWatcherWithChannel creates a watcher that reads from a sample channel
// (for --live embedded mode) instead of polling files.
func NewCSVWatcherWithChannel(sampleCh <-chan ExperimentSample) *CSVWatcher {
	return &CSVWatcher{
		stores:   make(map[int]*monitor.MetricsStore),
		offsets:  make(map[string]int64),
		stopCh:   make(chan struct{}),
		sampleCh: sampleCh,
	}
}

// Stores returns a snapshot of the current run stores map.
func (w *CSVWatcher) Stores() map[int]*monitor.MetricsStore {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make(map[int]*monitor.MetricsStore, len(w.stores))
	for k, v := range w.stores {
		out[k] = v
	}
	return out
}

// RunIDs returns sorted run IDs.
func (w *CSVWatcher) RunIDs() []int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]int, len(w.runIDs))
	copy(out, w.runIDs)
	return out
}

// TotalRows returns the total number of rows ingested.
func (w *CSVWatcher) TotalRows() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.totalRows
}

// Stop signals the watcher to stop.
func (w *CSVWatcher) Stop() {
	select {
	case <-w.stopCh:
	default:
		close(w.stopCh)
	}
}

// Run starts the polling loop (blocks until Stop or stopCh closes).
func (w *CSVWatcher) Run() {
	if w.sampleCh != nil {
		w.runFromChannel()
		return
	}
	w.runFromFiles()
}

func (w *CSVWatcher) runFromChannel() {
	for {
		select {
		case <-w.stopCh:
			return
		case sample, ok := <-w.sampleCh:
			if !ok {
				return
			}
			w.ingestSample(sample)
		}
	}
}

func (w *CSVWatcher) runFromFiles() {
	// Initial scan
	w.pollFiles()

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.pollFiles()
		}
	}
}

func (w *CSVWatcher) pollFiles() {
	pattern := filepath.Join(w.dir, "experiment_run_*.csv")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	sort.Strings(matches)

	for _, path := range matches {
		w.readNewRows(path)
	}
}

func (w *CSVWatcher) readNewRows(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	// Seek to last known offset
	offset := w.offsets[path]
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return
		}
	}

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	reader.ReuseRecord = true

	// If starting from the beginning, read and store the header
	var header []string
	if offset == 0 {
		headerRow, err := reader.Read()
		if err != nil {
			return
		}
		header = make([]string, len(headerRow))
		copy(header, headerRow)
	} else {
		header = ExperimentSampleCSVHeader()
	}

	for {
		row, err := reader.Read()
		if err != nil {
			break
		}
		rowCopy := make([]string, len(row))
		copy(rowCopy, row)

		var sample ExperimentSample
		if parseErr := sample.FromCSVRow(header, rowCopy); parseErr != nil {
			continue
		}
		w.ingestSample(sample)
	}

	// Update offset
	newOffset, _ := f.Seek(0, io.SeekCurrent)
	w.offsets[path] = newOffset
}

func (w *CSVWatcher) ingestSample(sample ExperimentSample) {
	w.mu.Lock()
	defer w.mu.Unlock()

	runID := sample.RunID
	store, exists := w.stores[runID]
	if !exists {
		store = monitor.NewMetricsStore(10000)
		w.stores[runID] = store
		w.runIDs = append(w.runIDs, runID)
		sort.Ints(w.runIDs)
	}

	// Remap timestamp to elapsed time from run start so multiple runs
	// overlay on the same time axis, making differences easy to spot.
	remappedTime := analysisEpoch.Add(time.Duration(sample.TSeconds * float64(time.Second)))

	values := sampleToMetricMap(sample)
	store.IngestExperimentSample(remappedTime, values)
	w.totalRows++
}

// sampleToMetricMap converts an ExperimentSample to the flat metric map
// expected by MetricsStore.IngestExperimentSample.
func sampleToMetricMap(s ExperimentSample) map[string]float64 {
	return map[string]float64{
		"tps":                      s.Measurements.TPS,
		"l2_tx_cost_rbtc":          s.Measurements.L2TxCostRBTC,
		"l2_tx_speed_s":            s.Measurements.L2TxSpeedS,
		"l2_tx_count":              float64(s.Measurements.L2TxCount),
		"l1_tx_count":              float64(s.Measurements.L1TxCount),
		"posting_freq_s":           s.Measurements.PostingFreqS,
		"deposits":                 float64(s.Measurements.Deposits),
		"withdrawals":              float64(s.Measurements.Withdrawals),
		"safe_head_lag":            float64(s.Measurements.SafeHeadLag),
		"batcher_post_cost_rbtc":   s.Measurements.BatcherPostCostRBTC,
		"batcher_l1_gas_spend_wei": s.Measurements.BatcherL1GasSpendWei,
		"batcher_l1_gas_price_wei": s.Measurements.BatcherL1GasPriceWei,
		"batcher_data_size_bytes":  float64(s.Measurements.BatcherDataSizeBytes),
		"compression_ratio":        s.Measurements.CompressionRatio,
		"compression_ratio_delta":  s.Measurements.CompressionRatioDelta,
		"l2_block":                 float64(s.Measurements.L2Block),
		"l1_block":                 float64(s.Measurements.L1Block),
		"total_l2_txs":             float64(s.Measurements.TotalL2Txs),
		"total_deposits":           float64(s.Measurements.TotalDeposits),
		"total_withdrawals":        float64(s.Measurements.TotalWithdrawals),
		"unsafe_l2":                float64(s.Measurements.UnsafeL2),
		"safe_l2":                  float64(s.Measurements.SafeL2),
		"txpool_pending":           float64(s.Measurements.TxPoolPending),
		"txpool_queued":            float64(s.Measurements.TxPoolQueued),
		"batcher_pending_blocks":   s.Measurements.BatcherPendingBlocks,
		"batcher_pending_bytes":    s.Measurements.BatcherPendingBytes,
		"batcher_l1_basefee_gwei":  s.Measurements.BatcherL1BaseFeeGwei,
		"batcher_balance_rbtc":     s.Measurements.BatcherBalanceRBTC,
		"batcher_channel_timeouts": s.Measurements.BatcherChannelTimeouts,
		"batcher_tx_failed":        s.Measurements.BatcherTxFailed,
		"batcher_gas_bumps":        s.Measurements.BatcherGasBumps,
		"total_batcher_posts":      float64(s.Measurements.TotalBatcherPosts),
		"total_batcher_blocks":     float64(s.Measurements.TotalBatcherBlocks),
		"total_batcher_cost_rbtc":  s.Measurements.TotalBatcherCostRBTC,
		"l2_to_l1_throughput":      s.Measurements.L2ToL1Throughput,
		"l2_to_l1_latency":         s.Measurements.L2ToL1Latency,
		"total_l2_txs_finalized":   float64(s.Measurements.TotalL2TxsFinalized),
	}
}

// ---------------------------------------------------------------------------
// AnalysisModel -- Bubble Tea TUI for experiment analysis
// ---------------------------------------------------------------------------

type analysisTickMsg time.Time

// chartPanel describes one chart panel in the analysis TUI.
type chartPanel struct {
	key         string // config key (e.g. "tps", "l2_tx_cost")
	label       string
	unit        string
	format      string
	fullWidth   bool   // rendered full-width instead of in a 2-column pair
	formula     string // mathematical formula shown in the "f" overlay
	description string // human-readable explanation shown in the "f" overlay
	// extractFn returns the time series for a given metric from a snapshot.
	extractFn func(snap monitor.Snapshot) []monitor.TimeValue
	// summaryFn returns the summary snapshot for this metric.
	summaryFn func(snap monitor.Snapshot) monitor.SummarySnapshot
}

var analysisPanels = []chartPanel{
	{key: "tps", label: "L2→L1 TPS", unit: "tx/s", format: "%.2f",
		formula:     "TPS = l2TxsInChannel / (l1ConfirmTime − earliestTxSendTime)",
		description: "L2 transactions reaching L1 finality per second. Computed when a\nbatcher channel completes on L1: total L2 txs divided by latency from\nearliest tracked tx send time (or L2 block time as fallback) to L1.",
		extractFn:   func(s monitor.Snapshot) []monitor.TimeValue { return s.L2ToL1Throughput },
		summaryFn:   func(s monitor.Snapshot) monitor.SummarySnapshot { return s.SumL2ToL1Throughput },
	},
	{key: "l2_tx_cost", label: "L2 Tx Cost (RBTC)", unit: "RBTC", format: "%.8f",
		formula:     "cost = avg(gasPrice × gasUsed + l1Fee)",
		description: "Mean gas cost across user transactions in the L2 block.\ngasPrice × gasUsed gives the L2 execution cost, l1Fee is the L1\ndata-availability surcharge. The sum is converted from wei to RBTC.",
		extractFn:   func(s monitor.Snapshot) []monitor.TimeValue { return s.L2TxCost },
		summaryFn:   func(s monitor.Snapshot) monitor.SummarySnapshot { return s.SumL2Cost },
	},
	{key: "l2_tx_speed", label: "L2 Tx Speed (s)", unit: "s", format: "%.2f",
		formula:     "speed = receiptTime − sendTime",
		description: "Seconds elapsed from transaction submission to receipt confirmation.\nOnly recorded when the traffic simulator is active.",
		extractFn:   func(s monitor.Snapshot) []monitor.TimeValue { return s.L2TxSpeed },
		summaryFn:   func(s monitor.Snapshot) monitor.SummarySnapshot { return s.SumL2Speed },
	},
	{key: "txs_per_block", label: "Txs / L2 Block", unit: "tx", format: "%.0f",
		formula:     "count = len(non-deposit txs)",
		description: "Number of user (non-deposit) transactions included in each L2 block.\nDeposit transactions originating from L1 are excluded from the count.",
		extractFn:   func(s monitor.Snapshot) []monitor.TimeValue { return s.L2TxCount },
		summaryFn:   func(s monitor.Snapshot) monitor.SummarySnapshot { return s.SumL2Count },
	},
	{key: "batcher_post_cost", label: "Batcher Post Cost (RBTC)", unit: "RBTC", format: "%.8f",
		formula:     "cost = gasPrice × gasUsed",
		description: "Gas cost of the batcher transaction posted to the batch inbox on L1,\nconverted from wei to RBTC.",
		extractFn:   func(s monitor.Snapshot) []monitor.TimeValue { return s.BatcherPostCost },
		summaryFn:   func(s monitor.Snapshot) monitor.SummarySnapshot { return s.SumBatcherPostCost },
	},
	{key: "batcher_l1_gas_spend_wei", label: "Batcher L1 Gas Spend", unit: "wei", format: "%.0f",
		formula:     "spend = effectiveGasPrice × gasUsed",
		description: "Execution gas paid for the batcher inbox L1 transaction (wei),\nexcluding OP-stack L1 data fee when present.",
		extractFn:   func(s monitor.Snapshot) []monitor.TimeValue { return s.BatcherL1GasSpendWei },
		summaryFn:   func(s monitor.Snapshot) monitor.SummarySnapshot { return s.SumBatcherL1GasSpendWei },
	},
	{key: "batcher_l1_gas_price_wei", label: "Batcher L1 Gas Price", unit: "wei", format: "%.0f",
		formula:     "price = receipt.effectiveGasPrice (or tx.gasPrice)",
		description: "Effective gas price for the batcher inbox L1 transaction (wei).",
		extractFn:   func(s monitor.Snapshot) []monitor.TimeValue { return s.BatcherL1GasPriceWei },
		summaryFn:   func(s monitor.Snapshot) monitor.SummarySnapshot { return s.SumBatcherL1GasPriceWei },
	},
	{key: "compression_ratio", label: "Compression Ratio", unit: "", format: "%.3f",
		formula:     "bytes = len(batcherTx.data)\nratio = comprRatioSum / comprRatioCount",
		description: "When compression metrics are unavailable: raw calldata size of the\nbatcher transaction in bytes.\nWhen available: compression ratio from op_batcher Prometheus metrics\n(channel_compr_ratio_sum / channel_compr_ratio_count).",
		extractFn: func(s monitor.Snapshot) []monitor.TimeValue {
			if len(s.CompressionRatio) > 0 {
				return s.CompressionRatio
			}
			return s.BatcherDataSize
		},
		summaryFn: func(s monitor.Snapshot) monitor.SummarySnapshot {
			if s.SumCompressionRatio.Peak > 0 {
				return s.SumCompressionRatio
			}
			return s.SumBatcherDataSize
		},
	},
	{key: "batcher_data_size", label: "Batcher Data Size", unit: "B", format: "%.0f",
		formula:     "bytes = len(batcherTx.data)",
		description: "Raw calldata size of each batcher transaction posted to the\nbatch inbox on L1, in bytes.",
		extractFn:   func(s monitor.Snapshot) []monitor.TimeValue { return s.BatcherDataSize },
		summaryFn:   func(s monitor.Snapshot) monitor.SummarySnapshot { return s.SumBatcherDataSize },
	},
	{key: "batcher_pipeline", label: "Batcher Pipeline", unit: "", format: "%.0f",
		formula:     "blocks = pending_blocks_count\nbytes  = pending_blocks_bytes_current",
		description: "Number of L2 blocks and bytes waiting in the batcher pipeline\nto be posted to L1. Sourced from op-batcher Prometheus metrics.",
		extractFn: func(s monitor.Snapshot) []monitor.TimeValue {
			return append(s.BatcherPendingBlocks, s.BatcherPendingBytes...)
		},
		summaryFn: func(s monitor.Snapshot) monitor.SummarySnapshot { return monitor.SummarySnapshot{} },
	},
	{key: "l1_fee_pressure", label: "L1 Fee Pressure", unit: "gwei", format: "%.2f",
		formula:     "basefee = txmgr_basefee_wei / 1e9\nbumps   = txmgr_tx_gas_bump",
		description: "L1 base fee in gwei and cumulative gas bump count from the\nbatcher transaction manager. High bumps indicate fee volatility.",
		extractFn: func(s monitor.Snapshot) []monitor.TimeValue {
			return append(s.BatcherL1BaseFee, s.BatcherGasBumps...)
		},
		summaryFn: func(s monitor.Snapshot) monitor.SummarySnapshot { return monitor.SummarySnapshot{} },
	},
	{key: "deposits_withdrawals", label: "Deposits & Withdrawals", unit: "", format: "%.0f",
		formula:     "deposits  = depositTxCount − 1\nwithdrawals = count(txs to L2ToL1MessagePasser)",
		description: "Deposits: number of deposit transactions in the L2 block minus 1\n(the L1-attributes system deposit is excluded).\nWithdrawals: number of transactions sent to the L2ToL1MessagePasser\npredeploy contract.",
		extractFn: func(s monitor.Snapshot) []monitor.TimeValue {
			return append(s.Deposits, s.Withdrawals...)
		},
		summaryFn: func(s monitor.Snapshot) monitor.SummarySnapshot { return s.SumDeposits },
	},
}

// panelsByKey indexes analysisPanels by their config key.
var panelsByKey = func() map[string]chartPanel {
	m := make(map[string]chartPanel, len(analysisPanels))
	for _, p := range analysisPanels {
		m[p.key] = p
	}
	return m
}()

// filterPanels returns the subset of analysisPanels whose keys appear in names,
// preserving the order from names. Unrecognised keys are logged as warnings.
func filterPanels(names []string) []chartPanel {
	var out []chartPanel
	for _, name := range names {
		if p, ok := panelsByKey[name]; ok {
			out = append(out, p)
		} else {
			slog.Warn("Unknown panel name in config, ignoring", "panel", name)
		}
	}
	if len(out) == 0 {
		return analysisPanels
	}
	return out
}

// AnalysisModel is the Bubble Tea model for experiment analysis.
type AnalysisModel struct {
	watcher *CSVWatcher
	dir     string

	// Active panels (filtered from analysisPanels by config).
	activePanels []chartPanel
	charts       []timeserieslinechart.Model

	// Terminal dimensions
	width, height int

	// Chart zoom state
	selectedChart int
	chartScales   []anChartScale

	// Default time window from config
	defaultTimeWindow time.Duration

	// Refresh rate
	pollInterval time.Duration

	// Whether the initial auto-zoom has been applied
	initialZoomApplied bool

	// Start time for elapsed display
	startTime time.Time

	// Action log
	actionLog       *experimentLog
	showLog         bool
	logLines        int
	logScrollOffset int

	// Overlay state
	showLogOverlay   bool
	logOverlayOffset int
	showHelp         bool
	showFormula      bool

	// Error log overlay state
	showErrorOverlay   bool
	errorOverlayOffset int
	lastErrorCount     int
	errorsDismissed    bool

	// Panel config overlay (toggle panel visibility at runtime)
	panelVisible      []bool // indexed by analysisPanels position
	showPanelConfig   bool
	panelConfigCursor int

	// cancelExperiment, when non-nil, cancels the parent experiment context.
	// In raw-mode (altscreen) Ctrl+C does not generate SIGINT, so the TUI
	// must propagate the quit intent back to the experiment loop explicitly.
	cancelExperiment context.CancelFunc
}

// NewAnalysisModel creates the analysis TUI model.
func NewAnalysisModel(watcher *CSVWatcher, dir string, pollInterval time.Duration, actionLog *experimentLog, liveCfg *ResolvedLiveConfig) AnalysisModel {
	var cfg ResolvedLiveConfig
	if liveCfg != nil {
		cfg = *liveCfg
	} else {
		cfg = ResolveLiveConfig(LiveConfig{})
	}

	panels := filterPanels(cfg.Panels)

	// Build panelVisible from the initial panel selection.
	panelSet := make(map[string]bool, len(panels))
	for _, p := range panels {
		panelSet[p.key] = true
	}
	panelVisible := make([]bool, len(analysisPanels))
	for i, p := range analysisPanels {
		panelVisible[i] = panelSet[p.key]
	}

	m := AnalysisModel{
		watcher:           watcher,
		dir:               dir,
		activePanels:      panels,
		charts:            make([]timeserieslinechart.Model, len(panels)),
		chartScales:       make([]anChartScale, len(panels)),
		defaultTimeWindow: cfg.TimeWindow,
		width:             80,
		height:            40,
		pollInterval:      pollInterval,
		startTime:         time.Now(),
		actionLog:         actionLog,
		showLog:           cfg.ShowLog,
		logLines:          cfg.LogLines,
		panelVisible:      panelVisible,
	}
	for i := range m.chartScales {
		m.chartScales[i] = anChartScale{yZoom: 1.0, timeWin: cfg.TimeWindow}
	}
	m.initCharts(80, 40)
	return m
}

// rebuildActivePanels recomputes activePanels, charts, and chartScales from
// the current panelVisible state. Called after toggling panel visibility.
func (m *AnalysisModel) rebuildActivePanels() {
	var panels []chartPanel
	for i, p := range analysisPanels {
		if m.panelVisible[i] {
			panels = append(panels, p)
		}
	}
	if len(panels) == 0 {
		panels = analysisPanels
	}

	m.activePanels = panels
	m.charts = make([]timeserieslinechart.Model, len(panels))
	m.chartScales = make([]anChartScale, len(panels))
	for i := range m.chartScales {
		m.chartScales[i] = anChartScale{yZoom: 1.0, timeWin: m.defaultTimeWindow}
	}
	m.initCharts(m.width, m.height)

	if m.selectedChart >= len(m.activePanels) {
		m.selectedChart = len(m.activePanels) - 1
	}
	if m.selectedChart < 0 {
		m.selectedChart = 0
	}
	m.initialZoomApplied = false
}

func (m *AnalysisModel) initCharts(w, h int) {
	cw, ch := m.chartSize(w, h)

	for i, panel := range m.activePanels {
		opts := []timeserieslinechart.Option{
			timeserieslinechart.WithXLabelFormatter(anElapsedTimeFormatter()),
			timeserieslinechart.WithTimeRange(analysisEpoch, analysisEpoch.Add(m.defaultTimeWindow)),
			timeserieslinechart.WithXYSteps(3, 2),
		}
		if panel.unit != "" {
			opts = append(opts, timeserieslinechart.WithYLabelFormatter(anUnitYFormatter(panel.unit)))
		} else {
			opts = append(opts, timeserieslinechart.WithYLabelFormatter(anAdaptiveYFormatter))
		}
		m.charts[i] = timeserieslinechart.New(cw, ch, opts...)
	}
}

// chartLayoutRows returns the number of visual rows the chart grid occupies.
func (m *AnalysisModel) chartLayoutRows() int {
	var regular, fullWidth int
	for _, p := range m.activePanels {
		if p.fullWidth {
			fullWidth++
		} else {
			regular++
		}
	}
	pairedRows := (regular + 1) / 2
	return pairedRows + fullWidth
}

// headerHeight returns the number of terminal lines the header occupies.
// When the run legend is too wide to fit alongside the status text, it wraps
// to a third line.
func (m *AnalysisModel) headerHeight(termW int) int {
	runIDs := m.watcher.RunIDs()
	nRuns := len(runIDs)
	if nRuns == 0 || termW <= 0 {
		return 2
	}
	statusWidth := 45 // approximate "Runs: N   Rows: NNNNN   Elapsed: XXmXXs"
	legendWidth := 3  // "   " prefix
	for _, id := range runIDs {
		legendWidth += len(fmt.Sprintf("Run %d ", id))
	}
	if statusWidth+legendWidth <= termW {
		return 2
	}
	return 3
}

func (m *AnalysisModel) chartSize(termW, termH int) (int, int) {
	cols := 2
	chartRows := m.chartLayoutRows()
	if chartRows < 1 {
		chartRows = 1
	}

	chartW := (termW - 8) / cols
	if chartW < 20 {
		chartW = 20
	}

	logOverhead := 0
	if m.showLog {
		logOverhead = m.logLines + 3
	}
	headerH := m.headerHeight(termW)
	// headerH + chartRows*(chartH + 3 border/title) + log panel + footer (1)
	overhead := chartRows*3 + logOverhead + headerH + 1
	chartH := (termH - overhead) / chartRows
	if chartH < 3 {
		chartH = 3
	}
	return chartW, chartH
}

// Init implements tea.Model.
func (m AnalysisModel) Init() tea.Cmd {
	return tea.Tick(m.pollInterval, func(t time.Time) tea.Msg {
		return analysisTickMsg(t)
	})
}

// Update implements tea.Model.
func (m AnalysisModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		cw, ch := m.chartSize(msg.Width, msg.Height)
		for i := range m.charts {
			m.charts[i].Resize(cw, ch)
		}
		return m, nil

	case tea.KeyMsg:
		// Overlay-specific key handling
		if m.showHelp {
			switch msg.String() {
			case "?", "esc", "q":
				m.showHelp = false
			}
			return m, nil
		}
		if m.showFormula {
			switch msg.String() {
			case "ctrl+c":
				m.watcher.Stop()
				if m.cancelExperiment != nil {
					m.cancelExperiment()
				}
				return m, tea.Quit
			default:
				m.showFormula = false
			}
			return m, nil
		}
		if m.showLogOverlay {
			switch msg.String() {
			case "l", "esc", "q":
				m.showLogOverlay = false
			case "j", "down":
				if m.logOverlayOffset > 0 {
					m.logOverlayOffset--
				}
			case "k", "up":
				m.logOverlayOffset++
			case "pgdown":
				m.logOverlayOffset -= 10
				if m.logOverlayOffset < 0 {
					m.logOverlayOffset = 0
				}
			case "pgup":
				m.logOverlayOffset += 10
			case "g":
				if m.actionLog != nil {
					m.logOverlayOffset = len(m.actionLog.Entries()) - 1
				}
			case "G":
				m.logOverlayOffset = 0
			}
			return m, nil
		}
		if m.showErrorOverlay {
			switch msg.String() {
			case "e", "esc", "q":
				m.showErrorOverlay = false
				m.errorsDismissed = true
			case "j", "down":
				if m.errorOverlayOffset > 0 {
					m.errorOverlayOffset--
				}
			case "k", "up":
				m.errorOverlayOffset++
			case "pgdown":
				m.errorOverlayOffset -= 10
				if m.errorOverlayOffset < 0 {
					m.errorOverlayOffset = 0
				}
			case "pgup":
				m.errorOverlayOffset += 10
			case "g":
				if m.actionLog != nil {
					errors := filterErrorEntries(m.actionLog.Entries())
					m.errorOverlayOffset = len(errors) - 1
				}
			case "G":
				m.errorOverlayOffset = 0
			case "ctrl+c":
				m.watcher.Stop()
				if m.cancelExperiment != nil {
					m.cancelExperiment()
				}
				return m, tea.Quit
			}
			return m, nil
		}
		if m.showPanelConfig {
			switch msg.String() {
			case "ctrl+c":
				m.watcher.Stop()
				if m.cancelExperiment != nil {
					m.cancelExperiment()
				}
				return m, tea.Quit
			case "p", "esc":
				m.showPanelConfig = false
			case "j", "down":
				m.panelConfigCursor = (m.panelConfigCursor + 1) % len(analysisPanels)
			case "k", "up":
				m.panelConfigCursor = (m.panelConfigCursor - 1 + len(analysisPanels)) % len(analysisPanels)
			case " ", "enter":
				// Count currently visible panels to enforce minimum of 1.
				visCount := 0
				for _, v := range m.panelVisible {
					if v {
						visCount++
					}
				}
				if m.panelVisible[m.panelConfigCursor] && visCount <= 1 {
					// Can't hide the last visible panel.
				} else {
					m.panelVisible[m.panelConfigCursor] = !m.panelVisible[m.panelConfigCursor]
					m.rebuildActivePanels()
				}
			}
			return m, nil
		}

		switch msg.String() {
		case "q", "ctrl+c":
			m.watcher.Stop()
			if m.cancelExperiment != nil {
				m.cancelExperiment()
			}
			return m, tea.Quit

		// Overlays
		case "?":
			m.showHelp = true
			return m, nil
		case "f":
			m.showFormula = true
			return m, nil
		case "a":
			panel := m.activePanels[m.selectedChart]
			if isMultiDatasetPanel(panel.key) {
				m.chartScales[m.selectedChart].normalized = !m.chartScales[m.selectedChart].normalized
			}
			return m, nil
		case "l":
			m.showLogOverlay = true
			m.logOverlayOffset = 0
			return m, nil
		case "e":
			m.showErrorOverlay = true
			m.errorOverlayOffset = 0
			return m, nil
		case "p":
			m.showPanelConfig = true
			return m, nil

		// Chart selection
		case "tab":
			m.selectedChart = (m.selectedChart + 1) % len(m.activePanels)
			return m, nil
		case "shift+tab":
			m.selectedChart = (m.selectedChart - 1 + len(m.activePanels)) % len(m.activePanels)
			return m, nil

		// Y-axis zoom
		case "+", "=", "up":
			m.chartScales[m.selectedChart].yZoom *= 1.5
			return m, nil
		case "-", "down":
			z := m.chartScales[m.selectedChart].yZoom / 1.5
			if z < 0.1 {
				z = 0.1
			}
			m.chartScales[m.selectedChart].yZoom = z
			return m, nil

		// Time-axis zoom
		case "[", "left":
			tw := m.chartScales[m.selectedChart].timeWin / 2
			if tw < 10*time.Second {
				tw = 10 * time.Second
			}
			m.chartScales[m.selectedChart].timeWin = tw
			return m, nil
		case "]", "right":
			m.chartScales[m.selectedChart].timeWin *= 2
			return m, nil

		case "0":
			m.chartScales[m.selectedChart] = anChartScale{yZoom: 1.0, timeWin: m.defaultTimeWindow}
			return m, nil

		// Action log scrolling
		case "pgup":
			if m.actionLog != nil {
				maxOff := len(m.actionLog.Entries()) - m.logLines
				if maxOff < 0 {
					maxOff = 0
				}
				m.logScrollOffset += m.logLines
				if m.logScrollOffset > maxOff {
					m.logScrollOffset = maxOff
				}
			}
			return m, nil
		case "pgdown":
			m.logScrollOffset -= m.logLines
			if m.logScrollOffset < 0 {
				m.logScrollOffset = 0
			}
			return m, nil
		}

	case analysisTickMsg:
		m.refreshCharts()

		// Auto-show error overlay when new errors detected
		if m.actionLog != nil && !m.errorsDismissed {
			errors := filterErrorEntries(m.actionLog.Entries())
			if len(errors) > m.lastErrorCount {
				m.showErrorOverlay = true
				m.errorOverlayOffset = 0
			}
			m.lastErrorCount = len(errors)
		}

		return m, tea.Tick(m.pollInterval, func(t time.Time) tea.Msg {
			return analysisTickMsg(t)
		})
	}

	return m, nil
}

func (m *AnalysisModel) refreshCharts() {
	stores := m.watcher.Stores()
	runIDs := m.watcher.RunIDs()

	if len(runIDs) == 0 {
		return
	}

	cw, ch := m.chartSize(m.width, m.height)
	for i := range m.charts {
		m.charts[i].Resize(cw, ch)
	}

	// Collect snapshots for all runs.
	snapshots := make(map[int]monitor.Snapshot, len(runIDs))
	for _, id := range runIDs {
		if store, ok := stores[id]; ok {
			snapshots[id] = store.GetSnapshot()
		}
	}

	// Apply dataset styles for each run.
	for _, id := range runIDs {
		color := runColor(id)
		label := runLabel(id)
		for i := range m.charts {
			m.charts[i].SetDataSetStyle(label, color)
		}
	}

	// Push data into charts.
	for panelIdx, panel := range m.activePanels {
		m.charts[panelIdx].ClearAllData()

		norm := m.chartScales[panelIdx].normalized

		// Helper: optionally normalize a series.
		maybeNorm := func(data []monitor.TimeValue) []monitor.TimeValue {
			if norm {
				return anNormalizeTimeSeries(data)
			}
			return data
		}

		// Helper: push a slice of TimeValue into a named dataset.
		pushDS := func(name string, data []monitor.TimeValue) {
			for _, tv := range data {
				m.charts[panelIdx].PushDataSet(name, timeserieslinechart.TimePoint{
					Time: tv.Time, Value: tv.Value,
				})
			}
		}

		if panel.key == "deposits_withdrawals" {
			for _, id := range runIDs {
				snap := snapshots[id]
				depLabel := fmt.Sprintf("Run %d Dep", id)
				wdlLabel := fmt.Sprintf("Run %d Wdl", id)
				m.charts[panelIdx].SetDataSetStyle(depLabel, runColor(id))
				wdlColor := lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
				if id > 1 {
					wdlColor = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))
				}
				m.charts[panelIdx].SetDataSetStyle(wdlLabel, wdlColor)
				pushDS(depLabel, maybeNorm(snap.Deposits))
				pushDS(wdlLabel, maybeNorm(snap.Withdrawals))
			}
		} else if panel.key == "batcher_pipeline" {
			for _, id := range runIDs {
				snap := snapshots[id]
				blkLabel := fmt.Sprintf("Run %d Blk", id)
				bytLabel := fmt.Sprintf("Run %d Byt", id)
				m.charts[panelIdx].SetDataSetStyle(blkLabel, lipgloss.NewStyle().Foreground(lipgloss.Color("75")))
				m.charts[panelIdx].SetDataSetStyle(bytLabel, lipgloss.NewStyle().Foreground(lipgloss.Color("215")))
				pushDS(blkLabel, maybeNorm(snap.BatcherPendingBlocks))
				pushDS(bytLabel, maybeNorm(snap.BatcherPendingBytes))
			}
		} else if panel.key == "l1_fee_pressure" {
			for _, id := range runIDs {
				snap := snapshots[id]
				feeLabel := fmt.Sprintf("Run %d Fee", id)
				bmpLabel := fmt.Sprintf("Run %d Bmp", id)
				m.charts[panelIdx].SetDataSetStyle(feeLabel, lipgloss.NewStyle().Foreground(lipgloss.Color("220")))
				m.charts[panelIdx].SetDataSetStyle(bmpLabel, lipgloss.NewStyle().Foreground(lipgloss.Color("196")))
				pushDS(feeLabel, maybeNorm(snap.BatcherL1BaseFee))
				pushDS(bmpLabel, maybeNorm(snap.BatcherGasBumps))
			}
		} else {
			for _, id := range runIDs {
				snap := snapshots[id]
				label := runLabel(id)
				data := panel.extractFn(snap)
				for _, tv := range data {
					m.charts[panelIdx].PushDataSet(label, timeserieslinechart.TimePoint{
						Time: tv.Time, Value: tv.Value,
					})
				}
			}
		}
	}

	// Auto-fit time window on first tick with data.
	if !m.initialZoomApplied {
		m.autoFitTimeWindow(snapshots, runIDs)
	}

	// Apply zoom to all charts; swap Y-axis formatter when normalized.
	for i, panel := range m.activePanels {
		var allData []monitor.TimeValue
		for _, id := range runIDs {
			snap := snapshots[id]
			switch panel.key {
			case "deposits_withdrawals":
				allData = append(allData, snap.Deposits...)
				allData = append(allData, snap.Withdrawals...)
			case "batcher_pipeline":
				allData = append(allData, snap.BatcherPendingBlocks...)
				allData = append(allData, snap.BatcherPendingBytes...)
			case "l1_fee_pressure":
				allData = append(allData, snap.BatcherL1BaseFee...)
				allData = append(allData, snap.BatcherGasBumps...)
			default:
				allData = append(allData, panel.extractFn(snap)...)
			}
		}
		if m.chartScales[i].normalized && isMultiDatasetPanel(panel.key) {
			allData = anNormalizeTimeSeries(allData)
			m.charts[i].YLabelFormatter = anNormalizedYFormatter
		} else if panel.unit != "" {
			m.charts[i].YLabelFormatter = anUnitYFormatter(panel.unit)
		} else {
			m.charts[i].YLabelFormatter = anAdaptiveYFormatter
		}
		m.applyChartZoom(&m.charts[i], m.chartScales[i], allData)
	}
}

func (m *AnalysisModel) autoFitTimeWindow(snapshots map[int]monitor.Snapshot, runIDs []int) {
	var earliest, latest time.Time
	found := false

	for _, id := range runIDs {
		snap := snapshots[id]
		allSeries := [][]monitor.TimeValue{
			snap.TPS, snap.L2TxCost, snap.L2TxSpeed, snap.L2TxCount,
			snap.BatcherPostCost, snap.BatcherDataSize, snap.CompressionRatio,
			snap.Deposits, snap.Withdrawals,
			snap.BatcherPendingBlocks, snap.BatcherPendingBytes,
			snap.BatcherL1BaseFee, snap.BatcherGasBumps,
		}
		for _, series := range allSeries {
			if len(series) == 0 {
				continue
			}
			first := series[0].Time
			last := series[len(series)-1].Time
			if !found || first.Before(earliest) {
				earliest = first
			}
			if !found || last.After(latest) {
				latest = last
			}
			found = true
		}
	}

	if !found {
		return
	}

	m.initialZoomApplied = true

	span := latest.Sub(earliest)
	if span <= m.defaultTimeWindow {
		return
	}

	fitWindow := span + span/10
	if fitWindow < m.defaultTimeWindow {
		fitWindow = m.defaultTimeWindow
	}
	for i := range m.chartScales {
		m.chartScales[i].timeWin = fitWindow
	}
}

func (m *AnalysisModel) applyChartZoom(chart *timeserieslinechart.Model, scale anChartScale, data []monitor.TimeValue) {
	minY, maxY, dataEarliest, dataLatest := anDataRange(data)

	latest := dataLatest
	if latest.IsZero() {
		latest = analysisEpoch.Add(scale.timeWin)
	}
	earliest := latest.Add(-scale.timeWin)
	if !dataEarliest.IsZero() && dataEarliest.Before(earliest) {
		earliest = dataEarliest
	}
	chart.SetTimeRange(earliest, latest)
	chart.SetViewTimeRange(latest.Add(-scale.timeWin), latest)

	if len(data) == 0 {
		return
	}
	mid := (minY + maxY) / 2
	halfRange := (maxY - minY) / 2
	if halfRange < 1e-9 {
		halfRange = 1.0
	}
	adjustedHalf := halfRange / scale.yZoom
	lo := mid - adjustedHalf
	if lo < 0 {
		lo = 0
	}
	chart.SetYRange(lo, mid+adjustedHalf)
	chart.SetViewYRange(lo, mid+adjustedHalf)
}

// View implements tea.Model.
func (m AnalysisModel) View() string {
	stores := m.watcher.Stores()
	runIDs := m.watcher.RunIDs()
	totalRows := m.watcher.TotalRows()

	cw, _ := m.chartSize(m.width, m.height)

	// Header
	elapsed := time.Since(m.startTime).Round(time.Second)
	dirName := m.dir
	if dirName == "" {
		dirName = "(live)"
	} else {
		dirName = filepath.Base(dirName)
	}
	headerLine1 := anTitleStyle.Render(fmt.Sprintf(
		"Experiment Analysis   Dir: %s", dirName,
	))
	headerLine2 := anStatusStyle.Render(fmt.Sprintf(
		"Runs: %d   Rows: %d   Elapsed: %s",
		len(runIDs), totalRows, elapsed,
	))

	// Run legend — place on a separate line when it would overflow terminal width.
	var legendParts []string
	for _, id := range runIDs {
		style := runColor(id)
		legendParts = append(legendParts, style.Render(fmt.Sprintf("Run %d", id)))
	}
	if len(legendParts) > 0 {
		legendStr := strings.Join(legendParts, " ")
		combined := headerLine2 + "   " + legendStr
		if lipgloss.Width(combined) <= m.width {
			headerLine2 = combined
		} else {
			headerLine2 += "\n" + legendStr
		}
	}

	header := headerLine1 + "\n" + headerLine2

	// Build snapshots for summary display.
	snapshots := make(map[int]monitor.Snapshot, len(runIDs))
	for _, id := range runIDs {
		if store, ok := stores[id]; ok {
			snapshots[id] = store.GetSnapshot()
		}
	}

	anNormStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("82"))

	anChartTitleSuffix := func(chartIdx int) string {
		var suffix string
		if m.chartScales[chartIdx].normalized {
			suffix += " " + anNormStyle.Render("[norm]")
		}
		if zi := anZoomIndicator(m.chartScales[chartIdx], m.defaultTimeWindow); zi != "" {
			suffix += " " + anStatusStyle.Render(zi)
		}
		return suffix
	}

	// Render a single chart panel (normal width).
	renderPanel := func(chartIdx int) string {
		panel := m.activePanels[chartIdx]
		chart := &m.charts[chartIdx]
		chart.DrawAll()

		var lastVals []string
		for _, id := range runIDs {
			snap := snapshots[id]
			data := panel.extractFn(snap)
			style := runColor(id)
			lastVals = append(lastVals, style.Render(
				fmt.Sprintf("R%d:%s", id, anLastValueStr(data, panel.format)),
			))
		}

		title := anChartLabelStyle.Render(fmt.Sprintf(" %s ", panel.label))
		if len(lastVals) > 0 {
			title += strings.Join(lastVals, " ")
		}

		var allData []monitor.TimeValue
		for _, id := range runIDs {
			snap := snapshots[id]
			allData = append(allData, panel.extractFn(snap)...)
		}
		if len(allData) > 0 {
			_, maxY := anDataYRange(allData)
			title += " " + anPeakStyle.Render(fmt.Sprintf("peak:%s", anFmtSummaryVal(maxY, panel.format)))
		}

		title += anChartTitleSuffix(chartIdx)

		content := chart.View()
		borderStyle := anChartBorderStyle
		if chartIdx == m.selectedChart {
			borderStyle = anSelectedChartBorderStyle
		}
		return borderStyle.Width(cw + 2).Render(title + "\n" + content)
	}

	// Render a full-width chart panel.
	renderFullWidthPanel := func(chartIdx int) string {
		panel := m.activePanels[chartIdx]
		chart := &m.charts[chartIdx]
		chart.DrawAll()

		title := anChartLabelStyle.Render(fmt.Sprintf(" %s ", panel.label)) + anChartTitleSuffix(chartIdx)

		content := chart.View()
		borderStyle := anChartBorderStyle
		if chartIdx == m.selectedChart {
			borderStyle = anSelectedChartBorderStyle
		}
		return borderStyle.Width(cw*2 + 6).Render(title + "\n" + content)
	}

	// Separate panels into regular (2-column) and full-width.
	var regularIdx, fullWidthIdx []int
	for i, p := range m.activePanels {
		if p.fullWidth {
			fullWidthIdx = append(fullWidthIdx, i)
		} else {
			regularIdx = append(regularIdx, i)
		}
	}

	parts := []string{header}

	// Render regular panels in 2-column rows.
	for i := 0; i < len(regularIdx); i += 2 {
		if i+1 < len(regularIdx) {
			row := lipgloss.JoinHorizontal(lipgloss.Top,
				renderPanel(regularIdx[i]), renderPanel(regularIdx[i+1]))
			parts = append(parts, row)
		} else {
			parts = append(parts, renderPanel(regularIdx[i]))
		}
	}

	// Render full-width panels.
	for _, idx := range fullWidthIdx {
		parts = append(parts, renderFullWidthPanel(idx))
	}

	// Action Log panel
	if m.showLog {
		logPanel := m.renderLogPanel(cw*2 + 6)
		parts = append(parts, logPanel)
	}

	// Footer
	footer := anStatusStyle.Render(
		"q: quit  Tab: chart  ←→↑↓: zoom  0: reset  p: panels  f: formula  a: normalize  l: log  e: errors  ?: help",
	)
	parts = append(parts, footer)

	base := lipgloss.JoinVertical(lipgloss.Left, parts...)

	if m.showPanelConfig {
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			m.renderPanelConfigOverlay(),
			lipgloss.WithWhitespaceChars(" "),
		)
	}
	if m.showHelp {
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			m.renderHelpOverlay(),
			lipgloss.WithWhitespaceChars(" "),
		)
	}
	if m.showFormula {
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			m.renderFormulaOverlay(),
			lipgloss.WithWhitespaceChars(" "),
		)
	}
	if m.showLogOverlay {
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			m.renderLogOverlay(),
			lipgloss.WithWhitespaceChars(" "),
		)
	}
	if m.showErrorOverlay {
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			m.renderErrorOverlay(),
			lipgloss.WithWhitespaceChars(" "),
		)
	}

	return base
}

func (m AnalysisModel) renderLogPanel(width int) string {
	if m.actionLog == nil {
		return ""
	}
	entries := m.actionLog.Entries()
	total := len(entries)
	visLines := m.logLines

	end := total - m.logScrollOffset
	if end < 0 {
		end = 0
	}
	start := end - visLines
	if start < 0 {
		start = 0
	}

	var lines []string
	for _, entry := range entries[start:end] {
		ts := anLogTimeStyle.Render(entry.Time.Format("15:04:05"))
		msg := entry.Message
		style := anLogMsgStyle
		if strings.Contains(msg, "error") || strings.Contains(msg, "FAILED") {
			style = anLogErrorStyle
		} else if strings.Contains(msg, "WARNING") || strings.Contains(msg, "SLOW") {
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
		}
		lines = append(lines, fmt.Sprintf("%s %s", ts, style.Render(msg)))
	}

	if len(lines) == 0 {
		lines = append(lines, anLogTimeStyle.Render("  (waiting for experiment output...)"))
	}
	for len(lines) < visLines {
		lines = append(lines, "")
	}

	content := strings.Join(lines, "\n")
	title := anLogLabelStyle.Render(" Action Log")
	if m.logScrollOffset > 0 {
		title += " " + anStatusStyle.Render(fmt.Sprintf("(+%d)", m.logScrollOffset))
	}
	return anLogBorderStyle.Width(width).Render(title + "\n" + content)
}

func (m AnalysisModel) renderHelpOverlay() string {
	help := []struct{ key, desc string }{
		{"q / Ctrl+C", "Quit"},
		{"Tab / Shift+Tab", "Cycle selected chart"},
		{"↑ / ↓  or  + / -", "Y-axis zoom (selected chart)"},
		{"← / →  or  [ / ]", "Time-axis zoom (selected chart)"},
		{"0", "Reset zoom on selected chart"},
		{"PgUp / PgDn", "Scroll action log"},
		{"p", "Show/hide panels"},
		{"f", "Show chart formula / description"},
		{"a", "Toggle normalized view (multi-dataset charts)"},
		{"l", "Full-screen action log"},
		{"?", "Toggle this help"},
	}

	var lines []string
	for _, h := range help {
		keyStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
		lines = append(lines, fmt.Sprintf("  %s  %s", keyStyle.Render(fmt.Sprintf("%-20s", h.key)), h.desc))
	}

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Align(lipgloss.Center)
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	content := strings.Join([]string{
		titleStyle.Render("Experiment Analysis — Help"),
		"",
		strings.Join(lines, "\n"),
		"",
		hintStyle.Render("Press ? or Esc to close"),
	}, "\n")

	boxWidth := 60
	if m.width-6 < boxWidth {
		boxWidth = m.width - 6
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Padding(1, 3).
		Width(boxWidth).
		Render(content)
}

func (m AnalysisModel) renderFormulaOverlay() string {
	panel := m.activePanels[m.selectedChart]

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("205")).
		Align(lipgloss.Center)

	formulaStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("99"))

	descStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("252"))

	sectionStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("214"))

	hintStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241"))

	lines := []string{
		titleStyle.Render(panel.label),
		"",
		sectionStyle.Render("Formula"),
		formulaStyle.Render(panel.formula),
		"",
		sectionStyle.Render("Description"),
		descStyle.Render(panel.description),
	}

	if m.chartScales[m.selectedChart].normalized {
		normHeaderStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("82"))
		normDescStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
		lines = append(lines,
			"",
			normHeaderStyle.Render("[Normalized View Active]"),
			normDescStyle.Render("Each series is divided by its own maximum value,\nmapping all series to a 0–1 range for visual comparison."),
		)
	}

	lines = append(lines, "", hintStyle.Render("      Press any key to close"))

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Padding(1, 3).
		Render(strings.Join(lines, "\n"))
}

func (m AnalysisModel) renderPanelConfigOverlay() string {
	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("205")).
		Align(lipgloss.Center)

	normalStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("252"))

	cursorStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("205"))

	checkStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("82"))

	uncheckStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241"))

	hintStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241"))

	var lines []string
	for i, panel := range analysisPanels {
		check := uncheckStyle.Render("[ ]")
		if m.panelVisible[i] {
			check = checkStyle.Render("[x]")
		}
		label := normalStyle.Render(panel.label)
		prefix := "  "
		if i == m.panelConfigCursor {
			prefix = cursorStyle.Render("> ")
			label = cursorStyle.Render(panel.label)
		}
		lines = append(lines, fmt.Sprintf("%s%s %s", prefix, check, label))
	}

	content := strings.Join([]string{
		titleStyle.Render("Panel Configuration"),
		"",
		strings.Join(lines, "\n"),
		"",
		hintStyle.Render("j/k: navigate  Space/Enter: toggle  p/Esc: close"),
	}, "\n")

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Padding(1, 3).
		Render(content)
}

func (m AnalysisModel) renderLogOverlay() string {
	if m.actionLog == nil {
		return ""
	}
	entries := m.actionLog.Entries()
	total := len(entries)

	visibleLines := m.height - 9
	if visibleLines < 3 {
		visibleLines = 3
	}

	end := total - m.logOverlayOffset
	if end < 0 {
		end = 0
	}
	start := end - visibleLines
	if start < 0 {
		start = 0
	}

	var lines []string
	for _, entry := range entries[start:end] {
		ts := anLogTimeStyle.Render(entry.Time.Format("15:04:05"))
		msg := entry.Message
		style := anLogMsgStyle
		if strings.Contains(msg, "error") || strings.Contains(msg, "FAILED") {
			style = anLogErrorStyle
		} else if strings.Contains(msg, "WARNING") || strings.Contains(msg, "SLOW") {
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
		}
		lines = append(lines, fmt.Sprintf("%s %s", ts, style.Render(msg)))
	}
	if len(lines) == 0 {
		lines = append(lines, anLogTimeStyle.Render("  (no log entries yet)"))
	}
	for len(lines) < visibleLines {
		lines = append(lines, "")
	}

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Align(lipgloss.Center)
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	titleText := fmt.Sprintf("Action Log [%d entries]", total)
	if m.logOverlayOffset > 0 {
		showing := end - start
		titleText = fmt.Sprintf("Action Log [%d-%d of %d]", start+1, start+showing, total)
	}

	content := strings.Join([]string{
		titleStyle.Render(titleText),
		"",
		strings.Join(lines, "\n"),
		"",
		hintStyle.Render("j/k: scroll  PgUp/PgDn: page  l/Esc: close"),
	}, "\n")

	boxWidth := m.width - 6
	if boxWidth < 40 {
		boxWidth = 40
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Padding(1, 3).
		Width(boxWidth).
		Render(content)
}

func (m AnalysisModel) renderErrorOverlay() string {
	if m.actionLog == nil {
		return ""
	}
	entries := filterErrorEntries(m.actionLog.Entries())
	total := len(entries)

	visibleLines := m.height - 9
	if visibleLines < 3 {
		visibleLines = 3
	}

	end := total - m.errorOverlayOffset
	if end < 0 {
		end = 0
	}
	start := end - visibleLines
	if start < 0 {
		start = 0
	}

	var lines []string
	for _, entry := range entries[start:end] {
		ts := anLogTimeStyle.Render(entry.Time.Format("15:04:05"))
		msg := entry.Message
		lines = append(lines, fmt.Sprintf("%s %s", ts, anLogErrorStyle.Render(msg)))
	}
	if len(lines) == 0 {
		lines = append(lines, anLogTimeStyle.Render("  (no errors)"))
	}
	for len(lines) < visibleLines {
		lines = append(lines, "")
	}

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196")).Align(lipgloss.Center)
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	titleText := fmt.Sprintf("Errors [%d]", total)
	if m.errorOverlayOffset > 0 {
		showing := end - start
		titleText = fmt.Sprintf("Errors [%d-%d of %d]", start+1, start+showing, total)
	}

	content := strings.Join([]string{
		titleStyle.Render(titleText),
		"",
		strings.Join(lines, "\n"),
		"",
		hintStyle.Render("j/k: scroll  PgUp/PgDn: page  e/Esc: close"),
	}, "\n")

	boxWidth := m.width - 6
	if boxWidth < 40 {
		boxWidth = 40
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("196")).
		Padding(1, 3).
		Width(boxWidth).
		Render(content)
}

// RunAnalysisTUI starts the analysis TUI and blocks until it exits.
func RunAnalysisTUI(watcher *CSVWatcher, dir string, pollInterval time.Duration, actionLog *experimentLog, liveCfg *ResolvedLiveConfig) error {
	model := NewAnalysisModel(watcher, dir, pollInterval, actionLog, liveCfg)
	p := tea.NewProgram(model, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// RunAnalysisTUIAsync starts the analysis TUI in the background and returns
// a wait function that blocks until the TUI exits, and a quit function that
// can be called to programmatically stop the TUI. The cancel function, when
// non-nil, is called when the user quits the TUI so that the experiment
// context is also cancelled (necessary because raw-mode terminals don't
// deliver SIGINT on Ctrl+C).
func RunAnalysisTUIAsync(watcher *CSVWatcher, dir string, pollInterval time.Duration, actionLog *experimentLog, liveCfg *ResolvedLiveConfig, cancel context.CancelFunc) (wait func() error, quit func()) {
	model := NewAnalysisModel(watcher, dir, pollInterval, actionLog, liveCfg)
	model.cancelExperiment = cancel
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithoutSignalHandler())

	errCh := make(chan error, 1)
	go func() {
		_, err := p.Run()
		errCh <- err
	}()

	return func() error {
			return <-errCh
		}, func() {
			p.Quit()
		}
}

// ExperimentSampleToMetricMap is an exported alias for sampleToMetricMap,
// allowing the standalone analysis binary to use it.
func ExperimentSampleToMetricMap(s ExperimentSample) map[string]float64 {
	return sampleToMetricMap(s)
}

// AnalysisCSVHeader returns the canonical experiment CSV header for reference.
func AnalysisCSVHeader() []string {
	return ExperimentSampleCSVHeader()
}

// ParseExperimentSample wraps FromCSVRow for external callers.
func ParseExperimentSample(header, row []string) (ExperimentSample, error) {
	var s ExperimentSample
	err := s.FromCSVRow(header, row)
	return s, err
}

// NewAnalysisMetricsStore creates a MetricsStore sized for experiment analysis.
func NewAnalysisMetricsStore() *monitor.MetricsStore {
	return monitor.NewMetricsStore(10000)
}

// FormatRunID returns a string like "Run 1".
func FormatRunID(id int) string {
	return runLabel(id)
}

// RunIDFromCSVPath extracts the run ID from a filename like experiment_run_3.csv.
func RunIDFromCSVPath(path string) (int, error) {
	base := filepath.Base(path)
	base = strings.TrimPrefix(base, "experiment_run_")
	base = strings.TrimSuffix(base, ".csv")
	return strconv.Atoi(base)
}
