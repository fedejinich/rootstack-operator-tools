package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/fedejinich/rootstack-operator-tools/cmd/rollup-monitor/monitor"
)

// HealthSnapshot is a read-only copy of health metrics for rendering.
type HealthSnapshot struct {
	// L1
	L1Block   uint64
	L1Latency float64

	// L2 (from optimism_syncStatus)
	UnsafeL2    uint64
	SafeL2      uint64
	FinalizedL2 uint64

	// High-water marks survive node restarts so the dashboard doesn't
	// show misleading regressions while the derivation pipeline re-catches up.
	SafeL2HWM      uint64
	FinalizedL2HWM uint64

	// Process liveness
	NodeUp     bool
	BatcherUp  bool
	ProposerUp bool

	// Component active state (running vs paused)
	SequencerActive bool
	BatcherActive   bool
	ProposerActive  bool

	// Pending action state (for visual feedback during toggle)
	SequencerPending bool
	BatcherPending   bool
	ProposerPending  bool

	// Derivation
	DerivationIdle   bool
	DerivationErrors float64
	PipelineResets   float64

	// Batcher pipeline
	BatcherPendingBlocks float64
	BatcherPendingBytes  float64
	BatcherGasBumps      float64
	BatcherBalance       float64
	BatcherStaleness     string

	// Proposer
	ProposedSeqNum  float64
	ProposerBalance float64
	ProposerChecked bool // true if we actually reached the proposer Prometheus

	// Timestamps
	LastUpdated time.Time
}

// HealthData holds mutable health metrics protected by a mutex.
type HealthData struct {
	mu sync.RWMutex
	s  HealthSnapshot
}

func (h *HealthData) Snapshot() HealthSnapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.s
}

// HealthCollector polls RPC and Prometheus endpoints to build HealthData.
type HealthCollector struct {
	ctx    context.Context
	cancel context.CancelFunc
	cfg    Config
	health *HealthData

	l1          *ethclient.Client
	l2          *ethclient.Client
	nodeRPC     *rpc.Client
	batcherRPC  *rpc.Client
	proposerRPC *rpc.Client
}

func NewHealthCollector(cfg Config) *HealthCollector {
	return &HealthCollector{
		cfg: cfg,
		health: &HealthData{s: HealthSnapshot{
			BatcherStaleness: "UNKNOWN",
			SequencerActive:  true, // assume active until we know otherwise
			BatcherActive:    true,
			ProposerActive:   true,
		}},
	}
}

func (hc *HealthCollector) Health() *HealthData {
	return hc.health
}

// Start begins the polling loops. Call Stop to clean up.
func (hc *HealthCollector) Start(parentCtx context.Context) {
	hc.ctx, hc.cancel = context.WithCancel(parentCtx)

	// Dial L2
	if hc.cfg.L2RPC != "" {
		if l2, err := ethclient.DialContext(hc.ctx, hc.cfg.L2RPC); err == nil {
			hc.l2 = l2
		}
	}

	// Dial L1
	if hc.cfg.L1RPC != "" {
		if l1, err := ethclient.DialContext(hc.ctx, hc.cfg.L1RPC); err == nil {
			hc.l1 = l1
		}
	}

	// Dial op-node
	if hc.cfg.NodeRPC != "" {
		if c, err := rpc.DialContext(hc.ctx, hc.cfg.NodeRPC); err == nil {
			hc.nodeRPC = c
		}
	}

	// Dial batcher admin RPC
	if hc.cfg.BatcherRPC != "" {
		if c, err := rpc.DialContext(hc.ctx, hc.cfg.BatcherRPC); err == nil {
			hc.batcherRPC = c
		}
	}

	// Dial proposer admin RPC
	if hc.cfg.ProposerRPC != "" {
		if c, err := rpc.DialContext(hc.ctx, hc.cfg.ProposerRPC); err == nil {
			hc.proposerRPC = c
		}
	}

	go hc.pollLoop()
}

func (hc *HealthCollector) Stop() {
	if hc.cancel != nil {
		hc.cancel()
	}
	if hc.l1 != nil {
		hc.l1.Close()
	}
	if hc.l2 != nil {
		hc.l2.Close()
	}
	if hc.nodeRPC != nil {
		hc.nodeRPC.Close()
	}
	if hc.batcherRPC != nil {
		hc.batcherRPC.Close()
	}
	if hc.proposerRPC != nil {
		hc.proposerRPC.Close()
	}
}

func (hc *HealthCollector) pollLoop() {
	hc.poll()

	ticker := time.NewTicker(hc.cfg.PollInterval)
	defer ticker.Stop()

	l1Ticker := time.NewTicker(hc.cfg.PollInterval * 5)
	defer l1Ticker.Stop()

	for {
		select {
		case <-hc.ctx.Done():
			return
		case <-ticker.C:
			hc.pollL2()
			hc.pollSyncStatus()
			hc.pollNodeMetrics()
			hc.pollBatcherMetrics()
			hc.pollProposerMetrics()
			hc.pollRPCLiveness()
			hc.pollComponentActiveState()
		case <-l1Ticker.C:
			hc.pollL1()
		}
	}
}

func (hc *HealthCollector) poll() {
	hc.pollL2()
	hc.pollL1()
	hc.pollSyncStatus()
	hc.pollNodeMetrics()
	hc.pollBatcherMetrics()
	hc.pollProposerMetrics()
	hc.pollRPCLiveness()
	hc.pollComponentActiveState()
}

func (hc *HealthCollector) pollL2() {
	if hc.l2 == nil {
		return
	}
	if _, err := hc.l2.BlockNumber(hc.ctx); err != nil {
		return
	}
	hc.health.mu.Lock()
	hc.health.s.LastUpdated = time.Now()
	hc.health.mu.Unlock()
}

func (hc *HealthCollector) pollL1() {
	if hc.l1 == nil {
		return
	}
	num, err := hc.l1.BlockNumber(hc.ctx)
	if err != nil {
		return
	}
	hc.health.mu.Lock()
	hc.health.s.L1Block = num
	hc.health.mu.Unlock()
}

func (hc *HealthCollector) pollSyncStatus() {
	if hc.nodeRPC == nil {
		return
	}

	var raw json.RawMessage
	if err := hc.nodeRPC.CallContext(hc.ctx, &raw, "optimism_syncStatus"); err != nil {
		return
	}

	var status monitor.SyncStatusResult
	if err := json.Unmarshal(raw, &status); err != nil {
		return
	}

	hc.health.mu.Lock()
	hc.health.s.UnsafeL2 = status.UnsafeL2.Number
	hc.health.s.SafeL2 = status.SafeL2.Number
	hc.health.s.FinalizedL2 = status.FinalizedL2.Number
	if status.SafeL2.Number > hc.health.s.SafeL2HWM {
		hc.health.s.SafeL2HWM = status.SafeL2.Number
	}
	if status.FinalizedL2.Number > hc.health.s.FinalizedL2HWM {
		hc.health.s.FinalizedL2HWM = status.FinalizedL2.Number
	}
	hc.health.mu.Unlock()
}

func (hc *HealthCollector) pollNodeMetrics() {
	if hc.cfg.NodeMetricsURL == "" {
		return
	}

	result, err := monitor.ScrapePrometheus(hc.ctx, hc.cfg.NodeMetricsURL)
	if err != nil {
		hc.health.mu.Lock()
		hc.health.s.NodeUp = false
		hc.health.mu.Unlock()
		return
	}

	hc.health.mu.Lock()
	defer hc.health.mu.Unlock()

	if up, ok := result.Get("_up"); ok {
		hc.health.s.NodeUp = up == 1
	} else {
		hc.health.s.NodeUp = true
	}

	if idle, ok := result.Get("derivation_idle"); ok {
		hc.health.s.DerivationIdle = idle == 1
	}

	if v, ok := result.Get("derivation_errors_total"); ok {
		hc.health.s.DerivationErrors = v
	}
	if v, ok := result.Get("pipeline_resets_total"); ok {
		hc.health.s.PipelineResets = v
	}
	if v, ok := result.GetLabeled("refs_latency", "layer", "l1", "type", "head"); ok {
		if v < 0 {
			v = -v
		}
		hc.health.s.L1Latency = v
	}
}

func (hc *HealthCollector) pollBatcherMetrics() {
	if hc.cfg.BatcherMetricsURL == "" {
		return
	}

	result, err := monitor.ScrapePrometheus(hc.ctx, hc.cfg.BatcherMetricsURL)
	if err != nil {
		hc.health.mu.Lock()
		hc.health.s.BatcherUp = false
		hc.health.mu.Unlock()
		return
	}

	hc.health.mu.Lock()
	defer hc.health.mu.Unlock()

	if up, ok := result.Get("_up"); ok {
		hc.health.s.BatcherUp = up == 1
	} else {
		hc.health.s.BatcherUp = true
	}

	if v, ok := result.BatcherPendingBlocksCount(); ok {
		hc.health.s.BatcherPendingBlocks = v
	}
	if v, ok := result.Get("pending_blocks_bytes_current"); ok {
		hc.health.s.BatcherPendingBytes = v
	}
	if v, ok := result.Get("txmgr_tx_gas_bump"); ok {
		hc.health.s.BatcherGasBumps = v
	}
	if v, ok := result.Get("_balance"); ok {
		hc.health.s.BatcherBalance = v
	}
}

func (hc *HealthCollector) pollProposerMetrics() {
	if hc.cfg.ProposerMetricsURL == "" {
		return
	}

	result, err := monitor.ScrapePrometheus(hc.ctx, hc.cfg.ProposerMetricsURL)
	if err != nil {
		hc.health.mu.Lock()
		hc.health.s.ProposerUp = false
		hc.health.s.ProposerChecked = true
		hc.health.mu.Unlock()
		return
	}

	hc.health.mu.Lock()
	defer hc.health.mu.Unlock()

	hc.health.s.ProposerChecked = true
	if up, ok := result.Get("_up"); ok {
		hc.health.s.ProposerUp = up == 1
	} else {
		hc.health.s.ProposerUp = true
	}

	if v, ok := result.Get("proposed_sequence_number"); ok {
		hc.health.s.ProposedSeqNum = v
	}
	if v, ok := result.Get("_balance"); ok {
		hc.health.s.ProposerBalance = v
	}
}

// pollRPCLiveness uses RPC connectivity as a fallback for process liveness.
// If Prometheus metrics already set a component as UP, this is a no-op for
// that component. Otherwise, it tries RPC calls to determine reachability.
// It also lazily (re)dials RPC connections that failed at startup.
func (hc *HealthCollector) pollRPCLiveness() {
	// Lazily connect RPC clients that weren't ready at Start() time
	hc.ensureRPCConnections()

	hc.health.mu.Lock()
	nodeUp := hc.health.s.NodeUp
	batcherUp := hc.health.s.BatcherUp
	proposerUp := hc.health.s.ProposerUp
	hc.health.mu.Unlock()

	// Node: if not already UP via Prometheus, check if op-node RPC responds
	if !nodeUp && hc.nodeRPC != nil {
		var raw json.RawMessage
		if err := hc.nodeRPC.CallContext(hc.ctx, &raw, "optimism_syncStatus"); err == nil {
			hc.health.mu.Lock()
			hc.health.s.NodeUp = true
			hc.health.mu.Unlock()
		}
	}

	// Batcher: if not already UP, check if batcher admin RPC responds.
	if !batcherUp && hc.batcherRPC != nil {
		var result interface{}
		err := hc.batcherRPC.CallContext(hc.ctx, &result, "admin_getStatus")
		if err == nil || isJSONRPCError(err) {
			hc.health.mu.Lock()
			hc.health.s.BatcherUp = true
			hc.health.mu.Unlock()
		}
	}

	// Proposer: if not already UP via Prometheus, check if proposer admin RPC responds.
	if !proposerUp && hc.proposerRPC != nil {
		var result interface{}
		err := hc.proposerRPC.CallContext(hc.ctx, &result, "admin_getStatus")
		if err == nil || isJSONRPCError(err) {
			hc.health.mu.Lock()
			hc.health.s.ProposerUp = true
			hc.health.s.ProposerChecked = true
			hc.health.mu.Unlock()
		}
	}
}

// ensureRPCConnections lazily dials any RPC endpoints that weren't available
// during Start() (e.g. the proposer starts after the health collector).
func (hc *HealthCollector) ensureRPCConnections() {
	if hc.nodeRPC == nil && hc.cfg.NodeRPC != "" {
		if c, err := rpc.DialContext(hc.ctx, hc.cfg.NodeRPC); err == nil {
			hc.nodeRPC = c
		}
	}
	if hc.batcherRPC == nil && hc.cfg.BatcherRPC != "" {
		if c, err := rpc.DialContext(hc.ctx, hc.cfg.BatcherRPC); err == nil {
			hc.batcherRPC = c
		}
	}
	if hc.proposerRPC == nil && hc.cfg.ProposerRPC != "" {
		if c, err := rpc.DialContext(hc.ctx, hc.cfg.ProposerRPC); err == nil {
			hc.proposerRPC = c
		}
	}
}

// pollComponentActiveState queries the active/paused state of components.
// Sequencer has an admin RPC for this. Batcher/proposer state is updated when toggled via dashboard.
func (hc *HealthCollector) pollComponentActiveState() {
	if hc.nodeRPC != nil {
		var active bool
		if err := hc.nodeRPC.CallContext(hc.ctx, &active, "admin_sequencerActive"); err == nil {
			hc.health.mu.Lock()
			hc.health.s.SequencerActive = active
			hc.health.mu.Unlock()
		}
	}
}

// StopSequencer pauses L2 block production via admin RPC.
func (hc *HealthCollector) StopSequencer(ctx context.Context) error {
	if hc.nodeRPC == nil {
		return fmt.Errorf("op-node RPC not connected")
	}
	var hash string
	return hc.nodeRPC.CallContext(ctx, &hash, "admin_stopSequencer")
}

// StartSequencer resumes L2 block production via admin RPC.
// It requires the hash of the last unsafe L2 block as a safety check.
func (hc *HealthCollector) StartSequencer(ctx context.Context, unsafeHeadHash string) error {
	if hc.nodeRPC == nil {
		return fmt.Errorf("op-node RPC not connected")
	}
	return hc.nodeRPC.CallContext(ctx, nil, "admin_startSequencer", unsafeHeadHash)
}

// GetUnsafeHeadHash returns the hash of the current unsafe L2 head.
func (hc *HealthCollector) GetUnsafeHeadHash(ctx context.Context) (string, error) {
	if hc.nodeRPC == nil {
		return "", fmt.Errorf("op-node RPC not connected")
	}
	var raw json.RawMessage
	if err := hc.nodeRPC.CallContext(ctx, &raw, "optimism_syncStatus"); err != nil {
		return "", err
	}
	var status struct {
		UnsafeL2 struct {
			Hash string `json:"hash"`
		} `json:"unsafe_l2"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return "", err
	}
	return status.UnsafeL2.Hash, nil
}

// StopBatcher pauses batch submission via admin RPC.
func (hc *HealthCollector) StopBatcher(ctx context.Context) error {
	if hc.batcherRPC == nil {
		return fmt.Errorf("batcher RPC not connected")
	}
	return hc.batcherRPC.CallContext(ctx, nil, "admin_stopBatcher")
}

// StartBatcher resumes batch submission via admin RPC.
func (hc *HealthCollector) StartBatcher(ctx context.Context) error {
	if hc.batcherRPC == nil {
		return fmt.Errorf("batcher RPC not connected")
	}
	return hc.batcherRPC.CallContext(ctx, nil, "admin_startBatcher")
}

// FlushBatcher forces immediate posting of pending batch data.
func (hc *HealthCollector) FlushBatcher(ctx context.Context) error {
	if hc.batcherRPC == nil {
		return fmt.Errorf("batcher RPC not connected")
	}
	return hc.batcherRPC.CallContext(ctx, nil, "admin_flushBatcher")
}

// StopProposer pauses output root proposal submission.
func (hc *HealthCollector) StopProposer(ctx context.Context) error {
	if hc.proposerRPC == nil {
		return fmt.Errorf("proposer RPC not connected")
	}
	return hc.proposerRPC.CallContext(ctx, nil, "admin_stopProposer")
}

// StartProposer resumes output root proposal submission.
func (hc *HealthCollector) StartProposer(ctx context.Context) error {
	if hc.proposerRPC == nil {
		return fmt.Errorf("proposer RPC not connected")
	}
	return hc.proposerRPC.CallContext(ctx, nil, "admin_startProposer")
}

// SetPending marks a component as having a pending action (for UI feedback).
func (hc *HealthCollector) SetPending(component string, pending bool) {
	hc.health.mu.Lock()
	defer hc.health.mu.Unlock()
	switch component {
	case "sequencer":
		hc.health.s.SequencerPending = pending
	case "batcher":
		hc.health.s.BatcherPending = pending
	case "proposer":
		hc.health.s.ProposerPending = pending
	}
}

// SetActiveState updates the active state of a component immediately after a successful action.
func (hc *HealthCollector) SetActiveState(component string, active bool) {
	hc.health.mu.Lock()
	defer hc.health.mu.Unlock()
	switch component {
	case "sequencer":
		hc.health.s.SequencerActive = active
	case "batcher":
		hc.health.s.BatcherActive = active
	case "proposer":
		hc.health.s.ProposerActive = active
	}
}

// isJSONRPCError returns true if the error is a JSON-RPC method-level error
// (meaning the server IS reachable, but the method was unknown or returned an error).
func isJSONRPCError(err error) bool {
	if err == nil {
		return false
	}
	// go-ethereum rpc.Client wraps JSON-RPC errors as *rpc.jsonError which
	// implements error. Network errors contain "dial", "connect", "EOF", etc.
	s := err.Error()
	if strings.Contains(s, "the method") || strings.Contains(s, "method not found") ||
		strings.Contains(s, "Method not found") || strings.Contains(s, "does not exist") {
		return true
	}
	// If we got a structured JSON-RPC error code, the server responded
	if _, ok := err.(rpc.Error); ok {
		return true
	}
	return false
}

// --- Rendering ---

var (
	healthOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	healthWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	healthBad   = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	healthDim   = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	healthLabel = lipgloss.NewStyle().Foreground(lipgloss.Color("248"))
)

func processStatusStyle(up bool) lipgloss.Style {
	if up {
		return healthOK
	}
	return healthBad
}

func processStatusText(up bool) string {
	if up {
		return "UP"
	}
	return "DOWN"
}

// RenderHealthBar renders a compact 3-line health overview.
func RenderHealthBar(h HealthSnapshot, width int) string {
	// Line 1: Block numbers and sync
	var syncLag string
	effectiveSafe := h.SafeL2HWM
	if h.SafeL2 > effectiveSafe {
		effectiveSafe = h.SafeL2
	}
	if h.UnsafeL2 > effectiveSafe {
		syncLag = fmt.Sprintf("lag:%d", h.UnsafeL2-effectiveSafe)
	} else {
		syncLag = "synced"
	}

	safeStr := fmt.Sprintf("%d", h.SafeL2)
	if h.SafeL2 < h.SafeL2HWM {
		safeStr = fmt.Sprintf("%d(re-deriving, prev:%d)", h.SafeL2, h.SafeL2HWM)
	}
	finalStr := fmt.Sprintf("%d", h.FinalizedL2)
	if h.FinalizedL2 < h.FinalizedL2HWM {
		finalStr = fmt.Sprintf("%d(prev:%d)", h.FinalizedL2, h.FinalizedL2HWM)
	}

	line1 := fmt.Sprintf(
		"%s %s  %s %s  %s  %s",
		healthLabel.Render("L1:"),
		healthOK.Render(fmt.Sprintf("#%d", h.L1Block)),
		healthLabel.Render("L2:"),
		healthOK.Render(fmt.Sprintf("#%d", h.UnsafeL2)),
		healthDim.Render(fmt.Sprintf("(unsafe:%d safe:%s final:%s)", h.UnsafeL2, safeStr, finalStr)),
		healthLabel.Render(syncLag),
	)

	// Line 2: LED indicators + process status
	// LED indicators like car dashboard lights: ● active, ◐ paused, ◑ pending, ○ down
	const ledOn = "●"
	const ledPaused = "◐"
	const ledPending = "◑"
	const ledOff = "○"

	// Sequencer LED
	seqLED := healthOK.Render(ledOn)
	seqLabel := "SEQ"
	if h.SequencerPending {
		seqLED = healthWarn.Render(ledPending)
	} else if !h.SequencerActive {
		seqLED = healthWarn.Render(ledPaused)
	}

	// Batcher LED
	batchLED := healthOK.Render(ledOn)
	batchLabel := "BATCH"
	if h.BatcherPending {
		batchLED = healthWarn.Render(ledPending)
	} else if !h.BatcherUp {
		batchLED = healthBad.Render(ledOff)
	} else if !h.BatcherActive {
		batchLED = healthWarn.Render(ledPaused)
	}

	// Proposer LED
	propLED := healthOK.Render(ledOn)
	propLabel := "PROP"
	if h.ProposerPending {
		propLED = healthWarn.Render(ledPending)
	} else if !h.ProposerUp && !h.ProposerChecked {
		propLED = healthDim.Render(ledOff)
	} else if !h.ProposerUp {
		propLED = healthBad.Render(ledOff)
	} else if !h.ProposerActive {
		propLED = healthWarn.Render(ledPaused)
	}

	// Derivation LED
	derivLED := healthOK.Render(ledOn)
	derivLabel := "DERIV"
	if h.DerivationIdle {
		derivLED = healthDim.Render(ledOff)
	}

	// Node process status
	nodeStatus := processStatusStyle(h.NodeUp).Render("node:" + processStatusText(h.NodeUp))
	l1LatStr := healthDim.Render(fmt.Sprintf("L1lag:%.1fs", h.L1Latency))

	line2 := fmt.Sprintf("%s %s  %s %s  %s %s  %s %s  │  %s  %s",
		seqLED, healthLabel.Render(seqLabel),
		batchLED, healthLabel.Render(batchLabel),
		propLED, healthLabel.Render(propLabel),
		derivLED, healthLabel.Render(derivLabel),
		nodeStatus, l1LatStr,
	)

	// Line 3: Batcher pipeline + proposer progress
	batcherInfo := healthDim.Render("batcher:N/A")
	if h.BatcherUp {
		pendStyle := healthOK
		if h.BatcherPendingBlocks > 50 {
			pendStyle = healthWarn
		}
		if h.BatcherPendingBlocks > 200 {
			pendStyle = healthBad
		}
		batcherInfo = fmt.Sprintf(
			"%s %s %s",
			healthLabel.Render("pending:"),
			pendStyle.Render(fmt.Sprintf("%.0f blks / %.0f B", h.BatcherPendingBlocks, h.BatcherPendingBytes)),
			healthDim.Render(fmt.Sprintf("bumps:%.0f bal:%.6f", h.BatcherGasBumps, h.BatcherBalance)),
		)
	}

	proposerInfo := healthDim.Render("proposer:N/A")
	if h.ProposerUp {
		proposerInfo = fmt.Sprintf("%s %s",
			healthLabel.Render("proposed:"),
			healthDim.Render(fmt.Sprintf("#%.0f bal:%.6f", h.ProposedSeqNum, h.ProposerBalance)),
		)
	}

	line3 := fmt.Sprintf("%s  │  %s", batcherInfo, proposerInfo)

	// Truncate lines to width
	lines := []string{line1, line2, line3}
	for i, l := range lines {
		if width > 0 && lipgloss.Width(l) > width {
			lines[i] = l[:width]
		}
	}

	return strings.Join(lines, "\n")
}
