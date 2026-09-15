package monitor

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/NimbleMarkets/ntcharts/linechart/timeserieslinechart"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/fedejinich/rootstack-operator-tools/rsk/opdeployer/keyderive"

	tea "github.com/charmbracelet/bubbletea"
)

// Chart panel labels.
const (
	labelTPS             = "TPS"
	labelL2TxCost        = "L2 Tx Cost (RBTC)"
	labelL2TxSpeed       = "L2 Tx Speed (s)"
	labelL2TxCount       = "Txs / L2 Block"
	labelBatcherPostCost = "Batcher Post Cost (RBTC)"
	labelBatcherData     = "Batch Data (bytes)"
	labelDeposits        = "Deposits & Withdrawals"

	// Prometheus-sourced panels
	labelDerivHealth     = "Derivation Health"
	labelBatcherPipeline = "Batcher Pipeline"
	labelL1FeePressure   = "L1 Fee Pressure"
	labelProcessHealth   = "Process Health (MB / goroutines)"
	labelProposerSeqNum  = "Proposer Progress"

	// On-chain derived panels
	labelPostingFreq      = "Posting Frequency (s)"
	labelSafeHeadLag      = "Safe Head Lag (blocks)"
	labelTxPoolStatus     = "TxPool Status"
	labelL1TxCount        = "Txs / L1 Block"
	labelL2BlocksPerBatch = "L2 Blocks per Batch"
)

// chartPanelLabels maps each chart index to its display label.
var chartPanelLabels = [numCharts]string{
	labelTPS, labelL2TxCost, labelL2TxSpeed, labelL2TxCount,
	labelBatcherPostCost, labelBatcherData, labelDeposits,
	labelDerivHealth, labelBatcherPipeline, labelL1FeePressure,
	labelProcessHealth, labelProposerSeqNum,
	labelPostingFreq, labelSafeHeadLag, labelTxPoolStatus,
	labelL1TxCount, labelL2BlocksPerBatch,
}

// chartFormulaEntry holds the title, formula, and description for a chart panel.
type chartFormulaEntry struct {
	Title       string
	Formula     string
	Description string
}

// chartFormulas maps each chart index (0-6) to its formula explanation.
var chartFormulas = [numCharts]chartFormulaEntry{
	{ // 0 – TPS
		Title:       "TPS (Transactions Per Second)",
		Formula:     "TPS = userTxCount / blockTime",
		Description: "Number of non-deposit L2 transactions in the block divided by the\nblock time in seconds (clamped to a minimum of 2s).",
	},
	{ // 1 – L2 Tx Cost
		Title:       "L2 Tx Cost (RBTC)",
		Formula:     "cost = avg(gasPrice × gasUsed + l1Fee)",
		Description: "Mean gas cost across user transactions in the L2 block.\ngasPrice × gasUsed gives the L2 execution cost, l1Fee is the L1\ndata-availability surcharge. The sum is converted from wei to RBTC.",
	},
	{ // 2 – L2 Tx Speed
		Title:       "L2 Tx Speed (seconds)",
		Formula:     "speed = receiptTime − sendTime",
		Description: "Seconds elapsed from transaction submission to receipt confirmation.\nOnly recorded when the traffic simulator is active.",
	},
	{ // 3 – Txs / L2 Block
		Title:       "Transactions per L2 Block",
		Formula:     "count = len(non-deposit txs)",
		Description: "Number of user (non-deposit) transactions included in each L2 block.\nDeposit transactions originating from L1 are excluded from the count.",
	},
	{ // 4 – Batcher Post Cost
		Title:       "Batcher Post Cost (RBTC)",
		Formula:     "cost = gasPrice × gasUsed",
		Description: "Gas cost of the batcher transaction posted to the batch inbox on L1,\nconverted from wei to RBTC.",
	},
	{ // 5 – Batch Data / Compression Ratio
		Title:       "Batch Data / Compression Ratio",
		Formula:     "bytes = len(batcherTx.data)\nratio = comprRatioSum / comprRatioCount",
		Description: "When compression metrics are unavailable: raw calldata size of the\nbatcher transaction in bytes.\nWhen available: compression ratio from op_batcher Prometheus metrics\n(channel_compr_ratio_sum / channel_compr_ratio_count).",
	},
	{ // 6 – Deposits & Withdrawals
		Title:       "Deposits & Withdrawals",
		Formula:     "deposits  = depositTxCount − 1\nwithdrawals = count(txs to L2ToL1MessagePasser)",
		Description: "Deposits: number of deposit transactions in the L2 block minus 1\n(the L1-attributes system deposit is excluded).\nWithdrawals: number of transactions sent to the L2ToL1MessagePasser\npredeploy contract.",
	},
	{ // 7 – Derivation Health
		Title:       "Derivation Health (errors / resets per poll)",
		Formula:     "errors = Δ(derivation_errors_total)\nresets = Δ(pipeline_resets_total)",
		Description: "Delta of op-node derivation error and pipeline reset counters\nper poll interval. Nonzero values indicate derivation issues.",
	},
	{ // 8 – Batcher Pipeline
		Title:       "Batcher Pipeline",
		Formula:     "pending_blocks = op_batcher pending_blocks_count\npending_bytes = op_batcher pending_blocks_bytes_current",
		Description: "Number of L2 blocks and bytes pending in the batcher's queue.\nHigh values indicate the batcher is falling behind.",
	},
	{ // 9 – L1 Fee Pressure
		Title:       "L1 Fee Pressure",
		Formula:     "base_fee = txmgr_basefee_wei / 1e9  (gwei)\ngas_bumps = txmgr_tx_gas_bump",
		Description: "L1 base fee (in gwei) and gas bump count from the batcher's\ntransaction manager. High bumps indicate fee contention.",
	},
	{ // 10 – Process Health
		Title:       "Process Health (Memory & Goroutines)",
		Formula:     "heap_mb = HeapInuse / 1048576\ngoroutines = count(goroutine headers)",
		Description: "Heap memory usage (MB) and goroutine count from pprof endpoints.\nRising goroutines may indicate a leak.",
	},
	{ // 11 – Proposer Progress
		Title:       "Proposer Progress",
		Formula:     "seq_num = proposed_sequence_number",
		Description: "Latest proposed output sequence number from the op-proposer.\nShould advance steadily over time.",
	},
	{ // 12 – Posting Frequency
		Title:       "Posting Frequency (seconds)",
		Formula:     "interval = batcherTx[n].blockTime − batcherTx[n-1].blockTime",
		Description: "Seconds elapsed between consecutive batcher L1 transactions.\nLong intervals may indicate the batcher is stalled or L1 congestion.",
	},
	{ // 13 – Safe Head Lag
		Title:       "Safe Head Lag (blocks)",
		Formula:     "lag = unsafeL2 − safeL2",
		Description: "Number of L2 blocks between the unsafe (latest) and safe (derived)\nL2 heads. High lag means the derivation pipeline is falling behind.",
	},
	{ // 14 – TxPool Status
		Title:       "TxPool Status",
		Formula:     "pending = txpool_status.pending\nqueued = txpool_status.queued",
		Description: "Number of pending (executable) and queued (non-executable)\ntransactions in the L2 node's transaction pool.",
	},
	{ // 15 – L1 Tx Count
		Title:       "Transactions per L1 Block",
		Formula:     "count = len(block.Transactions())",
		Description: "Total number of transactions included in each L1 block.\nIncludes all L1 activity, not just batcher transactions.",
	},
	{ // 16 – L2 Blocks per Batch
		Title:       "L2 Blocks per Batch Channel",
		Formula:     "l2_blocks = count(SingularBatch in channel)\nl1_txs = frames.len (1 frame per L1 tx)",
		Description: "Number of L2 blocks packed into each completed batch channel,\ndecoded from batcher calldata. Also shows how many L1 txs\nwere needed to post each channel.",
	},
}

// cmdRegistryEntry describes a single command for completion and help.
type cmdRegistryEntry struct {
	Cmd  string // e.g. "deposit rbtc"
	Desc string // e.g. "Trigger L1 RBTC deposit"
}

// cmdRegistry lists all available commands for tab-completion and help overlay.
var cmdRegistry = []cmdRegistryEntry{
	{"deposit rbtc", "Deposit RBTC (default: config amount)"},
	{"deposit usdrif", "Deposit USDRIF (default: config amount)"},
	{"withdraw rbtc", "Withdraw RBTC (default: config amount)"},
	{"withdraw usdrif", "Withdraw USDRIF (default: config amount)"},
	{"traffic on", "Start traffic simulator"},
	{"traffic off", "Stop traffic simulator"},
	{"traffic toggle", "Toggle traffic on/off"},
	{"traffic rate", "Set traffic rate (tx/s)"},
	{"traffic accounts", "Set sender account count"},
	{"gas bump", "Bump gas multiplier (1.5x)"},
	{"poll", "Set poll interval (e.g. 2s)"},
	{"zoom reset", "Reset zoom on selected chart"},
	{"set", "Set config value"},
	{"info", "Show connection details & balances"},
	{"scan", "Scan derived accounts (all configured or :scan <key>)"},
	{"events clear", "Clear completed/failed events"},
	{"help", "Show help"},
	{"quit", "Quit"},
}

// filterCmdSuggestions returns registry entries whose Cmd starts with the given prefix.
func filterCmdSuggestions(prefix string) []cmdRegistryEntry {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		return cmdRegistry
	}
	var results []cmdRegistryEntry
	for _, entry := range cmdRegistry {
		if strings.HasPrefix(entry.Cmd, prefix) {
			results = append(results, entry)
		}
	}
	return results
}

// logLines is the number of visible log entries in the Action Log panel.
const logLines = 3

// Keyboard message types
type tickMsg time.Time

// l1SenderCreatedMsg is sent when async L1Sender creation completes.
type l1SenderCreatedMsg struct {
	sender   *L1Sender
	err      error
	retryKey string
}

// retryActionMsg re-dispatches an action key after a prompt or async operation completes.
type retryActionMsg struct {
	key string
}

// defaultMaxTrafficRate is the fallback cap before theoretical max TPS is known.
const defaultMaxTrafficRate = 100_000

// numCharts is the number of chart panels in the TUI.
const numCharts = 17

// defaultTimeWindow is the default visible time window for charts.
const defaultTimeWindow = 5 * time.Minute

// chartScale holds per-chart zoom state.
type chartScale struct {
	yZoom      float64       // 1.0 = auto; >1 = zoomed in (narrower Y range); <1 = zoomed out
	timeWin    time.Duration // visible time window
	normalized bool          // true = show each series as fraction of its own peak (0–1)
}

// isMultiDatasetChart returns true for chart indices that plot more than one data series.
func isMultiDatasetChart(idx int) bool {
	switch idx {
	case 6, 7, 8, 9, 10, 14, 16:
		return true
	}
	return false
}

// multiDatasetFormulaColors maps chart indices to colors for each formula line (by line order).
// Colors match the dataset styles set in initCharts.
var multiDatasetFormulaColors = map[int][]string{
	6:  {"82", "196"},  // Deposits & Withdrawals: green, red
	7:  {"196", "214"}, // Derivation Health: red, orange
	8:  {"75", "215"},  // Batcher Pipeline: blue, orange
	9:  {"220", "196"}, // L1 Fee Pressure: gold, red
	10: {"75", "215"},  // Process Health: blue, orange
	14: {"75", "215"},  // TxPool Status: blue, orange
	16: {"82", "214"},  // L2 Blocks per Batch: green, orange
}

// Styles
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("205"))
	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))
	chartBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("62")).
				Padding(0, 0)
	selectedChartBorderStyle = lipgloss.NewStyle().
					Border(lipgloss.RoundedBorder()).
					BorderForeground(lipgloss.Color("205")).
					Padding(0, 0)
	chartLabelStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("99"))
	logBorderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)
	logLabelStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("214"))
	logTimeStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))
	logMsgStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))
	logErrorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196"))
	depositColor  = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))  // green
	withdrawColor = lipgloss.NewStyle().Foreground(lipgloss.Color("196")) // red
	avgStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("244")) // dim grey
	peakStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("220")) // gold
	costStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("159")) // light cyan

	// Batcher status indicator styles
	batcherOKStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("82"))  // green
	batcherWarnStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214")) // yellow/orange
	batcherErrStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196")) // red
	batcherUnknownStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))            // dim grey
)

// DerivedAccountBalance holds balance info for a derived account.
type DerivedAccountBalance struct {
	Role    string
	Address string
	L1Bal   float64
	L2Bal   float64
}

// Model is the Bubble Tea model for the monitor TUI.
type Model struct {
	ctx     context.Context
	cfg     *Config
	metrics *MetricsStore

	// Charts (12 panels: deposits+withdrawals share one)
	chartTPS             timeserieslinechart.Model
	chartL2TxCost        timeserieslinechart.Model
	chartL2TxSpeed       timeserieslinechart.Model
	chartL2TxCount       timeserieslinechart.Model
	chartBatcherPostCost timeserieslinechart.Model
	chartBatcherData     timeserieslinechart.Model
	chartDeposits        timeserieslinechart.Model // has "deposits" and "withdrawals" data sets

	// Prometheus-sourced charts
	chartDerivHealth     timeserieslinechart.Model // derivation errors + pipeline resets
	chartBatcherPipeline timeserieslinechart.Model // pending blocks + pending bytes
	chartL1FeePressure   timeserieslinechart.Model // L1 base fee + gas bumps
	chartProcessHealth   timeserieslinechart.Model // heap MB + goroutines
	chartProposerSeqNum  timeserieslinechart.Model // proposed sequence number

	// On-chain derived charts
	chartPostingFreq      timeserieslinechart.Model // batcher posting interval
	chartSafeHeadLag      timeserieslinechart.Model // unsafe-safe L2 head gap
	chartTxPoolStatus     timeserieslinechart.Model // pending + queued txpool counts
	chartL1TxCount        timeserieslinechart.Model // txs per L1 block
	chartL2BlocksPerBatch timeserieslinechart.Model // L2 blocks per completed channel

	// Terminal dimensions
	width, height int

	// Chart zoom state
	selectedChart int                   // 0-6: which chart is selected for zoom controls
	chartScales   [numCharts]chartScale // per-chart Y-zoom and time window

	// Refresh rate (mutable at runtime)
	pollInterval time.Duration

	// Traffic simulator state
	trafficOn        bool
	trafficRate      float64
	trafficAccounts  int  // target number of sender accounts
	trafficGasPaused bool // mirrors TrafficSimulator.IsPausedForGas()

	// Help overlay
	showHelp bool

	// Prompt overlay for missing config values
	promptActive   bool
	promptLabel    string          // display label, e.g. "USDRIF L1 Token Address"
	promptField    string          // TOML key, e.g. "usdrif_token_l1"
	promptInput    textinput.Model // text input component
	promptRetryKey string          // original key to re-process after prompt completes

	// Vim-like command mode (activated by pressing ':')
	cmdMode        bool
	cmdInput       textinput.Model
	cmdSuggestions []cmdRegistryEntry // filtered suggestions matching current input
	cmdSuggIdx     int                // selected suggestion index (-1 = none)

	// Info overlay
	showInfo bool

	// Derived accounts panel (inside info overlay)
	derivedKeyInput         string                  // hex key input buffer
	derivedKeyInputActive   bool                    // input field focused
	derivedAccounts         []DerivedAccountBalance // cached scan results
	derivedAccountsScroll   int                     // scroll offset for results list
	derivedAccountsLoading  bool                    // scanning in progress
	derivedAccountsLastScan time.Time               // last scan time for periodic refresh

	// Formula overlay (shows calculation for the selected chart)
	showFormula bool

	// Expanded action log overlay
	showLogOverlay   bool
	logOverlayOffset int // 0 = pinned to bottom (most recent)

	// Quit-with-save prompt
	quitPromptActive bool   // whether the "save CSV?" prompt is showing
	saveCSV          bool   // set to true if user confirmed save
	statsPath        string // permanent CSV path (for display in prompt)

	// Whether the initial auto-zoom to fit historical data has been applied.
	initialZoomApplied bool

	// Log scroll state (0 = pinned to bottom / most recent)
	logScrollOffset int

	// Panel visibility (toggled at runtime via 'p' overlay)
	chartVisible      [numCharts]bool
	showPanelConfig   bool
	panelConfigCursor int

	// Collector references (set after Init)
	collector  *Collector
	trafficSim *TrafficSimulator
	l1Sender   *L1Sender
}

// NewModel creates a new TUI Model.
func NewModel(ctx context.Context, cfg *Config, metrics *MetricsStore, statsPath string) Model {
	numAccounts := cfg.TrafficAccounts
	if numAccounts <= 0 {
		numAccounts = 1
	}
	m := Model{
		ctx:             ctx,
		cfg:             cfg,
		metrics:         metrics,
		width:           80,
		height:          40,
		pollInterval:    cfg.PollInterval,
		trafficRate:     cfg.TrafficRate,
		trafficAccounts: numAccounts,
		statsPath:       statsPath,
	}
	for i := range m.chartScales {
		m.chartScales[i] = chartScale{yZoom: 1.0, timeWin: defaultTimeWindow}
	}
	for i := range m.chartVisible {
		// Original 7 charts + on-chain panels visible by default; Prometheus
		// charts hidden unless metrics endpoints are configured.
		m.chartVisible[i] = i < 7
	}
	// On-chain derived panels (always available)
	m.chartVisible[12] = true // Posting Frequency
	m.chartVisible[13] = true // Safe Head Lag
	m.chartVisible[14] = true // TxPool Status
	m.chartVisible[15] = true // L1 Tx Count
	m.chartVisible[16] = true // L2 Blocks per Batch

	if cfg.NodeMetricsURL != "" {
		m.chartVisible[7] = true  // Derivation Health
		m.chartVisible[11] = true // Proposer Progress (if proposer URL set too)
	}
	if cfg.BatcherMetricsURL != "" {
		m.chartVisible[8] = true // Batcher Pipeline
		m.chartVisible[9] = true // L1 Fee Pressure
	}
	if cfg.ProposerMetricsURL != "" {
		m.chartVisible[11] = true // Proposer Progress
	}
	if cfg.NodePprofURL != "" || cfg.BatcherPprofURL != "" {
		m.chartVisible[10] = true // Process Health
	}
	// Set initial TPS window duration from config
	if cfg.TPSWindowDuration > 0 {
		metrics.SetTPSWindowDuration(cfg.TPSWindowDuration)
	}
	m.initCharts(80, 40)
	return m
}

// SetCollector sets the collector for the TUI.
func (m *Model) SetCollector(c *Collector) {
	m.collector = c
}

// SetTrafficSimulator sets the traffic simulator for the TUI.
func (m *Model) SetTrafficSimulator(ts *TrafficSimulator) {
	m.trafficSim = ts
}

// SetL1Sender sets the L1 sender for the TUI.
func (m *Model) SetL1Sender(s *L1Sender) {
	m.l1Sender = s
}

// SaveCSV returns whether the user chose to save the CSV on quit.
func (m Model) SaveCSV() bool {
	return m.saveCSV
}

// showPrompt activates the prompt overlay for a missing config value.
// label is the display text (e.g. "USDRIF L1 Token Address"), field is the
// TOML key (e.g. "usdrif_token_l1"), and retryKey is the original key press
// (e.g. "u") to re-process after the value has been saved.
func (m *Model) showPrompt(label, field, retryKey string) {
	ti := textinput.New()
	ti.Placeholder = "Paste or type value..."
	ti.Focus()
	ti.CharLimit = 256
	ti.Width = 50

	m.promptActive = true
	m.promptLabel = label
	m.promptField = field
	m.promptInput = ti
	m.promptRetryKey = retryKey
}

// adaptiveYFormatter formats Y-axis labels with precision adapted to the value's
// magnitude. Small values (< 1) get decimal places; large values are integers.
func adaptiveYFormatter(i int, v float64) string {
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

// unitYFormatter returns a Y-axis formatter that appends a unit suffix.
func unitYFormatter(unit string) func(int, float64) string {
	return func(i int, v float64) string {
		return adaptiveYFormatter(i, v) + " " + unit
	}
}

// normalizedYFormatter formats Y-axis labels as percentages (0–100%).
func normalizedYFormatter(_ int, v float64) string {
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

func (m *Model) initCharts(w, h int) {
	cw, ch := m.chartSize(w, h)

	now := time.Now()
	baseOpts := func() []timeserieslinechart.Option {
		return []timeserieslinechart.Option{
			timeserieslinechart.WithXLabelFormatter(timeserieslinechart.HourTimeLabelFormatter()),
			timeserieslinechart.WithTimeRange(now.Add(-5*time.Minute), now),
			timeserieslinechart.WithXYSteps(3, 2),
		}
	}

	chartOpts := func(unit string) []timeserieslinechart.Option {
		o := baseOpts()
		if unit != "" {
			o = append(o, timeserieslinechart.WithYLabelFormatter(unitYFormatter(unit)))
		} else {
			o = append(o, timeserieslinechart.WithYLabelFormatter(adaptiveYFormatter))
		}
		return o
	}

	m.chartTPS = timeserieslinechart.New(cw, ch, chartOpts("tx/s")...)
	m.chartL2TxCost = timeserieslinechart.New(cw, ch, chartOpts("RBTC")...)
	m.chartL2TxSpeed = timeserieslinechart.New(cw, ch, chartOpts("s")...)
	m.chartL2TxCount = timeserieslinechart.New(cw, ch, chartOpts("tx")...)
	m.chartBatcherPostCost = timeserieslinechart.New(cw, ch, chartOpts("RBTC")...)
	m.chartBatcherData = timeserieslinechart.New(cw, ch, chartOpts("B")...)
	m.chartDeposits = timeserieslinechart.New(cw, ch, chartOpts("")...)
	m.chartDeposits.SetDataSetStyle("deposits", depositColor)
	m.chartDeposits.SetDataSetStyle("withdrawals", withdrawColor)

	// Prometheus-sourced charts
	m.chartDerivHealth = timeserieslinechart.New(cw, ch, chartOpts("")...)
	m.chartDerivHealth.SetDataSetStyle("errors", lipgloss.NewStyle().Foreground(lipgloss.Color("196")))
	m.chartDerivHealth.SetDataSetStyle("resets", lipgloss.NewStyle().Foreground(lipgloss.Color("214")))
	m.chartBatcherPipeline = timeserieslinechart.New(cw, ch, chartOpts("")...)
	m.chartBatcherPipeline.SetDataSetStyle("blocks", lipgloss.NewStyle().Foreground(lipgloss.Color("75")))
	m.chartBatcherPipeline.SetDataSetStyle("bytes", lipgloss.NewStyle().Foreground(lipgloss.Color("215")))
	m.chartL1FeePressure = timeserieslinechart.New(cw, ch, chartOpts("gwei")...)
	m.chartL1FeePressure.SetDataSetStyle("basefee", lipgloss.NewStyle().Foreground(lipgloss.Color("220")))
	m.chartL1FeePressure.SetDataSetStyle("bumps", lipgloss.NewStyle().Foreground(lipgloss.Color("196")))
	m.chartProcessHealth = timeserieslinechart.New(cw, ch, chartOpts("MB")...)
	m.chartProcessHealth.SetDataSetStyle("node_heap", lipgloss.NewStyle().Foreground(lipgloss.Color("75")))
	m.chartProcessHealth.SetDataSetStyle("batcher_heap", lipgloss.NewStyle().Foreground(lipgloss.Color("215")))
	m.chartProposerSeqNum = timeserieslinechart.New(cw, ch, chartOpts("")...)

	// On-chain derived charts
	m.chartPostingFreq = timeserieslinechart.New(cw, ch, chartOpts("s")...)
	m.chartSafeHeadLag = timeserieslinechart.New(cw, ch, chartOpts("blk")...)
	m.chartTxPoolStatus = timeserieslinechart.New(cw, ch, chartOpts("")...)
	m.chartTxPoolStatus.SetDataSetStyle("pending", lipgloss.NewStyle().Foreground(lipgloss.Color("75")))
	m.chartTxPoolStatus.SetDataSetStyle("queued", lipgloss.NewStyle().Foreground(lipgloss.Color("215")))
	m.chartL1TxCount = timeserieslinechart.New(cw, ch, chartOpts("tx")...)
	m.chartL2BlocksPerBatch = timeserieslinechart.New(cw, ch, chartOpts("")...)
	m.chartL2BlocksPerBatch.SetDataSetStyle("l2_blocks", lipgloss.NewStyle().Foreground(lipgloss.Color("82")))
	m.chartL2BlocksPerBatch.SetDataSetStyle("l1_txs", lipgloss.NewStyle().Foreground(lipgloss.Color("214")))
}

// visibleChartRows returns the number of visual rows the visible charts occupy.
// Charts 0-5 are "regular" (rendered in 2-column pairs); chart 6 (deposits) is full-width.
func (m *Model) visibleChartRows() int {
	var regular int
	fullWidth := 0
	for i := 0; i < numCharts; i++ {
		if !m.chartVisible[i] {
			continue
		}
		if i == 6 {
			fullWidth++
		} else {
			regular++
		}
	}
	pairedRows := (regular + 1) / 2
	rows := pairedRows + fullWidth
	if rows < 1 {
		rows = 1
	}
	return rows
}

// chartSize calculates the width and height for each chart panel.
// pendingEventLines is the number of extra lines consumed by the pending events
// panel (0 when no events are active).
func (m *Model) chartSize(termW, termH int, pendingEventLines ...int) (int, int) {
	cols := 2
	chartRows := m.visibleChartRows()

	// Each chart panel rendered width = Width(cw+2) + 2 (border) = cw+4.
	// Two side-by-side panels must fit in termW: 2*(cw+4) <= termW.
	chartW := (termW - 8) / cols
	if chartW < 20 {
		chartW = 20
	}

	// Precise height budget:
	//   header:    4 lines (block info + balances/pending + process status + costs/maxTPS)
	//   per chart row: chartH + 3 (2 border + 1 title)
	//   log panel: logLines + 3 (2 border + 1 title)
	//   pending events panel: eventLines (0 when empty, else N events + 3 border/title)
	//   footer:    1 line (or 2 when suggestions shown)
	// Total = 4 + chartRows*(chartH+3) + (logLines+3) + eventLines + 1
	//       = chartRows*chartH + chartRows*3 + logLines + eventLines + 8
	// Solve: chartH = (termH - chartRows*3 - logLines - eventLines - 8) / chartRows
	evtLines := 0
	if len(pendingEventLines) > 0 {
		evtLines = pendingEventLines[0]
	}
	overhead := chartRows*3 + logLines + evtLines + 8
	chartH := (termH - overhead) / chartRows
	if chartH < 3 {
		chartH = 3
	}
	return chartW, chartH
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.Tick(m.pollInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		cw, ch := m.chartSize(msg.Width, msg.Height)
		for i := 0; i < numCharts; i++ {
			m.chartByIndex(i).Resize(cw, ch)
		}
		return m, nil

	case tea.KeyMsg:
		// When prompt overlay is visible, route keys to the text input.
		if m.promptActive {
			switch msg.Type {
			case tea.KeyEnter:
				value := strings.TrimSpace(m.promptInput.Value())
				if value != "" {
					// Save to runtime config
					m.cfg.SetConfigValue(m.promptField, value)
					// Persist to TOML file
					if err := SaveConfigField(m.cfg.ConfigPath, m.promptField, value); err != nil {
						m.metrics.AppendLog("[config] save error: %v", err)
					} else {
						m.metrics.AppendLog("[config] saved %s to config", m.promptField)
					}
					retryKey := m.promptRetryKey
					field := m.promptField
					m.promptActive = false
					m.promptRetryKey = ""

					// If we just got a private key and have no L1Sender, create one async.
					if field == "private_key" && m.l1Sender == nil {
						ctx := m.ctx
						cfg := m.cfg
						metrics := m.metrics
						m.metrics.AppendLog("[config] creating L1 sender...")
						return m, func() tea.Msg {
							sender, err := NewL1Sender(ctx, cfg, metrics)
							return l1SenderCreatedMsg{sender: sender, err: err, retryKey: retryKey}
						}
					}
					// Otherwise, retry the original action immediately.
					if retryKey != "" {
						return m, func() tea.Msg {
							return retryActionMsg{key: retryKey}
						}
					}
				}
				return m, nil
			case tea.KeyEsc:
				m.promptActive = false
				m.promptRetryKey = ""
				m.metrics.AppendLog("[config] prompt cancelled")
				return m, nil
			default:
				if msg.String() == "ctrl+c" {
					return m, tea.Quit
				}
				var cmd tea.Cmd
				m.promptInput, cmd = m.promptInput.Update(msg)
				return m, cmd
			}
		}

		// When command mode is active, route keys to the command input.
		if m.cmdMode {
			switch msg.Type {
			case tea.KeyEnter:
				input := m.cmdInput.Value()
				m.cmdMode = false
				m.cmdSuggestions = nil
				m.cmdSuggIdx = -1
				return m.executeCommand(input)
			case tea.KeyEsc:
				m.cmdMode = false
				m.cmdSuggestions = nil
				m.cmdSuggIdx = -1
				return m, nil
			case tea.KeyTab:
				if len(m.cmdSuggestions) > 0 {
					m.cmdSuggIdx = (m.cmdSuggIdx + 1) % len(m.cmdSuggestions)
					m.cmdInput.SetValue(m.cmdSuggestions[m.cmdSuggIdx].Cmd)
					m.cmdInput.CursorEnd()
				}
				return m, nil
			case tea.KeyShiftTab:
				if len(m.cmdSuggestions) > 0 {
					m.cmdSuggIdx = (m.cmdSuggIdx - 1 + len(m.cmdSuggestions)) % len(m.cmdSuggestions)
					m.cmdInput.SetValue(m.cmdSuggestions[m.cmdSuggIdx].Cmd)
					m.cmdInput.CursorEnd()
				}
				return m, nil
			default:
				if msg.String() == "ctrl+c" {
					return m, tea.Quit
				}
				var cmd tea.Cmd
				m.cmdInput, cmd = m.cmdInput.Update(msg)
				// Recompute suggestions after each keystroke
				m.cmdSuggestions = filterCmdSuggestions(m.cmdInput.Value())
				m.cmdSuggIdx = -1
				return m, cmd
			}
		}

		// When quit prompt is visible, handle y/n/esc/enter.
		if m.quitPromptActive {
			switch msg.String() {
			case "y", "enter":
				m.saveCSV = true
				return m, tea.Quit
			case "n", "esc":
				m.saveCSV = false
				return m, tea.Quit
			case "ctrl+c":
				return m, tea.Quit
			default:
				return m, nil
			}
		}

		// When help overlay is visible, only allow dismissing it or quitting.
		if m.showHelp {
			switch msg.String() {
			case "?", "esc":
				m.showHelp = false
				return m, nil
			case "ctrl+c":
				return m, tea.Quit
			default:
				return m, nil
			}
		}

		// When info overlay is visible, handle derived accounts input and navigation.
		if m.showInfo {
			key := msg.String()
			switch {
			case key == "ctrl+c":
				return m, tea.Quit
			case key == "tab":
				m.derivedKeyInputActive = !m.derivedKeyInputActive
				return m, nil
			case m.derivedKeyInputActive:
				switch key {
				case "enter":
					if m.derivedKeyInput != "" {
						m.derivedAccountsLoading = true
						m.derivedKeyInputActive = false
						// If showing "(N configured keys)", scan all; otherwise scan the entered key
						if strings.HasPrefix(m.derivedKeyInput, "(") && strings.HasSuffix(m.derivedKeyInput, "configured keys)") {
							return m, m.scanDerivedAccountsCmd("") // Empty = scan all
						}
						return m, m.scanDerivedAccountsCmd(m.derivedKeyInput)
					}
					return m, nil
				case "esc":
					m.derivedKeyInput = ""
					m.derivedKeyInputActive = false
					return m, nil
				case "backspace":
					if len(m.derivedKeyInput) > 0 {
						m.derivedKeyInput = m.derivedKeyInput[:len(m.derivedKeyInput)-1]
					}
					return m, nil
				default:
					// Accept pasted content: filter to hex characters only
					for _, c := range key {
						if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') || c == 'x' {
							m.derivedKeyInput += string(c)
						}
					}
					return m, nil
				}
			case key == "j", key == "down":
				maxScroll := len(m.derivedAccounts) - 8
				if maxScroll < 0 {
					maxScroll = 0
				}
				if m.derivedAccountsScroll < maxScroll {
					m.derivedAccountsScroll++
				}
				return m, nil
			case key == "k", key == "up":
				if m.derivedAccountsScroll > 0 {
					m.derivedAccountsScroll--
				}
				return m, nil
			case key == "esc":
				m.showInfo = false
				m.derivedKeyInput = ""
				m.derivedKeyInputActive = false
				m.derivedAccounts = nil
				m.derivedAccountsScroll = 0
				return m, nil
			default:
				m.showInfo = false
				m.derivedKeyInput = ""
				m.derivedKeyInputActive = false
				m.derivedAccounts = nil
				m.derivedAccountsScroll = 0
				return m, nil
			}
		}

		// When formula overlay is visible, dismiss on any key.
		if m.showFormula {
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			default:
				m.showFormula = false
				return m, nil
			}
		}

		// When panel config overlay is visible, handle navigation and toggling.
		if m.showPanelConfig {
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "p", "esc":
				m.showPanelConfig = false
				return m, nil
			case "j", "down":
				m.panelConfigCursor = (m.panelConfigCursor + 1) % numCharts
				return m, nil
			case "k", "up":
				m.panelConfigCursor = (m.panelConfigCursor - 1 + numCharts) % numCharts
				return m, nil
			case " ", "enter":
				visCount := 0
				for _, v := range m.chartVisible {
					if v {
						visCount++
					}
				}
				if m.chartVisible[m.panelConfigCursor] && visCount <= 1 {
					// Can't hide the last visible panel.
				} else {
					m.chartVisible[m.panelConfigCursor] = !m.chartVisible[m.panelConfigCursor]
					// Ensure selectedChart points to a visible chart.
					if !m.chartVisible[m.selectedChart] {
						m.selectedChart = m.nextVisibleChart(m.selectedChart)
					}
				}
				return m, nil
			default:
				return m, nil
			}
		}

		// When log overlay is visible, handle scrolling and dismiss.
		if m.showLogOverlay {
			snap := m.metrics.GetSnapshot()
			visibleLines := m.logOverlayVisibleLines()
			maxOffset := len(snap.Log) - visibleLines
			if maxOffset < 0 {
				maxOffset = 0
			}
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "l", "esc":
				m.showLogOverlay = false
				return m, nil
			case "up", "k":
				m.logOverlayOffset++
				if m.logOverlayOffset > maxOffset {
					m.logOverlayOffset = maxOffset
				}
				return m, nil
			case "down", "j":
				m.logOverlayOffset--
				if m.logOverlayOffset < 0 {
					m.logOverlayOffset = 0
				}
				return m, nil
			case "pgup":
				m.logOverlayOffset += visibleLines
				if m.logOverlayOffset > maxOffset {
					m.logOverlayOffset = maxOffset
				}
				return m, nil
			case "pgdown":
				m.logOverlayOffset -= visibleLines
				if m.logOverlayOffset < 0 {
					m.logOverlayOffset = 0
				}
				return m, nil
			case "home", "g":
				m.logOverlayOffset = maxOffset
				return m, nil
			case "end", "G":
				m.logOverlayOffset = 0
				return m, nil
			default:
				return m, nil
			}
		}

		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "?":
			m.showHelp = true
			return m, nil
		case "f":
			m.showFormula = true
			return m, nil
		case "a":
			if isMultiDatasetChart(m.selectedChart) {
				m.chartScales[m.selectedChart].normalized = !m.chartScales[m.selectedChart].normalized
			}
			return m, nil
		case "l":
			m.showLogOverlay = true
			m.logOverlayOffset = 0
			return m, nil
		case "p":
			m.showPanelConfig = true
			return m, nil
		case ":":
			ti := textinput.New()
			ti.Focus()
			ti.CharLimit = 256
			ti.Width = m.width - 4
			m.cmdMode = true
			m.cmdInput = ti
			m.cmdSuggestions = filterCmdSuggestions("")
			m.cmdSuggIdx = -1
			return m, nil

		// Chart selection (skip hidden charts)
		case "tab":
			m.selectedChart = m.nextVisibleChart(m.selectedChart)
			return m, nil
		case "shift+tab":
			m.selectedChart = m.prevVisibleChart(m.selectedChart)
			return m, nil

		// Y-axis zoom on selected chart
		case "up":
			s := &m.chartScales[m.selectedChart]
			s.yZoom *= 1.5
			if s.yZoom > 10.0 {
				s.yZoom = 10.0
			}
			return m, nil
		case "down":
			s := &m.chartScales[m.selectedChart]
			s.yZoom /= 1.5
			if s.yZoom < 0.25 {
				s.yZoom = 0.25
			}
			return m, nil

		// Time window zoom on selected chart
		case "left":
			s := &m.chartScales[m.selectedChart]
			s.timeWin /= 2
			if s.timeWin < 30*time.Second {
				s.timeWin = 30 * time.Second
			}
			return m, nil
		case "right":
			s := &m.chartScales[m.selectedChart]
			s.timeWin *= 2
			if s.timeWin > 24*time.Hour {
				s.timeWin = 24 * time.Hour
			}
			return m, nil

		// Action log scrolling
		case "pgup":
			snap := m.metrics.GetSnapshot()
			maxOffset := len(snap.Log) - logLines
			if maxOffset < 0 {
				maxOffset = 0
			}
			m.logScrollOffset += logLines
			if m.logScrollOffset > maxOffset {
				m.logScrollOffset = maxOffset
			}
			return m, nil
		case "pgdown":
			m.logScrollOffset -= logLines
			if m.logScrollOffset < 0 {
				m.logScrollOffset = 0
			}
			return m, nil

		// TPS window duration adjustment
		case "w":
			d := m.metrics.GetTPSWindowDuration()
			d -= 5 * time.Second
			if d < 5*time.Second {
				d = 5 * time.Second
			}
			m.metrics.SetTPSWindowDuration(d)
			m.metrics.AppendLog("[tps] window duration -> %s", d)
			return m, nil
		case "W":
			d := m.metrics.GetTPSWindowDuration()
			d += 5 * time.Second
			if d > 300*time.Second {
				d = 300 * time.Second
			}
			m.metrics.SetTPSWindowDuration(d)
			m.metrics.AppendLog("[tps] window duration -> %s", d)
			return m, nil
		}

	case l1SenderCreatedMsg:
		if msg.err != nil {
			m.metrics.AppendLog("[config] L1 sender creation failed: %v", msg.err)
		} else {
			m.l1Sender = msg.sender
			m.metrics.AppendLog("[config] L1 sender ready")
		}
		if msg.retryKey != "" && msg.err == nil {
			return m, func() tea.Msg {
				return retryActionMsg{key: msg.retryKey}
			}
		}
		return m, nil

	case retryActionMsg:
		return m.executeCommand(msg.key)

	case derivedAccountsScanMsg:
		m.derivedAccountsLoading = false
		if msg.err != nil {
			m.metrics.AppendLog("[info] derived accounts scan failed: %v", msg.err)
		} else {
			m.derivedAccounts = msg.accounts
			m.derivedAccountsScroll = 0
		}
		return m, nil

	case tickMsg:
		m.refreshCharts()
		// Poll traffic simulator for gas-pause state.
		if m.trafficSim != nil {
			m.trafficGasPaused = m.trafficSim.IsPausedForGas()
		}
		// Auto-expire completed/failed events older than 60s.
		m.metrics.ExpireOldEvents(60 * time.Second)

		// Periodic rescan of derived accounts while info overlay is visible
		var scanCmd tea.Cmd
		if m.showInfo && !m.derivedAccountsLoading && time.Since(m.derivedAccountsLastScan) > 30*time.Second {
			allKeys := len(m.cfg.ScanKeys)
			if m.cfg.PrivateKey != "" {
				allKeys++
			}
			if allKeys > 0 {
				m.derivedAccountsLoading = true
				m.derivedAccountsLastScan = time.Now()
				scanCmd = m.scanDerivedAccountsCmd("")
			}
		}

		tickCmd := tea.Tick(m.pollInterval, func(t time.Time) tea.Msg {
			return tickMsg(t)
		})
		if scanCmd != nil {
			return m, tea.Batch(tickCmd, scanCmd)
		}
		return m, tickCmd
	}

	return m, nil
}

// executeCommand parses and dispatches a vim-style command string.
// It is called from the command bar (Enter) and from retryActionMsg after a
// prompt or async operation completes.
func (m Model) executeCommand(input string) (tea.Model, tea.Cmd) {
	parts := strings.Fields(strings.TrimSpace(input))
	if len(parts) == 0 {
		return m, nil
	}
	verb := strings.ToLower(parts[0])
	args := parts[1:]

	switch verb {

	// --- Bridge commands ---

	case "deposit":
		if len(args) == 0 {
			m.metrics.AppendLog("[cmd] usage: deposit rbtc|usdrif [amount]")
			return m, nil
		}
		amountStr := ""
		if len(args) >= 2 {
			amountStr = args[1]
		}
		switch strings.ToLower(args[0]) {
		case "rbtc":
			return m.cmdDepositRBTC(input, amountStr)
		case "usdrif":
			return m.cmdDepositUSDRIF(input, amountStr)
		default:
			m.metrics.AppendLog("[cmd] unknown asset %q — use rbtc or usdrif", args[0])
			return m, nil
		}

	case "withdraw":
		if len(args) == 0 {
			m.metrics.AppendLog("[cmd] usage: withdraw rbtc|usdrif [amount]")
			return m, nil
		}
		amountStr := ""
		if len(args) >= 2 {
			amountStr = args[1]
		}
		switch strings.ToLower(args[0]) {
		case "rbtc":
			return m.cmdWithdrawRBTC(input, amountStr)
		case "usdrif":
			return m.cmdWithdrawUSDRIF(input, amountStr)
		default:
			m.metrics.AppendLog("[cmd] unknown asset %q — use rbtc or usdrif", args[0])
			return m, nil
		}

	// --- Traffic commands ---

	case "traffic":
		if len(args) == 0 {
			m.metrics.AppendLog("[cmd] usage: traffic on|off|toggle|rate <N>|accounts <N>")
			return m, nil
		}
		sub := strings.ToLower(args[0])
		switch sub {
		case "on":
			m.trafficOn = true
			if m.trafficSim != nil {
				m.trafficSim.SetNumAccounts(m.trafficAccounts)
				m.metrics.AppendLog("[cmd] starting traffic at %g tx/s, %d accounts", m.trafficRate, m.trafficAccounts)
				m.metrics.SetTrafficContext(m.trafficContextStr())
				m.trafficSim.Start(m.trafficRate)
			} else {
				m.metrics.AppendLog("[cmd] traffic simulator not available (no private key?)")
			}
			return m, nil
		case "off":
			m.trafficOn = false
			if m.trafficSim != nil {
				m.metrics.AppendLog("[cmd] stopping traffic")
				m.metrics.SetTrafficContext("traffic OFF")
				m.trafficSim.Stop()
			}
			return m, nil
		case "toggle":
			m.trafficOn = !m.trafficOn
			if m.trafficSim != nil {
				if m.trafficOn {
					m.trafficSim.SetNumAccounts(m.trafficAccounts)
					m.metrics.AppendLog("[cmd] starting traffic at %g tx/s, %d accounts", m.trafficRate, m.trafficAccounts)
					m.metrics.SetTrafficContext(m.trafficContextStr())
					m.trafficSim.Start(m.trafficRate)
				} else {
					m.metrics.AppendLog("[cmd] stopping traffic")
					m.metrics.SetTrafficContext("traffic OFF")
					m.trafficSim.Stop()
				}
			} else {
				m.metrics.AppendLog("[cmd] traffic simulator not available (no private key?)")
			}
			return m, nil
		case "rate":
			if len(args) < 2 {
				m.metrics.AppendLog("[cmd] usage: traffic rate <N>")
				return m, nil
			}
			rate, err := strconv.ParseFloat(args[1], 64)
			if err != nil {
				m.metrics.AppendLog("[cmd] invalid rate %q: %v", args[1], err)
				return m, nil
			}
			if rate < 0 {
				rate = 0
			}
			maxRate := m.maxTrafficRate()
			if rate > maxRate {
				rate = maxRate
			}
			m.trafficRate = rate
			if m.trafficOn && m.trafficSim != nil {
				m.trafficSim.SetRate(m.trafficRate)
				m.metrics.SetTrafficContext(m.trafficContextStr())
			}
			m.metrics.AppendLog("[cmd] traffic rate -> %g tx/s", m.trafficRate)
			return m, nil
		case "accounts":
			if len(args) < 2 {
				m.metrics.AppendLog("[cmd] usage: traffic accounts <N>")
				return m, nil
			}
			n, err := strconv.Atoi(args[1])
			if err != nil {
				m.metrics.AppendLog("[cmd] invalid count %q: %v", args[1], err)
				return m, nil
			}
			if n < 1 {
				n = 1
			}
			m.trafficAccounts = n
			if m.trafficSim != nil {
				m.trafficSim.SetNumAccounts(m.trafficAccounts)
			}
			m.metrics.AppendLog("[cmd] traffic accounts -> %d", m.trafficAccounts)
			if m.trafficOn {
				m.metrics.SetTrafficContext(m.trafficContextStr())
			}
			return m, nil
		default:
			m.metrics.AppendLog("[cmd] unknown traffic subcommand %q", sub)
			return m, nil
		}

	// --- Gas commands ---

	case "gas":
		if len(args) == 0 || strings.ToLower(args[0]) != "bump" {
			m.metrics.AppendLog("[cmd] usage: gas bump")
			return m, nil
		}
		if m.trafficSim != nil {
			cur := m.trafficSim.GasMultiplier()
			next := cur * 1.5
			m.trafficSim.SetGasMultiplier(next)
			if m.trafficGasPaused {
				m.trafficSim.ResumeFromGasPause()
				m.trafficGasPaused = false
				m.metrics.AppendLog("[cmd] bumped gas multiplier to %.1fx, resuming traffic", next)
				m.metrics.SetTrafficContext(m.trafficContextStr())
			} else {
				m.metrics.AppendLog("[cmd] gas multiplier -> %.1fx", next)
			}
		} else {
			m.metrics.AppendLog("[cmd] traffic simulator not available (no private key?)")
		}
		return m, nil

	// --- Poll interval ---

	case "poll":
		if len(args) == 0 {
			m.metrics.AppendLog("[cmd] usage: poll <duration>  (e.g. 2s, 500ms)")
			return m, nil
		}
		d, err := time.ParseDuration(args[0])
		if err != nil {
			m.metrics.AppendLog("[cmd] invalid duration %q: %v", args[0], err)
			return m, nil
		}
		if d < 500*time.Millisecond {
			d = 500 * time.Millisecond
		}
		if d > 30*time.Second {
			d = 30 * time.Second
		}
		m.pollInterval = d
		m.metrics.AppendLog("[cmd] poll interval -> %s", m.pollInterval)
		return m, nil

	// --- Zoom ---

	case "zoom":
		if len(args) == 0 || strings.ToLower(args[0]) != "reset" {
			m.metrics.AppendLog("[cmd] usage: zoom reset")
			return m, nil
		}
		m.chartScales[m.selectedChart] = chartScale{yZoom: 1.0, timeWin: defaultTimeWindow}
		m.metrics.AppendLog("[cmd] reset zoom on chart %d", m.selectedChart)
		return m, nil

	// --- Config ---

	case "set":
		if len(args) < 2 {
			m.metrics.AppendLog("[cmd] usage: set <key> <value>")
			return m, nil
		}
		field := args[0]
		value := strings.Join(args[1:], " ")
		m.cfg.SetConfigValue(field, value)
		if err := SaveConfigField(m.cfg.ConfigPath, field, value); err != nil {
			m.metrics.AppendLog("[cmd] save error: %v", err)
		} else {
			m.metrics.AppendLog("[cmd] saved %s to config", field)
		}
		return m, nil

	// --- Info ---

	case "info":
		m.showInfo = true
		// Auto-populate with configured keys info and trigger scan
		allKeys := len(m.cfg.ScanKeys)
		if m.cfg.PrivateKey != "" {
			allKeys++
		}
		if allKeys > 0 {
			m.derivedKeyInput = fmt.Sprintf("(%d configured keys)", allKeys)
			// Auto-scan if not already loading
			if !m.derivedAccountsLoading {
				m.derivedAccountsLoading = true
				m.derivedAccountsLastScan = time.Now()
				return m, m.scanDerivedAccountsCmd("")
			}
		}
		return m, nil

	// --- Scan derived accounts ---

	case "scan":
		var keyToScan string
		if len(args) > 0 {
			// Specific key provided
			keyToScan = strings.TrimPrefix(args[0], "0x")
			m.derivedKeyInput = keyToScan
		} else {
			// Scan all configured keys
			allKeys := len(m.cfg.ScanKeys)
			if m.cfg.PrivateKey != "" {
				allKeys++
			}
			if allKeys == 0 {
				m.metrics.AppendLog("[cmd] no keys configured (set private_key or scan_keys)")
				return m, nil
			}
			m.derivedKeyInput = fmt.Sprintf("(%d configured keys)", allKeys)
			keyToScan = "" // Empty means scan all
		}
		m.derivedAccountsLoading = true
		m.showInfo = true
		return m, m.scanDerivedAccountsCmd(keyToScan)

	// --- Events ---

	case "events":
		if len(args) > 0 && strings.ToLower(args[0]) == "clear" {
			m.metrics.ClearCompletedEvents()
			m.metrics.AppendLog("[cmd] cleared completed/failed events")
			return m, nil
		}
		m.metrics.AppendLog("[cmd] usage: events clear")
		return m, nil

	// --- Help / Quit ---

	case "help":
		m.showHelp = true
		return m, nil

	case "quit":
		m.quitPromptActive = true
		return m, nil

	default:
		m.metrics.AppendLog("[cmd] unknown command: %s", verb)
		return m, nil
	}
}

// cmdDepositRBTC triggers an L1 RBTC deposit, prompting for config if needed.
func (m Model) cmdDepositRBTC(retryCmd, amount string) (tea.Model, tea.Cmd) {
	if m.l1Sender == nil {
		if m.cfg.PrivateKey == "" {
			m.showPrompt("Private Key (hex)", "private_key", retryCmd)
			return m, nil
		}
		m.metrics.AppendLog("[cmd] deposit not available (rollup config missing?)")
		return m, nil
	}
	if amount != "" {
		m.l1Sender.SetBridgeAmountRBTC(amount)
		m.metrics.AppendLog("[cmd] triggering L1 deposit (%s RBTC)...", amount)
	} else {
		m.metrics.AppendLog("[cmd] triggering L1 deposit...")
	}
	go m.l1Sender.Deposit()
	return m, nil
}

// cmdWithdrawRBTC triggers an L2 RBTC withdrawal, prompting for config if needed.
func (m Model) cmdWithdrawRBTC(retryCmd, amount string) (tea.Model, tea.Cmd) {
	if m.l1Sender == nil {
		if m.cfg.PrivateKey == "" {
			m.showPrompt("Private Key (hex)", "private_key", retryCmd)
			return m, nil
		}
		m.metrics.AppendLog("[cmd] withdrawal not available (rollup config missing?)")
		return m, nil
	}
	if amount != "" {
		m.l1Sender.SetBridgeAmountRBTC(amount)
		m.metrics.AppendLog("[cmd] triggering L2 withdrawal (%s RBTC)...", amount)
	} else {
		m.metrics.AppendLog("[cmd] triggering L2 withdrawal...")
	}
	go m.l1Sender.Withdraw()
	return m, nil
}

// cmdDepositUSDRIF triggers a USDRIF deposit, prompting for config if needed.
func (m Model) cmdDepositUSDRIF(retryCmd, amount string) (tea.Model, tea.Cmd) {
	if m.l1Sender == nil {
		if m.cfg.PrivateKey == "" {
			m.showPrompt("Private Key (hex)", "private_key", retryCmd)
			return m, nil
		}
		m.metrics.AppendLog("[cmd] USDRIF deposit not available (rollup config missing?)")
		return m, nil
	}
	if m.cfg.USDRIFTokenL1 == "" {
		m.showPrompt("USDRIF L1 Token Address", "usdrif_token_l1", retryCmd)
		return m, nil
	}
	if amount != "" {
		m.l1Sender.SetBridgeAmountUSDRIF(amount)
		m.metrics.AppendLog("[cmd] triggering USDRIF deposit (%s)...", amount)
	} else {
		m.metrics.AppendLog("[cmd] triggering USDRIF deposit...")
	}
	go m.l1Sender.DepositUSDRIF()
	return m, nil
}

// cmdWithdrawUSDRIF triggers a USDRIF withdrawal, prompting for config if needed.
func (m Model) cmdWithdrawUSDRIF(retryCmd, amount string) (tea.Model, tea.Cmd) {
	if m.l1Sender == nil {
		if m.cfg.PrivateKey == "" {
			m.showPrompt("Private Key (hex)", "private_key", retryCmd)
			return m, nil
		}
		m.metrics.AppendLog("[cmd] USDRIF withdrawal not available (rollup config missing?)")
		return m, nil
	}
	if m.cfg.USDRIFTokenL1 == "" {
		m.showPrompt("USDRIF L1 Token Address", "usdrif_token_l1", retryCmd)
		return m, nil
	}
	if amount != "" {
		m.l1Sender.SetBridgeAmountUSDRIF(amount)
		m.metrics.AppendLog("[cmd] triggering USDRIF withdrawal (%s)...", amount)
	} else {
		m.metrics.AppendLog("[cmd] triggering USDRIF withdrawal...")
	}
	go m.l1Sender.WithdrawUSDRIF()
	return m, nil
}

// chartByIndex returns a pointer to the chart model at the given index.
func (m *Model) chartByIndex(i int) *timeserieslinechart.Model {
	switch i {
	case 0:
		return &m.chartTPS
	case 1:
		return &m.chartL2TxCost
	case 2:
		return &m.chartL2TxSpeed
	case 3:
		return &m.chartL2TxCount
	case 4:
		return &m.chartBatcherPostCost
	case 5:
		return &m.chartBatcherData
	case 6:
		return &m.chartDeposits
	case 7:
		return &m.chartDerivHealth
	case 8:
		return &m.chartBatcherPipeline
	case 9:
		return &m.chartL1FeePressure
	case 10:
		return &m.chartProcessHealth
	case 11:
		return &m.chartProposerSeqNum
	case 12:
		return &m.chartPostingFreq
	case 13:
		return &m.chartSafeHeadLag
	case 14:
		return &m.chartTxPoolStatus
	case 15:
		return &m.chartL1TxCount
	case 16:
		return &m.chartL2BlocksPerBatch
	default:
		return &m.chartTPS
	}
}

// nextVisibleChart returns the next visible chart index after current, wrapping around.
func (m *Model) nextVisibleChart(current int) int {
	for i := 1; i <= numCharts; i++ {
		idx := (current + i) % numCharts
		if m.chartVisible[idx] {
			return idx
		}
	}
	return current
}

// prevVisibleChart returns the previous visible chart index before current, wrapping around.
func (m *Model) prevVisibleChart(current int) int {
	for i := 1; i <= numCharts; i++ {
		idx := (current - i + numCharts) % numCharts
		if m.chartVisible[idx] {
			return idx
		}
	}
	return current
}

// refreshCharts reads the latest snapshot from the metrics store and replaces
// all chart data. Each tick we clear and re-push to avoid duplicating data.
func (m *Model) refreshCharts() {
	snap := m.metrics.GetSnapshot()

	// Resize charts to account for pending events panel height.
	evtLines := 0
	if len(snap.PendingEvents) > 0 {
		evtLines = len(snap.PendingEvents) + 3
	}
	cw, ch := m.chartSize(m.width, m.height, evtLines)
	for i := 0; i < numCharts; i++ {
		m.chartByIndex(i).Resize(cw, ch)
	}

	// On the first tick with data, auto-widen the time window so historical
	// data (from CSV or chain backfill) is immediately visible.
	if !m.initialZoomApplied {
		m.autoFitTimeWindow(snap)
	}

	m.replaceSeries(&m.chartTPS, snap.L2ToL1Throughput)
	m.replaceSeries(&m.chartL2TxCost, snap.L2TxCost)
	m.replaceSeries(&m.chartL2TxSpeed, snap.L2TxSpeed)
	m.replaceSeries(&m.chartL2TxCount, snap.L2TxCount)
	m.replaceSeries(&m.chartBatcherPostCost, snap.BatcherPostCost)

	// For batcher data chart: prefer compression ratio if available, else data size
	var batcherData []TimeValue
	if len(snap.CompressionRatio) > 0 {
		batcherData = snap.CompressionRatio
	} else {
		batcherData = snap.BatcherDataSize
	}
	m.replaceSeries(&m.chartBatcherData, batcherData)

	// Helper: push a slice of TimeValue into a named dataset.
	pushDataSet := func(chart *timeserieslinechart.Model, name string, data []TimeValue) {
		for _, tv := range data {
			chart.PushDataSet(name, timeserieslinechart.TimePoint{Time: tv.Time, Value: tv.Value})
		}
	}

	// Deposits & Withdrawals (chart 6)
	deposits, withdrawals := snap.Deposits, snap.Withdrawals
	if m.chartScales[6].normalized {
		deposits = normalizeTimeSeries(deposits)
		withdrawals = normalizeTimeSeries(withdrawals)
	}
	m.chartDeposits.ClearAllData()
	pushDataSet(&m.chartDeposits, "deposits", deposits)
	pushDataSet(&m.chartDeposits, "withdrawals", withdrawals)

	// Derivation health (chart 7: errors + resets)
	derivErrors, derivResets := snap.DerivationErrors, snap.PipelineResets
	if m.chartScales[7].normalized {
		derivErrors = normalizeTimeSeries(derivErrors)
		derivResets = normalizeTimeSeries(derivResets)
	}
	m.chartDerivHealth.ClearAllData()
	pushDataSet(&m.chartDerivHealth, "errors", derivErrors)
	pushDataSet(&m.chartDerivHealth, "resets", derivResets)

	// Batcher pipeline (chart 8: blocks + bytes)
	pendingBlocks, pendingBytes := snap.BatcherPendingBlocks, snap.BatcherPendingBytes
	if m.chartScales[8].normalized {
		pendingBlocks = normalizeTimeSeries(pendingBlocks)
		pendingBytes = normalizeTimeSeries(pendingBytes)
	}
	m.chartBatcherPipeline.ClearAllData()
	pushDataSet(&m.chartBatcherPipeline, "blocks", pendingBlocks)
	pushDataSet(&m.chartBatcherPipeline, "bytes", pendingBytes)

	// L1 fee pressure (chart 9: base fee + gas bumps)
	baseFee, gasBumps := snap.BatcherL1BaseFee, snap.BatcherGasBumps
	if m.chartScales[9].normalized {
		baseFee = normalizeTimeSeries(baseFee)
		gasBumps = normalizeTimeSeries(gasBumps)
	}
	m.chartL1FeePressure.ClearAllData()
	pushDataSet(&m.chartL1FeePressure, "basefee", baseFee)
	pushDataSet(&m.chartL1FeePressure, "bumps", gasBumps)

	// Process health (chart 10: node heap + batcher heap)
	nodeHeap, batcherHeap := snap.NodeHeapMB, snap.BatcherHeapMB
	if m.chartScales[10].normalized {
		nodeHeap = normalizeTimeSeries(nodeHeap)
		batcherHeap = normalizeTimeSeries(batcherHeap)
	}
	m.chartProcessHealth.ClearAllData()
	pushDataSet(&m.chartProcessHealth, "node_heap", nodeHeap)
	pushDataSet(&m.chartProcessHealth, "batcher_heap", batcherHeap)

	// Proposer progress (single series)
	m.replaceSeries(&m.chartProposerSeqNum, snap.ProposedSeqNum)

	// Posting Frequency (chart 12, single series)
	m.replaceSeries(&m.chartPostingFreq, snap.PostingFreq)

	// Safe Head Lag (chart 13, single series)
	m.replaceSeries(&m.chartSafeHeadLag, snap.SafeHeadLag)

	// TxPool Status (chart 14: pending + queued)
	txpPending, txpQueued := snap.TxPoolPending, snap.TxPoolQueued
	if m.chartScales[14].normalized {
		txpPending = normalizeTimeSeries(txpPending)
		txpQueued = normalizeTimeSeries(txpQueued)
	}
	m.chartTxPoolStatus.ClearAllData()
	pushDataSet(&m.chartTxPoolStatus, "pending", txpPending)
	pushDataSet(&m.chartTxPoolStatus, "queued", txpQueued)

	// L1 Tx Count (chart 15, single series)
	m.replaceSeries(&m.chartL1TxCount, snap.L1TxCount)

	// L2 Blocks per Batch (chart 16: l2_blocks + l1_txs)
	batchL2Blocks, batchL1Txs := snap.L2BlocksPerBatch, snap.L1TxsPerBatch
	if m.chartScales[16].normalized {
		batchL2Blocks = normalizeTimeSeries(batchL2Blocks)
		batchL1Txs = normalizeTimeSeries(batchL1Txs)
	}
	m.chartL2BlocksPerBatch.ClearAllData()
	pushDataSet(&m.chartL2BlocksPerBatch, "l2_blocks", batchL2Blocks)
	pushDataSet(&m.chartL2BlocksPerBatch, "l1_txs", batchL1Txs)

	// Apply per-chart zoom (time window + Y-axis scaling).
	// Pass the raw data series so Y range is computed from actual values,
	// not the chart's internal state (which would cause a feedback loop).
	dataSeries := [][]TimeValue{
		snap.L2ToL1Throughput, snap.L2TxCost, snap.L2TxSpeed, snap.L2TxCount,
		snap.BatcherPostCost, batcherData,
		append(snap.Deposits, snap.Withdrawals...),
		append(snap.DerivationErrors, snap.PipelineResets...),
		append(snap.BatcherPendingBlocks, snap.BatcherPendingBytes...),
		append(snap.BatcherL1BaseFee, snap.BatcherGasBumps...),
		append(snap.NodeHeapMB, snap.BatcherHeapMB...),
		snap.ProposedSeqNum,
		snap.PostingFreq,
		snap.SafeHeadLag,
		append(snap.TxPoolPending, snap.TxPoolQueued...),
		snap.L1TxCount,
		append(snap.L2BlocksPerBatch, snap.L1TxsPerBatch...),
	}

	// Original Y-axis formatters by chart index (mirrors initCharts order).
	origFormatters := [numCharts]func(int, float64) string{
		unitYFormatter("tx/s"), unitYFormatter("RBTC"), unitYFormatter("s"), unitYFormatter("tx"),
		unitYFormatter("RBTC"), unitYFormatter("B"),
		adaptiveYFormatter, adaptiveYFormatter, adaptiveYFormatter,
		unitYFormatter("gwei"), adaptiveYFormatter, adaptiveYFormatter,
		unitYFormatter("s"), unitYFormatter("blk"), adaptiveYFormatter,
		unitYFormatter("tx"), adaptiveYFormatter,
	}

	for i := 0; i < numCharts; i++ {
		chart := m.chartByIndex(i)
		ds := dataSeries[i]
		if m.chartScales[i].normalized && isMultiDatasetChart(i) {
			ds = normalizeTimeSeries(ds)
			chart.YLabelFormatter = normalizedYFormatter
		} else {
			chart.YLabelFormatter = origFormatters[i]
		}
		m.applyChartZoom(chart, m.chartScales[i], ds)
	}
}

// autoFitTimeWindow widens all chart time windows on the first tick that
// contains data spanning more than the default 5-minute window. This ensures
// historical data loaded from CSV or chain backfill is visible immediately.
func (m *Model) autoFitTimeWindow(snap Snapshot) {
	// Collect all series to find the widest time span.
	allSeries := [][]TimeValue{
		snap.L2ToL1Throughput, snap.L2TxCost, snap.L2TxSpeed, snap.L2TxCount,
		snap.BatcherPostCost, snap.BatcherDataSize, snap.CompressionRatio,
		snap.Deposits, snap.Withdrawals,
		snap.PostingFreq, snap.SafeHeadLag, snap.L1TxCount,
		snap.TxPoolPending, snap.TxPoolQueued,
		snap.L2BlocksPerBatch, snap.L1TxsPerBatch,
	}

	var earliest, latest time.Time
	found := false
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

	if !found {
		return // no data yet — try again on the next tick
	}

	m.initialZoomApplied = true

	span := latest.Sub(earliest)
	if span <= defaultTimeWindow {
		return // data fits in the default window; no adjustment needed
	}

	// Add 10 % margin so the edges of the data aren't right on the border.
	fitWindow := span + span/10
	if fitWindow < defaultTimeWindow {
		fitWindow = defaultTimeWindow
	}
	for i := range m.chartScales {
		m.chartScales[i].timeWin = fitWindow
	}
}

// replaceSeries clears the chart's data and re-pushes from the snapshot.
func (m *Model) replaceSeries(chart *timeserieslinechart.Model, data []TimeValue) {
	chart.ClearAllData()
	for _, tv := range data {
		chart.Push(timeserieslinechart.TimePoint{
			Time:  tv.Time,
			Value: tv.Value,
		})
	}
}

// dataYRange computes the min and max Y values from a data series.
func dataYRange(data []TimeValue) (float64, float64) {
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

// applyChartZoom sets the time window and Y-axis range on a chart based on the
// per-chart zoom state. Uses the raw data series to compute Y range, avoiding
// feedback loops from the chart's internal range state.
func (m *Model) applyChartZoom(chart *timeserieslinechart.Model, scale chartScale, data []TimeValue) {
	now := time.Now()

	// Expected range must cover all data so SetViewTimeRange isn't clamped.
	// Use the oldest data point or the zoom window, whichever is wider.
	earliest := now.Add(-scale.timeWin)
	if len(data) > 0 && data[0].Time.Before(earliest) {
		earliest = data[0].Time
	}
	chart.SetTimeRange(earliest, now)
	// View range is the zoomed window (subset of the expected range).
	chart.SetViewTimeRange(now.Add(-scale.timeWin), now)

	// Always compute Y range from actual data so the chart fits the values.
	// Without this, the ntcharts auto-range (which only expands, never
	// contracts) leaves the initial [0,1] range in place and tiny values
	// like TPS ~0.1 or tx costs ~1e-8 are invisible at the bottom.
	minY, maxY := dataYRange(data)
	if len(data) == 0 {
		return
	}
	mid := (minY + maxY) / 2
	halfRange := (maxY - minY) / 2
	if halfRange < 1e-9 {
		halfRange = 1.0 // avoid zero range
	}
	adjustedHalf := halfRange / scale.yZoom
	lo := mid - adjustedHalf
	if lo < 0 {
		lo = 0 // all monitored metrics are non-negative
	}
	chart.SetYRange(lo, mid+adjustedHalf)
	chart.SetViewYRange(lo, mid+adjustedHalf)
}

// View implements tea.Model.
func (m Model) View() string {
	snap := m.metrics.GetSnapshot()

	// Account for pending events panel height in chart sizing.
	evtLines := 0
	if len(snap.PendingEvents) > 0 {
		evtLines = len(snap.PendingEvents) + 3 // events + border (2) + title (1)
	}
	cw, _ := m.chartSize(m.width, m.height, evtLines)

	// Header line 1: block info + counters
	l2Info := fmt.Sprintf("L2: #%d", snap.LatestL2Block)
	if snap.SafeL2 > 0 {
		lag := uint64(0)
		if snap.UnsafeL2 > snap.SafeL2 {
			lag = snap.UnsafeL2 - snap.SafeL2
		}
		if snap.SafeHeadVel > 0 {
			// sequencer block time is ~2s, so production rate is ~0.5 blocks/s
			const sequencerRate = 0.5
			netRate := snap.SafeHeadVel - sequencerRate
			if netRate > 0 {
				etaSecs := float64(lag) / netRate
				l2Info = fmt.Sprintf("L2: #%d (safe: #%d, lag: %d -, ETA: %s)", snap.LatestL2Block, snap.SafeL2, lag, fmtDuration(etaSecs))
			} else {
				l2Info = fmt.Sprintf("L2: #%d (safe: #%d, lag: %d +)", snap.LatestL2Block, snap.SafeL2, lag)
			}
		} else {
			l2Info = fmt.Sprintf("L2: #%d (safe: #%d, lag: %d)", snap.LatestL2Block, snap.SafeL2, lag)
		}
	}
	headerLine1 := titleStyle.Render(fmt.Sprintf(
		"Rollup Monitor  |  L1: #%d  %s  |  L2 Txs: %d  Dep: %d  Wdl: %d",
		snap.LatestL1Block, l2Info, snap.TotalL2Txs,
		snap.TotalDeposits, snap.TotalWithdrawals,
	))

	// Header line 2: balances, pending, batcher status
	balStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("114"))
	pendStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("215"))
	batcherStr := renderBatcherStatus(snap)
	headerLine2 := fmt.Sprintf(
		"%s  L1: %.4f  L2: %.4f RBTC   %s  L1: %d  L2: %d   %s",
		balStyle.Render("Bal"),
		snap.L1Balance, snap.L2Balance,
		pendStyle.Render("Pending"),
		snap.L1PendingTxs, snap.L2PendingTxs,
		batcherStr,
	)

	// Header line 3: process liveness + derivation state + L1 latency
	headerLine3 := renderProcessStatus(snap)

	// Header line 4: costs, theoretical max TPS, compression
	headerLine4 := renderCostLine(snap)

	header := headerLine1 + "\n" + headerLine2 + "\n" + headerLine3 + "\n" + headerLine4

	// Render each chart panel with current value, avg, peak, and zoom indicator
	normStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("82"))

	// chartTitleSuffix returns zoom and normalization indicators for the given chart.
	chartTitleSuffix := func(chartIdx int) string {
		var suffix string
		if m.chartScales[chartIdx].normalized {
			suffix += " " + normStyle.Render("[norm]")
		}
		if zi := zoomIndicator(m.chartScales[chartIdx]); zi != "" {
			suffix += " " + statusStyle.Render(zi)
		}
		return suffix
	}

	// renderTPSPanel renders the L2→L1 TPS panel showing throughput to L1 finality.
	renderTPSPanel := func(chartIdx int) string {
		m.chartTPS.DrawAll()
		last := lastValueStr(snap.L2ToL1Throughput, "%.2f")
		avgStr := avgStyle.Render(fmt.Sprintf("avg:%s", fmtSummaryVal(snap.SumL2ToL1Throughput.Avg, "%.2f")))
		pkStr := peakStyle.Render(fmt.Sprintf("peak:%s", fmtSummaryVal(snap.SumL2ToL1Throughput.Peak, "%.2f")))
		latencyLast := lastValueStr(snap.L2ToL1Latency, "%.1f")
		title := chartLabelStyle.Render(fmt.Sprintf(" L2→L1 TPS %s", last)) +
			" " + avgStr +
			" " + pkStr +
			" " + statusStyle.Render(fmt.Sprintf("lat:%ss", latencyLast)) +
			chartTitleSuffix(chartIdx)
		content := m.chartTPS.View()
		borderStyle := chartBorderStyle
		if chartIdx == m.selectedChart {
			borderStyle = selectedChartBorderStyle
		}
		return borderStyle.Width(cw + 2).Render(title + "\n" + content)
	}

	renderPanel := func(chartIdx int, chart *timeserieslinechart.Model, label string, data []TimeValue, sum SummarySnapshot, fmt0 string) string {
		chart.DrawAll()
		last := lastValueStr(data, fmt0)
		avgStr := avgStyle.Render(fmt.Sprintf("avg:%s", fmtSummaryVal(sum.Avg, fmt0)))
		pkStr := peakStyle.Render(fmt.Sprintf("peak:%s", fmtSummaryVal(sum.Peak, fmt0)))
		title := chartLabelStyle.Render(fmt.Sprintf(" %s %s", label, last)) + " " + avgStr + " " + pkStr + chartTitleSuffix(chartIdx)
		content := chart.View()
		borderStyle := chartBorderStyle
		if chartIdx == m.selectedChart {
			borderStyle = selectedChartBorderStyle
		}
		bordered := borderStyle.Width(cw + 2).Render(title + "\n" + content)
		return bordered
	}

	renderFullWidthPanel := func(chartIdx int, chart *timeserieslinechart.Model, label string, data []TimeValue, sum SummarySnapshot, fmt0 string) string {
		chart.DrawAll()
		last := lastValueStr(data, fmt0)
		avgStr := avgStyle.Render(fmt.Sprintf("avg:%s", fmtSummaryVal(sum.Avg, fmt0)))
		pkStr := peakStyle.Render(fmt.Sprintf("peak:%s", fmtSummaryVal(sum.Peak, fmt0)))
		title := chartLabelStyle.Render(fmt.Sprintf(" %s %s", label, last)) + " " + avgStr + " " + pkStr + chartTitleSuffix(chartIdx)
		content := chart.View()
		borderStyle := chartBorderStyle
		if chartIdx == m.selectedChart {
			borderStyle = selectedChartBorderStyle
		}
		return borderStyle.Width(cw*2 + 6).Render(title + "\n" + content)
	}

	// Prepare batcher data label/series/format (may vary based on available data).
	batcherDataLabel := labelBatcherData
	batcherDataSeries := snap.BatcherDataSize
	batcherDataSum := snap.SumBatcherDataSize
	batcherDataFmt := "%.0f"
	if len(snap.CompressionRatio) > 0 {
		batcherDataLabel = "Compression Ratio"
		batcherDataSeries = snap.CompressionRatio
		batcherDataSum = snap.SumCompressionRatio
		batcherDataFmt = "%.3f"
	}

	// panelSpec maps chart index -> render args.
	type panelSpec struct {
		chart *timeserieslinechart.Model
		label string
		data  []TimeValue
		sum   SummarySnapshot
		fmt0  string
	}
	// Empty summary for multi-dataset charts (summary not meaningful)
	noSum := SummarySnapshot{}

	specs := [numCharts]panelSpec{
		{&m.chartTPS, "L2→L1 TPS", snap.L2ToL1Throughput, snap.SumL2ToL1Throughput, "%.2f"},
		{&m.chartL2TxCost, labelL2TxCost, snap.L2TxCost, snap.SumL2Cost, "%.8f"},
		{&m.chartL2TxSpeed, labelL2TxSpeed, snap.L2TxSpeed, snap.SumL2Speed, "%.2f"},
		{&m.chartL2TxCount, labelL2TxCount, snap.L2TxCount, snap.SumL2Count, "%.0f"},
		{&m.chartBatcherPostCost, labelBatcherPostCost, snap.BatcherPostCost, snap.SumBatcherPostCost, "%.8f"},
		{&m.chartBatcherData, batcherDataLabel, batcherDataSeries, batcherDataSum, batcherDataFmt},
		{}, // deposits handled specially (chart 6)
		{&m.chartDerivHealth, labelDerivHealth, snap.DerivationErrors, noSum, "%.0f"},
		{&m.chartBatcherPipeline, labelBatcherPipeline, snap.BatcherPendingBlocks, noSum, "%.0f"},
		{&m.chartL1FeePressure, labelL1FeePressure, snap.BatcherL1BaseFee, noSum, "%.2f"},
		{&m.chartProcessHealth, labelProcessHealth, snap.NodeHeapMB, noSum, "%.1f"},
		{&m.chartProposerSeqNum, labelProposerSeqNum, snap.ProposedSeqNum, noSum, "%.0f"},
		{&m.chartPostingFreq, labelPostingFreq, snap.PostingFreq, snap.SumPostFreq, "%.1f"},
		{&m.chartSafeHeadLag, labelSafeHeadLag, snap.SafeHeadLag, snap.SumSafeHeadLag, "%.0f"},
		{&m.chartTxPoolStatus, labelTxPoolStatus, snap.TxPoolPending, noSum, "%.0f"},
		{&m.chartL1TxCount, labelL1TxCount, snap.L1TxCount, snap.SumL1Count, "%.0f"},
		{&m.chartL2BlocksPerBatch, labelL2BlocksPerBatch, snap.L2BlocksPerBatch, snap.SumL2BlocksPerBatch, "%.0f"},
	}

	// Build visible regular panels (all except chart 6 which is full-width).
	var regularVisible []int
	showDeposits := m.chartVisible[6]
	for i := 0; i < numCharts; i++ {
		if i == 6 {
			continue // deposits is full-width, handled separately
		}
		if m.chartVisible[i] {
			regularVisible = append(regularVisible, i)
		}
	}

	// Render Deposits & Withdrawals panel (chart 6, always full-width).
	renderDepositsPanel := func() string {
		m.chartDeposits.DrawAll()
		depLast := lastValueStr(snap.Deposits, "%.0f")
		wdLast := lastValueStr(snap.Withdrawals, "%.0f")
		depTitle := chartLabelStyle.Render(fmt.Sprintf(" %s  D:%s W:%s", labelDeposits, depLast, wdLast)) + chartTitleSuffix(6)
		depContent := m.chartDeposits.View()
		depBorder := chartBorderStyle
		if m.selectedChart == 6 {
			depBorder = selectedChartBorderStyle
		}
		return depBorder.Width(cw*2 + 6).Render(depTitle + "\n" + depContent)
	}

	// Action Log panel: show last N log entries
	logPanel := m.renderLogPanel(snap, cw*2+6)

	// Pending events panel (between log panel and footer)
	eventsPanel := m.renderPendingEventsPanel(snap, cw*2+6)

	// Footer: status bar or command input
	var footer string
	if m.cmdMode {
		cmdStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
		suggLine := m.renderCmdSuggestions()
		if suggLine != "" {
			footer = suggLine + "\n" + cmdStyle.Render(":") + m.cmdInput.View()
		} else {
			footer = cmdStyle.Render(":") + m.cmdInput.View()
		}
	} else {
		trafficStr := "OFF"
		if m.trafficGasPaused {
			trafficStr = "PAUSED (pool full) — :gas bump / :traffic off"
		} else if m.trafficOn && m.trafficSim != nil {
			target, ready := m.trafficSim.NumAccounts()
			trafficStr = fmt.Sprintf("ON (%g tx/s, %d/%d accts)", m.trafficRate, ready, target)
			if mul := m.trafficSim.GasMultiplier(); mul > 1.0 {
				trafficStr += fmt.Sprintf(" gas: %.1fx", mul)
			}
		} else if m.trafficOn {
			trafficStr = fmt.Sprintf("ON (%g tx/s, %d accts)", m.trafficRate, m.trafficAccounts)
		}
		footer = statusStyle.Render(fmt.Sprintf(
			"%s  |  Traffic: %s  |  Poll: %s  |  :: command  p: panels  f: formula  a: normalize  ?: help  ctrl+c: quit",
			snap.Status, trafficStr, m.pollInterval,
		))
	}

	var baseParts []string
	baseParts = append(baseParts, header)

	// renderPanelOrTPS renders the appropriate panel based on chart index.
	renderPanelOrTPS := func(idx int, s panelSpec) string {
		if idx == 0 { // TPS panel has special rendering
			return renderTPSPanel(idx)
		}
		return renderPanel(idx, s.chart, s.label, s.data, s.sum, s.fmt0)
	}

	// Render regular panels in 2-column rows.
	for i := 0; i < len(regularVisible); i += 2 {
		idx1 := regularVisible[i]
		s1 := specs[idx1]
		if i+1 < len(regularVisible) {
			idx2 := regularVisible[i+1]
			s2 := specs[idx2]
			row := lipgloss.JoinHorizontal(lipgloss.Top,
				renderPanelOrTPS(idx1, s1),
				renderPanelOrTPS(idx2, s2),
			)
			baseParts = append(baseParts, row)
		} else {
			if idx1 == 0 { // TPS panel as full width
				baseParts = append(baseParts, renderTPSPanel(idx1))
			} else {
				baseParts = append(baseParts, renderFullWidthPanel(idx1, s1.chart, s1.label, s1.data, s1.sum, s1.fmt0))
			}
		}
	}
	if showDeposits {
		baseParts = append(baseParts, renderDepositsPanel())
	}

	baseParts = append(baseParts, logPanel)
	if eventsPanel != "" {
		baseParts = append(baseParts, eventsPanel)
	}
	baseParts = append(baseParts, footer)
	base := lipgloss.JoinVertical(lipgloss.Left, baseParts...)

	if m.showPanelConfig {
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			m.renderPanelConfigOverlay(),
			lipgloss.WithWhitespaceChars(" "),
		)
	}

	if m.quitPromptActive {
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			m.renderQuitPromptOverlay(),
			lipgloss.WithWhitespaceChars(" "),
		)
	}

	if m.promptActive {
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			m.renderPromptOverlay(),
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

	if m.showInfo {
		return lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			m.renderInfoOverlay(snap),
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
			m.renderLogOverlay(snap),
			lipgloss.WithWhitespaceChars(" "),
		)
	}

	return base
}

// renderHelpOverlay returns a styled help box listing all commands and keybindings.
func (m Model) renderHelpOverlay() string {
	helpTitle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("205")).
		Align(lipgloss.Center)

	helpCmd := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("99")).
		Width(26)

	helpKey := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("99")).
		Width(14)

	helpDesc := lipgloss.NewStyle().
		Foreground(lipgloss.Color("252"))

	sectionTitle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("214"))

	cmd := func(command, desc string) string {
		return helpCmd.Render(command) + helpDesc.Render(desc)
	}

	key := func(k, desc string) string {
		return helpKey.Render(k) + helpDesc.Render(desc)
	}

	content := strings.Join([]string{
		helpTitle.Render("Rollup Monitor Help"),
		"",
		sectionTitle.Render("Commands (press : then type, Tab to complete)"),
		"",
		cmd(":deposit rbtc [amount]", "Deposit RBTC (default: config)"),
		cmd(":deposit usdrif [amount]", "Deposit USDRIF (default: config)"),
		cmd(":withdraw rbtc [amount]", "Withdraw RBTC (default: config)"),
		cmd(":withdraw usdrif [amount]", "Withdraw USDRIF (default: config)"),
		"",
		cmd(":traffic on|off|toggle", "Control traffic simulator"),
		cmd(":traffic rate <N>", "Set traffic rate (tx/s)"),
		cmd(":traffic accounts <N>", "Set sender account count"),
		cmd(":gas bump", "Bump gas multiplier (1.5x)"),
		"",
		cmd(":poll <duration>", "Set poll interval (e.g. 2s)"),
		cmd(":zoom reset", "Reset zoom on selected chart"),
		cmd(":set <key> <value>", "Set a config value"),
		cmd(":info", "Show connection details & balances"),
		cmd(":events clear", "Clear completed/failed events"),
		cmd(":help", "Show this help"),
		cmd(":quit", "Quit"),
		"",
		sectionTitle.Render("Keybindings"),
		"",
		key(":", "Enter command mode"),
		key("?", "Toggle this help"),
		key("p", "Show/hide panels"),
		key("f", "Show chart formula"),
		key("a", "Toggle normalized view (multi-dataset charts)"),
		key("l", "Expand action log"),
		key("Ctrl+C", "Quit"),
		key("Tab/S-Tab", "Select chart / cycle suggestions"),
		key("Up / Down", "Zoom Y-axis in / out"),
		key("Left/Right", "Zoom time window in / out"),
		key("PgUp/PgDn", "Scroll action log up / down"),
		key("w / W", "TPS window -5s / +5s"),
		"",
		statusStyle.Render("        Press ? or Esc to close"),
	}, "\n")

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Padding(1, 3).
		Render(content)

	return box
}

// renderPromptOverlay returns a styled prompt box with a text input for missing config values.
func (m Model) renderPromptOverlay() string {
	promptTitle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("205")).
		Align(lipgloss.Center)

	promptDesc := lipgloss.NewStyle().
		Foreground(lipgloss.Color("252"))

	hintStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241"))

	content := strings.Join([]string{
		promptTitle.Render("Missing Configuration"),
		"",
		promptDesc.Render(fmt.Sprintf("Please enter: %s", m.promptLabel)),
		promptDesc.Render(fmt.Sprintf("(config key: %s)", m.promptField)),
		"",
		m.promptInput.View(),
		"",
		hintStyle.Render("Enter: save  |  Esc: cancel"),
	}, "\n")

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Padding(1, 3).
		Render(content)

	return box
}

// renderCmdSuggestions returns a single line showing filtered command suggestions.
// The currently selected suggestion (if any) is highlighted.
func (m Model) renderCmdSuggestions() string {
	if len(m.cmdSuggestions) == 0 {
		return ""
	}
	normalStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	activeStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))

	var parts []string
	for i, entry := range m.cmdSuggestions {
		s := entry.Cmd
		if i == m.cmdSuggIdx {
			parts = append(parts, activeStyle.Render(s))
		} else {
			parts = append(parts, normalStyle.Render(s))
		}
	}
	line := strings.Join(parts, normalStyle.Render(" | "))
	// If exact match, append description
	input := strings.TrimSpace(m.cmdInput.Value())
	for _, entry := range m.cmdSuggestions {
		if entry.Cmd == input {
			line += "  " + lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render(entry.Desc)
			break
		}
	}
	return line
}

// renderInfoOverlay returns a styled popup showing connection details and balances.
func (m Model) renderInfoOverlay(snap Snapshot) string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Align(lipgloss.Center)
	labelStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99")).Width(22)
	valStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	row := func(label, value string) string {
		return labelStyle.Render(label) + valStyle.Render(value)
	}

	// Left column: existing connection info
	var leftLines []string

	// Balances
	leftLines = append(leftLines, sectionStyle.Render("Balances"))
	leftLines = append(leftLines, row("L1 RBTC:", fmt.Sprintf("%.6f", snap.L1Balance)))
	leftLines = append(leftLines, row("L2 RBTC:", fmt.Sprintf("%.6f", snap.L2Balance)))
	leftLines = append(leftLines, row("L1 USDRIF:", fmt.Sprintf("%.6f", snap.L1USDRIFBalance)))
	leftLines = append(leftLines, row("L2 USDRIF:", fmt.Sprintf("%.6f", snap.L2USDRIFBalance)))
	leftLines = append(leftLines, "")

	// RPCs
	leftLines = append(leftLines, sectionStyle.Render("RPCs"))
	leftLines = append(leftLines, row("L1 RPC:", redactURL(m.cfg.L1RPC)))
	leftLines = append(leftLines, row("L2 RPC:", redactURL(m.cfg.L2RPC)))
	leftLines = append(leftLines, row("op-node RPC:", redactURL(m.cfg.NodeRPC)))
	leftLines = append(leftLines, "")

	// Chain IDs
	leftLines = append(leftLines, sectionStyle.Render("Chain IDs"))
	if m.cfg.Rollup != nil {
		leftLines = append(leftLines, row("L1 Chain ID:", fmt.Sprintf("%d", m.cfg.Rollup.L1ChainID)))
		leftLines = append(leftLines, row("L2 Chain ID:", fmt.Sprintf("%d", m.cfg.Rollup.L2ChainID)))
	} else {
		leftLines = append(leftLines, row("L1 Chain ID:", "(unknown)"))
		leftLines = append(leftLines, row("L2 Chain ID:", "(unknown)"))
	}
	leftLines = append(leftLines, "")

	// Contracts
	leftLines = append(leftLines, sectionStyle.Render("Key Contracts"))
	if m.cfg.L1Addrs != nil {
		leftLines = append(leftLines, row("OptimismPortal:", m.cfg.L1Addrs.OptimismPortal().Hex()))
		leftLines = append(leftLines, row("L1StandardBridge:", m.cfg.L1Addrs.L1StandardBridge().Hex()))
		leftLines = append(leftLines, row("DisputeGameFactory:", m.cfg.L1Addrs.DisputeGameFactory().Hex()))
	}
	if m.cfg.Rollup != nil {
		leftLines = append(leftLines, row("BatchInbox:", m.cfg.Rollup.BatchInbox().Hex()))
		leftLines = append(leftLines, row("DepositContract:", m.cfg.Rollup.DepositContract().Hex()))
	}
	leftLines = append(leftLines, "")

	// Account
	leftLines = append(leftLines, sectionStyle.Render("Account"))
	if m.l1Sender != nil {
		leftLines = append(leftLines, row("EOA:", m.l1Sender.Address().Hex()))
	} else if m.cfg.PrivateKey != "" {
		leftLines = append(leftLines, row("EOA:", "(sender not initialized)"))
	} else {
		leftLines = append(leftLines, row("EOA:", "(no private key set)"))
	}
	leftLines = append(leftLines, "")

	// Config
	leftLines = append(leftLines, sectionStyle.Render("Config"))
	leftLines = append(leftLines, row("Workdir:", m.cfg.WorkDir))
	leftLines = append(leftLines, row("Bridge RBTC:", m.cfg.BridgeAmountRBTC))
	leftLines = append(leftLines, row("Bridge USDRIF:", m.cfg.BridgeAmountUSDRIF))
	if m.cfg.USDRIFTokenL1 != "" {
		leftLines = append(leftLines, row("USDRIF Token L1:", m.cfg.USDRIFTokenL1))
	}
	if m.cfg.USDRIFTokenL2 != "" {
		leftLines = append(leftLines, row("USDRIF Token L2:", m.cfg.USDRIFTokenL2))
	}
	leftLines = append(leftLines, row("Poll Interval:", m.pollInterval.String()))

	// Batcher
	if m.cfg.Rollup != nil {
		leftLines = append(leftLines, "")
		leftLines = append(leftLines, sectionStyle.Render("Batcher"))
		leftLines = append(leftLines, row("Batcher Address:", m.cfg.Rollup.BatcherAddress().Hex()))
	}

	leftContent := strings.Join(leftLines, "\n")
	leftColumn := lipgloss.NewStyle().Width(50).Render(leftContent)

	// Right column: derived accounts panel
	rightContent := m.renderDerivedAccountsPanel()
	rightColumn := lipgloss.NewStyle().
		Width(42).
		BorderLeft(true).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("241")).
		PaddingLeft(2).
		Render(rightContent)

	// Combine columns
	columns := lipgloss.JoinHorizontal(lipgloss.Top, leftColumn, rightColumn)

	// Build final content with title and hints
	var finalLines []string
	finalLines = append(finalLines, titleStyle.Render("Connection & Account Info"))
	finalLines = append(finalLines, "")
	finalLines = append(finalLines, columns)
	finalLines = append(finalLines, "")
	finalLines = append(finalLines, hintStyle.Render("Tab: edit key  Enter: scan  j/k: scroll  Esc: close  (or :scan <key>)"))

	content := strings.Join(finalLines, "\n")
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Padding(1, 2).
		Render(content)
	return box
}

// renderDerivedAccountsPanel renders the right-side panel for derived account balances.
func (m Model) renderDerivedAccountsPanel() string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	labelStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99"))
	valStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	inputActiveStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	inputInactiveStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))

	var lines []string
	lines = append(lines, sectionStyle.Render("Derived Accounts"))
	lines = append(lines, "")

	// Input field
	inputLabel := "Key: "
	inputVal := m.derivedKeyInput
	if len(inputVal) > 20 {
		inputVal = inputVal[:8] + "..." + inputVal[len(inputVal)-8:]
	}
	if inputVal == "" {
		inputVal = "(paste hex key)"
	}

	if m.derivedKeyInputActive {
		lines = append(lines, labelStyle.Render(inputLabel)+inputActiveStyle.Render("["+inputVal+"]"))
	} else {
		lines = append(lines, labelStyle.Render(inputLabel)+inputInactiveStyle.Render("["+inputVal+"]"))
	}
	lines = append(lines, "")

	// Results
	if m.derivedAccountsLoading {
		lines = append(lines, dimStyle.Render("Scanning..."))
	} else if len(m.derivedAccounts) == 0 {
		lines = append(lines, dimStyle.Render("No accounts scanned yet"))
		lines = append(lines, dimStyle.Render("Tab to edit, paste, Enter"))
		lines = append(lines, dimStyle.Render("or use :scan <key>"))
	} else {
		// Show accounts with scrolling
		const visibleRows = 8
		total := len(m.derivedAccounts)
		start := m.derivedAccountsScroll
		end := start + visibleRows
		if end > total {
			end = total
		}

		if m.derivedAccountsScroll > 0 {
			lines = append(lines, dimStyle.Render(fmt.Sprintf("  ^ %d more above", m.derivedAccountsScroll)))
		}

		for i := start; i < end; i++ {
			acc := m.derivedAccounts[i]
			addrShort := acc.Address
			if len(addrShort) > 14 {
				addrShort = addrShort[:8] + ".." + addrShort[len(addrShort)-4:]
			}
			lines = append(lines, labelStyle.Render(acc.Role)+" "+dimStyle.Render(addrShort))

			balStr := ""
			if acc.L1Bal > 0 {
				balStr += fmt.Sprintf("L1:%.4f ", acc.L1Bal)
			}
			if acc.L2Bal > 0 {
				balStr += fmt.Sprintf("L2:%.4f", acc.L2Bal)
			}
			lines = append(lines, "  "+valStyle.Render(balStr))
		}

		remaining := total - end
		if remaining > 0 {
			lines = append(lines, dimStyle.Render(fmt.Sprintf("  v %d more below", remaining)))
		}

		lines = append(lines, "")
		lines = append(lines, dimStyle.Render(fmt.Sprintf("Total: %d accounts", total)))
	}

	return strings.Join(lines, "\n")
}

// redactURL masks credentials in a URL for display.
func redactURL(u string) string {
	// Simple redaction: if URL contains @ (credentials), mask the user:pass part
	if idx := strings.Index(u, "@"); idx > 0 {
		// Find the scheme separator
		schemeEnd := strings.Index(u, "://")
		if schemeEnd > 0 && schemeEnd < idx {
			return u[:schemeEnd+3] + "***:***" + u[idx:]
		}
	}
	return u
}

// renderFormulaOverlay returns a styled popup showing the formula for the selected chart.
func (m Model) renderFormulaOverlay() string {
	entry := chartFormulas[m.selectedChart]

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

	// Render formula lines with colors for multi-dataset charts
	var formulaRendered string
	if colors, ok := multiDatasetFormulaColors[m.selectedChart]; ok {
		formulaLines := strings.Split(entry.Formula, "\n")
		var coloredLines []string
		for i, line := range formulaLines {
			if i < len(colors) {
				lineStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colors[i]))
				coloredLines = append(coloredLines, lineStyle.Render(line))
			} else {
				coloredLines = append(coloredLines, formulaStyle.Render(line))
			}
		}
		formulaRendered = strings.Join(coloredLines, "\n")
	} else {
		formulaRendered = formulaStyle.Render(entry.Formula)
	}

	lines := []string{
		titleStyle.Render(entry.Title),
		"",
		sectionStyle.Render("Formula"),
		formulaRendered,
		"",
		sectionStyle.Render("Description"),
		descStyle.Render(entry.Description),
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

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Padding(1, 3).
		Render(strings.Join(lines, "\n"))
	return box
}

// renderPanelConfigOverlay returns a styled popup for toggling chart panel visibility.
func (m Model) renderPanelConfigOverlay() string {
	overlayTitle := lipgloss.NewStyle().
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
	for i := 0; i < numCharts; i++ {
		check := uncheckStyle.Render("[ ]")
		if m.chartVisible[i] {
			check = checkStyle.Render("[x]")
		}
		label := normalStyle.Render(chartPanelLabels[i])
		prefix := "  "
		if i == m.panelConfigCursor {
			prefix = cursorStyle.Render("> ")
			label = cursorStyle.Render(chartPanelLabels[i])
		}
		lines = append(lines, fmt.Sprintf("%s%s %s", prefix, check, label))
	}

	content := strings.Join([]string{
		overlayTitle.Render("Panel Configuration"),
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

// renderQuitPromptOverlay returns a styled popup asking whether to save the CSV before quitting.
func (m Model) renderQuitPromptOverlay() string {
	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("205")).
		Align(lipgloss.Center)

	descStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("252"))

	pathStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("99"))

	hintStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241"))

	content := strings.Join([]string{
		titleStyle.Render("Save session data?"),
		"",
		descStyle.Render("CSV will be saved to:"),
		pathStyle.Render(m.statsPath),
		"",
		hintStyle.Render("y/Enter: Save and quit  |  n/Esc: Quit without saving"),
	}, "\n")

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Padding(1, 3).
		Render(content)
	return box
}

// renderPendingEventsPanel renders the pending events section if there are any events.
func (m Model) renderPendingEventsPanel(snap Snapshot, width int) string {
	events := snap.PendingEvents
	if len(events) == 0 {
		return ""
	}

	evtLabelStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	evtTypeStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99")).Width(18)
	evtAmtStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("114")).Width(14)
	evtDoneStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	var lines []string
	for _, e := range events {
		statusStr := string(e.Status)
		style := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
		switch e.Status {
		case EventComplete:
			style = evtDoneStyle
			statusStr = "complete"
		case EventFailed:
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
			statusStr = "FAILED"
		case EventPending:
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("215"))
		}
		txStr := ""
		if e.TxHash != "" {
			hash := e.TxHash
			if len(hash) > 10 {
				hash = hash[:10]
			}
			txStr = fmt.Sprintf(" tx:%s", hash)
		}
		detail := ""
		if e.Detail != "" {
			detail = " " + e.Detail
		}
		elapsed := time.Since(e.StartTime).Truncate(time.Second)
		line := evtTypeStyle.Render("["+e.Type+"]") +
			evtAmtStyle.Render(e.Amount) +
			style.Render(statusStr) +
			lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render(txStr+detail) +
			lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render(fmt.Sprintf(" (%s)", elapsed))
		lines = append(lines, line)
	}

	content := strings.Join(lines, "\n")
	title := evtLabelStyle.Render(fmt.Sprintf(" Pending Events (%d)", len(events)))
	return logBorderStyle.Width(width).Render(title + "\n" + content)
}

// renderCostLine renders the third header line with costs, theoretical max TPS, and bridge op counts.
func renderCostLine(snap Snapshot) string {
	var parts []string

	// Theoretical max TPS
	if snap.TheoreticalMaxTPS > 0 {
		parts = append(parts, costStyle.Render(fmt.Sprintf("MaxTPS(theory): %.0f", snap.TheoreticalMaxTPS)))
	}

	// RBTC deposit cost
	if snap.LastDepositCostRBTC > 0 {
		parts = append(parts, costStyle.Render(fmt.Sprintf("RBTC Dep: %.6f", snap.LastDepositCostRBTC)))
	}

	// RBTC exit cost (L2 init + prove + finalize)
	if snap.ExitCostRBTC > 0 {
		parts = append(parts, costStyle.Render(fmt.Sprintf("RBTC Exit: %.6f (L2:%.6f P:%.6f F:%.6f)",
			snap.ExitCostRBTC, snap.LastWithdrawL2InitCostRBTC, snap.LastProveCostRBTC, snap.LastFinalizeCostRBTC)))
	}

	// USDRIF deposit cost
	if snap.LastUSDRIFDepositCostRBTC > 0 {
		parts = append(parts, costStyle.Render(fmt.Sprintf("USDRIF Dep: %.6f", snap.LastUSDRIFDepositCostRBTC)))
	}

	// USDRIF exit cost (L2 init + prove + finalize)
	if snap.LastUSDRIFWithdrawCostRBTC > 0 {
		parts = append(parts, costStyle.Render(fmt.Sprintf("USDRIF Exit: %.6f (L2:%.6f P:%.6f F:%.6f)",
			snap.LastUSDRIFWithdrawCostRBTC, snap.LastUSDRIFWithdrawL2InitCostRBTC,
			snap.LastUSDRIFWithdrawProveCostRBTC, snap.LastUSDRIFWithdrawFinalizeCostRBTC)))
	}

	// Bridge operation counts
	totalOps := snap.RBTCDepositsTriggered + snap.RBTCWithdrawalsTriggered +
		snap.USDRIFDepositsTriggered + snap.USDRIFWithdrawalsTriggered
	if totalOps > 0 {
		parts = append(parts, costStyle.Render(fmt.Sprintf("RBTC: %dD/%dW  USDRIF: %dD/%dW",
			snap.RBTCDepositsTriggered, snap.RBTCWithdrawalsTriggered,
			snap.USDRIFDepositsTriggered, snap.USDRIFWithdrawalsTriggered)))
	}

	if len(parts) == 0 {
		return costStyle.Render("Costs: (run :deposit/:withdraw to measure bridge/exit costs)")
	}
	return strings.Join(parts, "  |  ")
}

// renderLogPanel renders the action log as a bordered panel.
func (m Model) renderLogPanel(snap Snapshot, width int) string {
	entries := snap.Log
	total := len(entries)

	// Compute visible window using scroll offset.
	// logScrollOffset 0 = pinned to bottom (most recent entries).
	end := total - m.logScrollOffset
	if end < 0 {
		end = 0
	}
	start := end - logLines
	if start < 0 {
		start = 0
	}

	var lines []string
	for _, entry := range entries[start:end] {
		ts := logTimeStyle.Render(entry.Time.Format("15:04:05"))
		msg := entry.Message
		// Color errors/warnings
		style := logMsgStyle
		if strings.Contains(msg, "error") || strings.Contains(msg, "FAILED") || strings.Contains(msg, "STALE") {
			style = logErrorStyle
		} else if strings.Contains(msg, "WARNING") || strings.Contains(msg, "SLOW") {
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
		}
		lines = append(lines, fmt.Sprintf("%s %s", ts, style.Render(msg)))
	}

	if len(lines) == 0 {
		lines = append(lines, logTimeStyle.Render("  (no actions yet -- press : to enter a command)"))
	}

	// Pad to logLines for stable layout
	for len(lines) < logLines {
		lines = append(lines, "")
	}

	content := strings.Join(lines, "\n")
	title := logLabelStyle.Render(" Action Log")
	// Show scroll indicator when not pinned to the bottom.
	if m.logScrollOffset > 0 {
		title += " " + statusStyle.Render(fmt.Sprintf("(+%d)", m.logScrollOffset))
	}
	return logBorderStyle.Width(width).Render(title + "\n" + content)
}

// logOverlayVisibleLines returns how many log lines fit in the overlay box.
func (m Model) logOverlayVisibleLines() int {
	// height - border(2) - title(1) - blank(1) - hint(1) - outer padding(4)
	lines := m.height - 9
	if lines < 3 {
		lines = 3
	}
	return lines
}

// renderLogOverlay returns a near-fullscreen scrollable action log popup.
func (m Model) renderLogOverlay(snap Snapshot) string {
	entries := snap.Log
	total := len(entries)
	visibleLines := m.logOverlayVisibleLines()

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
		ts := logTimeStyle.Render(entry.Time.Format("15:04:05"))
		msg := entry.Message
		style := logMsgStyle
		if strings.Contains(msg, "error") || strings.Contains(msg, "FAILED") || strings.Contains(msg, "STALE") {
			style = logErrorStyle
		} else if strings.Contains(msg, "WARNING") || strings.Contains(msg, "SLOW") {
			style = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
		}
		lines = append(lines, fmt.Sprintf("%s %s", ts, style.Render(msg)))
	}

	if len(lines) == 0 {
		lines = append(lines, logTimeStyle.Render("  (no actions yet -- press : to enter a command)"))
	}

	for len(lines) < visibleLines {
		lines = append(lines, "")
	}

	overlayTitle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("205")).
		Align(lipgloss.Center)

	hintStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241"))

	titleText := fmt.Sprintf("Action Log [%d entries]", total)
	if m.logOverlayOffset > 0 {
		showing := end - start
		titleText = fmt.Sprintf("Action Log [%d-%d of %d]", start+1, start+showing, total)
	}

	content := strings.Join([]string{
		overlayTitle.Render(titleText),
		"",
		strings.Join(lines, "\n"),
		"",
		hintStyle.Render("j/k: scroll  PgUp/PgDn: page  g/G: top/bottom  l/Esc: close"),
	}, "\n")

	boxWidth := m.width - 6
	if boxWidth < 40 {
		boxWidth = 40
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Padding(1, 3).
		Width(boxWidth).
		Render(content)

	return box
}

// renderBatcherStatus returns a styled batcher status string for the header.
// renderProcessStatus renders the header line showing process liveness, derivation
// state, and L1 head latency sourced from Prometheus endpoints.
func renderProcessStatus(snap Snapshot) string {
	upStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("82"))    // green
	downStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196")) // red
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	fmtUp := func(name string, up bool) string {
		if up {
			return upStyle.Render(name + ":UP")
		}
		return downStyle.Render(name + ":DOWN")
	}

	var parts []string

	// Only show process status when at least one Prometheus metric has been seen
	anyProm := snap.NodeUp || snap.BatcherUp || snap.ProposerUp ||
		len(snap.DerivationErrors) > 0 || len(snap.BatcherPendingBlocks) > 0 || len(snap.ProposedSeqNum) > 0
	if anyProm {
		parts = append(parts, fmtUp("node", snap.NodeUp))
		parts = append(parts, fmtUp("batcher", snap.BatcherUp))
		parts = append(parts, fmtUp("proposer", snap.ProposerUp))
	}

	if snap.DerivationIdle {
		parts = append(parts, dimStyle.Render("derivation:idle"))
	} else if len(snap.DerivationErrors) > 0 {
		parts = append(parts, dimStyle.Render("derivation:active"))
	}

	if len(snap.L1HeadLatency) > 0 {
		lat := snap.L1HeadLatency[len(snap.L1HeadLatency)-1].Value
		parts = append(parts, dimStyle.Render(fmt.Sprintf("L1 lag:%.1fs", lat)))
	}

	if len(parts) == 0 {
		return dimStyle.Render("Process status: (no Prometheus endpoints configured)")
	}
	return strings.Join(parts, "  ")
}

func renderBatcherStatus(snap Snapshot) string {
	var statusStr string
	if snap.BatcherStaleness < 0 {
		statusStr = batcherUnknownStyle.Render("Batcher: UNKNOWN")
	} else {
		secs := snap.BatcherStaleness
		if secs < 120 {
			statusStr = batcherOKStyle.Render(fmt.Sprintf("Batcher: OK (%.0fs)", secs))
		} else if secs < 300 {
			statusStr = batcherWarnStyle.Render(fmt.Sprintf("Batcher: SLOW (%.0fs)", secs))
		} else {
			statusStr = batcherErrStyle.Render(fmt.Sprintf("Batcher: STALE (%.0fs)", secs))
		}
	}

	// Add pending blocks and ETA if available
	etaStr := calcBatcherETA(snap)
	if etaStr != "" {
		statusStr += "  " + etaStr
	}

	return statusStr
}

// calcBatcherETA calculates estimated time for batcher to process pending blocks.
func calcBatcherETA(snap Snapshot) string {
	// Get current pending blocks
	if len(snap.BatcherPendingBlocks) == 0 {
		return ""
	}
	pendingBlocks := snap.BatcherPendingBlocks[len(snap.BatcherPendingBlocks)-1].Value
	if pendingBlocks <= 0 {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("82")).Render("Pending: 0")
	}

	// Calculate processing rate from L2BlocksPerBatch and PostingFreq
	var avgBlocksPerBatch, avgPostingFreq float64

	// Average L2 blocks per batch (use last N values)
	if len(snap.L2BlocksPerBatch) > 0 {
		sum := 0.0
		count := 0
		for i := len(snap.L2BlocksPerBatch) - 1; i >= 0 && count < 10; i-- {
			sum += snap.L2BlocksPerBatch[i].Value
			count++
		}
		if count > 0 {
			avgBlocksPerBatch = sum / float64(count)
		}
	}

	// Average posting frequency
	if len(snap.PostingFreq) > 0 {
		sum := 0.0
		count := 0
		for i := len(snap.PostingFreq) - 1; i >= 0 && count < 10; i-- {
			sum += snap.PostingFreq[i].Value
			count++
		}
		if count > 0 {
			avgPostingFreq = sum / float64(count)
		}
	}

	// Calculate rate and ETA
	pendingStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("215"))
	if avgBlocksPerBatch > 0 && avgPostingFreq > 0 {
		blocksPerSecond := avgBlocksPerBatch / avgPostingFreq
		if blocksPerSecond > 0 {
			etaSeconds := pendingBlocks / blocksPerSecond
			etaStr := formatDuration(etaSeconds)
			return pendingStyle.Render(fmt.Sprintf("Pending: %.0f blks (ETA: %s)", pendingBlocks, etaStr))
		}
	}

	return pendingStyle.Render(fmt.Sprintf("Pending: %.0f blks", pendingBlocks))
}

// formatDuration formats seconds into a human-readable duration string.
func formatDuration(seconds float64) string {
	if seconds < 60 {
		return fmt.Sprintf("%.0fs", seconds)
	}
	if seconds < 3600 {
		mins := seconds / 60
		return fmt.Sprintf("%.1fm", mins)
	}
	hours := seconds / 3600
	return fmt.Sprintf("%.1fh", hours)
}

// lastValueStr returns the formatted last value from a series, or "-" if empty.
func lastValueStr(data []TimeValue, format string) string {
	if len(data) == 0 {
		return "[-]"
	}
	return fmt.Sprintf("["+format+"]", data[len(data)-1].Value)
}

// fmtSummaryVal formats a summary value, returning "-" for zero.
func fmtSummaryVal(v float64, format string) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf(format, v)
}

// fmtDuration formats seconds into a human-readable duration like "1h23m" or "2d5h".
func fmtDuration(seconds float64) string {
	if seconds < 0 {
		return "-"
	}
	s := int(seconds)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm%ds", s/60, s%60)
	}
	if s < 86400 {
		return fmt.Sprintf("%dh%dm", s/3600, (s%3600)/60)
	}
	return fmt.Sprintf("%dd%dh", s/86400, (s%86400)/3600)
}

// normalizeTimeSeries scales every value in data to the range [0, 1] by dividing
// by the series maximum. Returns the original slice unchanged when maxVal is 0.
func normalizeTimeSeries(data []TimeValue) []TimeValue {
	var maxVal float64
	for _, tv := range data {
		if tv.Value > maxVal {
			maxVal = tv.Value
		}
	}
	if maxVal == 0 {
		return data
	}
	out := make([]TimeValue, len(data))
	for i, tv := range data {
		out[i] = TimeValue{Time: tv.Time, Value: tv.Value / maxVal}
	}
	return out
}

// zoomIndicator returns a short string describing the zoom state, or "" if default.
func zoomIndicator(s chartScale) string {
	yDefault := s.yZoom == 1.0
	tDefault := s.timeWin == defaultTimeWindow
	if yDefault && tDefault {
		return ""
	}
	var parts []string
	if !yDefault {
		parts = append(parts, fmt.Sprintf("y:%.1fx", s.yZoom))
	}
	if !tDefault {
		parts = append(parts, fmt.Sprintf("t:%s", fmtTimeWin(s.timeWin)))
	}
	return strings.Join(parts, " ")
}

// fmtTimeWin formats a duration as a short human-readable string (e.g. "30s", "2m30s", "1h").
func fmtTimeWin(d time.Duration) string {
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

// trafficContextStr returns a context string for metrics annotation (e.g. peak labels).
func (m *Model) trafficContextStr() string {
	if m.trafficSim != nil {
		target, ready := m.trafficSim.NumAccounts()
		return fmt.Sprintf("traffic ON @ %g tx/s, %d/%d accts", m.trafficRate, ready, target)
	}
	return fmt.Sprintf("traffic ON @ %g tx/s, %d accts", m.trafficRate, m.trafficAccounts)
}

// maxTrafficRate returns the dynamic maximum traffic rate. If theoretical max TPS
// has been computed from the chain's gas limit, that value is used as the cap.
// Otherwise falls back to defaultMaxTrafficRate.
func (m *Model) maxTrafficRate() float64 {
	snap := m.metrics.GetSnapshot()
	if snap.TheoreticalMaxTPS > 0 {
		return snap.TheoreticalMaxTPS
	}
	return defaultMaxTrafficRate
}

// derivedAccountsScanMsg is sent when the derived accounts scan completes.
type derivedAccountsScanMsg struct {
	accounts []DerivedAccountBalance
	err      error
}

// scanDerivedAccountsCmd returns a tea.Cmd that scans for derived accounts with non-zero balances.
// If keyHex is empty, scans all configured keys (ScanKeys + PrivateKey).
func (m *Model) scanDerivedAccountsCmd(keyHex string) tea.Cmd {
	return func() tea.Msg {
		var keys []string
		if keyHex != "" {
			keys = []string{keyHex}
		} else {
			// Collect all configured keys
			if m.cfg.PrivateKey != "" {
				keys = append(keys, m.cfg.PrivateKey)
			}
			keys = append(keys, m.cfg.ScanKeys...)
		}

		if len(keys) == 0 {
			return derivedAccountsScanMsg{err: fmt.Errorf("no keys configured")}
		}

		accounts, err := scanMultipleKeys(m.ctx, keys, m.cfg.L1RPC, m.cfg.L2RPC)
		return derivedAccountsScanMsg{accounts: accounts, err: err}
	}
}

// scanMultipleKeys scans derived accounts for multiple master keys.
func scanMultipleKeys(ctx context.Context, keys []string, l1RPC, l2RPC string) ([]DerivedAccountBalance, error) {
	var allResults []DerivedAccountBalance
	for i, keyHex := range keys {
		results, err := scanDerivedAccounts(ctx, keyHex, l1RPC, l2RPC, i+1, len(keys) > 1)
		if err != nil {
			continue // Skip invalid keys
		}
		allResults = append(allResults, results...)
	}
	if len(allResults) == 0 {
		return nil, fmt.Errorf("no accounts with non-zero balance found")
	}
	return allResults, nil
}

// scanDerivedAccounts scans role accounts and traffic accounts for non-zero balances.
// keyIdx is the 1-based index of this key (for labeling when multiple keys are scanned).
// addPrefix indicates whether to prefix roles with the key index.
func scanDerivedAccounts(ctx context.Context, keyHex, l1RPC, l2RPC string, keyIdx int, addPrefix bool) ([]DerivedAccountBalance, error) {
	keyHex = strings.TrimPrefix(keyHex, "0x")
	masterKey, err := crypto.HexToECDSA(keyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}

	var l1Client, l2Client *ethclient.Client
	if l1RPC != "" {
		c, err := ethclient.DialContext(ctx, l1RPC)
		if err == nil {
			l1Client = c
			defer c.Close()
		}
	}
	if l2RPC != "" {
		c, err := ethclient.DialContext(ctx, l2RPC)
		if err == nil {
			l2Client = c
			defer c.Close()
		}
	}

	if l1Client == nil && l2Client == nil {
		return nil, fmt.Errorf("no RPC connections available")
	}

	// Helper to add key prefix if multiple keys
	roleLabel := func(role string) string {
		if addPrefix {
			return fmt.Sprintf("[%d] %s", keyIdx, role)
		}
		return role
	}

	var results []DerivedAccountBalance

	// Check master account
	masterAddr := crypto.PubkeyToAddress(masterKey.PublicKey)
	{
		var l1Bal, l2Bal float64
		if l1Client != nil {
			if bal, err := l1Client.BalanceAt(ctx, masterAddr, nil); err == nil {
				l1Bal = weiToRBTC(bal)
			}
		}
		if l2Client != nil {
			if bal, err := l2Client.BalanceAt(ctx, masterAddr, nil); err == nil {
				l2Bal = weiToRBTC(bal)
			}
		}
		if l1Bal > 0 || l2Bal > 0 {
			results = append(results, DerivedAccountBalance{
				Role:    roleLabel("Master"),
				Address: masterAddr.Hex(),
				L1Bal:   l1Bal,
				L2Bal:   l2Bal,
			})
		}
	}

	// Check role accounts (batcher, proposer)
	for _, role := range []string{keyderive.RoleBatcher, keyderive.RoleProposer} {
		_, addr, err := keyderive.DeriveKey(keyHex, role)
		if err != nil {
			continue
		}
		var l1Bal, l2Bal float64
		if l1Client != nil {
			if bal, err := l1Client.BalanceAt(ctx, addr, nil); err == nil {
				l1Bal = weiToRBTC(bal)
			}
		}
		if l2Client != nil {
			if bal, err := l2Client.BalanceAt(ctx, addr, nil); err == nil {
				l2Bal = weiToRBTC(bal)
			}
		}
		if l1Bal > 0 || l2Bal > 0 {
			roleName := strings.ToUpper(role[:1]) + role[1:]
			results = append(results, DerivedAccountBalance{
				Role:    roleLabel(roleName),
				Address: addr.Hex(),
				L1Bal:   l1Bal,
				L2Bal:   l2Bal,
			})
		}
	}

	// Check traffic accounts (indices 0-999, L2 only)
	for i := 0; i < 1000; i++ {
		_, addr := DeriveTrafficKey(masterKey, i)
		if l2Client != nil {
			if bal, err := l2Client.BalanceAt(ctx, addr, nil); err == nil && bal.Sign() > 0 {
				results = append(results, DerivedAccountBalance{
					Role:    roleLabel(fmt.Sprintf("Traffic#%d", i)),
					Address: addr.Hex(),
					L1Bal:   0,
					L2Bal:   weiToRBTC(bal),
				})
			}
		}
	}

	return results, nil
}

// weiToRBTC converts wei to RBTC (float64).
func weiToRBTC(wei *big.Int) float64 {
	if wei == nil {
		return 0
	}
	f := new(big.Float).SetInt(wei)
	f.Quo(f, big.NewFloat(1e18))
	result, _ := f.Float64()
	return result
}
