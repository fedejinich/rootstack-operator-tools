package main

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/fedejinich/rootstack-operator-tools/cmd/rollup-monitor/monitor"
)

const (
	// adaptiveSeedRate is the initial tx/s rate when adaptive mode starts up.
	adaptiveSeedRate = 10.0

	// adaptiveDefaultTargetPending is the default txpool_pending target for
	// the adaptive rate controller.
	adaptiveDefaultTargetPending = 100

	// adaptiveDefaultMaxRate caps the adaptive controller to prevent runaway
	// when metrics lag behind reality.
	adaptiveDefaultMaxRate = 5000.0

	// adaptiveDecisionInterval is the minimum time between AIMD rate
	// adjustments. Aligning this with the block time prevents the controller
	// from issuing dozens of adjustments between blocks when the pending
	// metric is stale.
	adaptiveDefaultDecisionInterval = 4 * time.Second

	// adaptiveEMAlpha is the smoothing factor for the exponential moving
	// average applied to txpool_pending. Lower values smooth more
	// aggressively but react more slowly. 0.3 gives ~3-sample half-life.
	adaptiveEMAlpha = 0.3
)

// ExperimentConfig is the top-level TOML experiment configuration.
type ExperimentConfig struct {
	Target          TargetConfig       `toml:"target"`
	Simulation      SimulationConfig   `toml:"simulation"`
	Batcher         BatcherKnobs       `toml:"batcher"`
	Chain           ChainKnobs         `toml:"chain"`
	Measurement     MeasurementConfig  `toml:"measurements"`
	Live            LiveConfig         `toml:"live"`
	RunStart        RunStartConditions `toml:"run_start_conditions"`
	NextRunStrategy string             `toml:"next_run_strategy"`
}

// LiveConfig configures the analysis TUI shown in --live mode.
type LiveConfig struct {
	Panels        []string `toml:"panels"`
	ShowLog       *bool    `toml:"show_log"`
	LogLines      int      `toml:"log_lines"`
	LogBufferSize int      `toml:"log_buffer_size"`
	TimeWindow    string   `toml:"time_window"`
	AutoQuit      *bool    `toml:"auto_quit"`
}

// ThresholdValue represents a condition threshold that can be either a numeric
// value or "any" (disabled). Used in RunStartConditions.
type ThresholdValue struct {
	Disabled bool
	Value    int
}

func (tv *ThresholdValue) UnmarshalTOML(data interface{}) error {
	switch v := data.(type) {
	case string:
		if v == "any" {
			tv.Disabled = true
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("threshold must be a number or \"any\", got %q", v)
		}
		tv.Value = n
	case int64:
		tv.Value = int(v)
	case float64:
		tv.Value = int(v)
	default:
		return fmt.Errorf("threshold must be a number or \"any\", got %T", data)
	}
	return nil
}

// RunStartConditions defines thresholds that must be met before starting each
// experiment run. This ensures consistent initial state across runs.
type RunStartConditions struct {
	PendingBlocks  ThresholdValue `toml:"pending_blocks"`
	SafeLag        ThresholdValue `toml:"safe_lag"`
	Timeout        string         `toml:"timeout"`
	OnFirstRun     *bool          `toml:"on_first_run"`
	PauseSequencer *bool          `toml:"pause_sequencer"`
	FlushBatcher   *bool          `toml:"flush_batcher"`
}

// ResolvedRunStartConditions holds the parsed run start conditions with defaults applied.
type ResolvedRunStartConditions struct {
	PendingBlocks    int
	PendingBlocksAny bool
	SafeLag          int
	SafeLagAny       bool
	Timeout          time.Duration
	OnFirstRun       bool
	PauseSequencer   bool
	FlushBatcher     bool
}

// ResolveRunStartConditions applies defaults and returns resolved conditions.
func ResolveRunStartConditions(rsc RunStartConditions) ResolvedRunStartConditions {
	r := ResolvedRunStartConditions{
		PendingBlocks:    500,
		PendingBlocksAny: false,
		SafeLag:          0,
		SafeLagAny:       true,
		Timeout:          5 * time.Minute,
		OnFirstRun:       false,
	}

	if rsc.PendingBlocks.Disabled {
		r.PendingBlocksAny = true
	} else if rsc.PendingBlocks.Value > 0 {
		r.PendingBlocks = rsc.PendingBlocks.Value
	}

	if rsc.SafeLag.Disabled {
		r.SafeLagAny = true
	} else if rsc.SafeLag.Value > 0 {
		r.SafeLag = rsc.SafeLag.Value
		r.SafeLagAny = false
	}

	if rsc.Timeout != "" {
		if d, err := time.ParseDuration(rsc.Timeout); err == nil && d > 0 {
			r.Timeout = d
		}
	}

	if rsc.OnFirstRun != nil {
		r.OnFirstRun = *rsc.OnFirstRun
	}

	if rsc.PauseSequencer != nil {
		r.PauseSequencer = *rsc.PauseSequencer
	}

	if rsc.FlushBatcher != nil {
		r.FlushBatcher = *rsc.FlushBatcher
	}

	return r
}

// HasConditions returns true if any run start condition is enabled.
func (r ResolvedRunStartConditions) HasConditions() bool {
	return !r.PendingBlocksAny || !r.SafeLagAny
}

// AllPanelNames returns the full ordered list of available TUI panel keys.
// Derived from analysisPanels so it stays in sync automatically.
func AllPanelNames() []string {
	names := make([]string, len(analysisPanels))
	for i, p := range analysisPanels {
		names[i] = p.key
	}
	return names
}

// ResolvedLiveConfig holds the parsed, defaults-applied live TUI configuration.
type ResolvedLiveConfig struct {
	Panels        []string
	ShowLog       bool
	LogLines      int
	LogBufferSize int
	TimeWindow    time.Duration
	AutoQuit      bool
}

// ResolveLiveConfig applies defaults and parses the LiveConfig.
func ResolveLiveConfig(lc LiveConfig) ResolvedLiveConfig {
	r := ResolvedLiveConfig{
		Panels:        lc.Panels,
		ShowLog:       true,
		LogLines:      lc.LogLines,
		LogBufferSize: lc.LogBufferSize,
	}
	if len(r.Panels) == 0 {
		r.Panels = AllPanelNames()
	}
	if lc.ShowLog != nil {
		r.ShowLog = *lc.ShowLog
	}
	if r.LogLines <= 0 {
		r.LogLines = 3
	}
	if r.LogBufferSize <= 0 {
		r.LogBufferSize = 200
	}
	if lc.AutoQuit != nil {
		r.AutoQuit = *lc.AutoQuit
	}
	if lc.TimeWindow != "" {
		if d, err := time.ParseDuration(lc.TimeWindow); err == nil && d > 0 {
			r.TimeWindow = d
		}
	}
	if r.TimeWindow <= 0 {
		r.TimeWindow = 5 * time.Minute
	}
	return r
}

// TargetConfig specifies the running L2 infrastructure to connect to.
type TargetConfig struct {
	L1RPC      string `toml:"l1_rpc"`
	L2RPC      string `toml:"l2_rpc"`
	NodeRPC    string `toml:"node_rpc"`
	PrivateKey string `toml:"private_key"`
	WorkDir    string `toml:"workdir"`

	BatcherBin        string `toml:"batcher_bin"`
	RollupNodeBin     string `toml:"rollup_node_bin"`
	RollupNodeConfig  string `toml:"rollup_node_config"`
	BatcherMetricsURL string `toml:"batcher_metrics_url"`

	// BatcherRPCURL is the admin RPC endpoint for controlling the batcher.
	// When set (and batcher_bin is empty), the experiment controls the batcher
	// via admin_startBatcher/admin_stopBatcher RPC calls instead of spawning
	// a subprocess. This is useful when the batcher runs embedded in rollup-node.
	BatcherRPCURL string `toml:"batcher_rpc_url"`

	// BatcherConfigPath is the path to the batcher config file (TOML).
	// When using RPC mode, the experiment writes batcher parameters to this file
	// before calling admin_startBatcher. This allows changing batcher config
	// between runs if the batcher reloads config on start.
	BatcherConfigPath string `toml:"batcher_config_path"`

	// Prometheus endpoints for op-node and op-proposer
	NodeMetricsURL     string `toml:"node_metrics_url"`
	ProposerMetricsURL string `toml:"proposer_metrics_url"`

	// pprof endpoints for runtime profiling
	NodePprofURL     string `toml:"node_pprof_url"`
	BatcherPprofURL  string `toml:"batcher_pprof_url"`
	ProposerPprofURL string `toml:"proposer_pprof_url"`
}

// ChainKnobs defines L2 chain-level parameters that can be swept.
// gas_limit is changed at runtime via SystemConfig.setGasLimit() on L1.
// block_time requires editing rollup.json and restarting rollup-node.
type ChainKnobs struct {
	GasLimit  SweepValue `toml:"gas_limit"`
	BlockTime SweepValue `toml:"block_time"`
}

// LengthValue is a TOML value that can be a single string or an array of strings.
// A single string specifies one stop condition. An array specifies multiple
// conditions combined with OR semantics — the experiment stops when any is met.
type LengthValue struct {
	Raw []string
}

func (lv *LengthValue) UnmarshalTOML(data interface{}) error {
	switch v := data.(type) {
	case string:
		lv.Raw = []string{v}
	case []interface{}:
		lv.Raw = make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return fmt.Errorf("length array element must be a string, got %T", item)
			}
			lv.Raw = append(lv.Raw, s)
		}
	default:
		return fmt.Errorf("length must be a string or array of strings, got %T", data)
	}
	return nil
}

// SimulationConfig defines the dynamic simulation parameters.
type SimulationConfig struct {
	Length         LengthValue  `toml:"length"`
	Accounts       int          `toml:"accounts"`
	OnlyMonologues bool         `toml:"only_monologues"`
	SampleInterval string       `toml:"sample_interval"`
	DrainOnExit    *bool        `toml:"drain_on_exit"`
	SimpleTx       SimpleTxTOML `toml:"simple_tx"`
	ERC20Tx        ERC20TxTOML  `toml:"erc20_tx"`
	SwapTx         SwapTxTOML   `toml:"swap_tx"`
	DepositRBTC    BridgeTxTOML `toml:"deposit_rbtc"`
	DepositERC20   BridgeTxTOML `toml:"deposit_erc20"`
	WithdrawRBTC   BridgeTxTOML `toml:"withdraw_rbtc"`
	WithdrawERC20  BridgeTxTOML `toml:"withdraw_erc20"`
}

// SimpleTxTOML defines simple transaction traffic parameters.
// Both rate and value support the full curve syntax (constant, array, random, jitter).
//
// RateRuns, when non-empty, triggers one experiment run per value (multiplied by
// the batcher/chain sweep). Each run uses a constant simple_tx rate equal to that
// value. Incompatible with adaptive, random, jitter, or multi-point rate curves;
// use rate = 0 or omit rate when RateRuns is set.
type SimpleTxTOML struct {
	Rate         CurveValue `toml:"rate"`
	RateRuns     []float64  `toml:"rate_runs"`
	Value        CurveValue `toml:"value"`
	CalldataSize int        `toml:"calldata_size"`
}

// ERC20TxTOML defines L2 ERC20 transfer traffic parameters.
// Rate and value support the full curve syntax. Value is in token smallest units.
type ERC20TxTOML struct {
	Rate  CurveValue `toml:"rate"`
	Value CurveValue `toml:"value"`
}

// SwapTxTOML defines L2 mock-swap traffic parameters. The simulator
// auto-deploys a MockSwapRouter + 2 ERC-20s on first run (or reads the
// optional pre-deployed addresses below). Each tick a random derived
// account calls router.swap(...) alternating direction.
//
// Calldata shape: 4 + 6*32 = 196 bytes — the L1 batch-cost overhead
// signal vs ERC20 transfer (68 bytes) is the point of the experiment.
type SwapTxTOML struct {
	Rate  CurveValue `toml:"rate"`
	Value CurveValue `toml:"value"`
	// Optional: pre-deployed contracts. If unset, the runner deploys a
	// fresh trio (TokenA, TokenB, MockSwapRouter) at preflight time.
	Router string `toml:"router"`
	TokenA string `toml:"token_a"`
	TokenB string `toml:"token_b"`
}

// BridgeTxTOML defines bridging transaction parameters (deposit or withdrawal).
// Both rate and value support the full curve syntax (constant, array, random, jitter).
// Rate is in operations/second (higher = more operations). 0 = disabled.
type BridgeTxTOML struct {
	Rate            CurveValue `toml:"rate"`
	Value           CurveValue `toml:"value"`
	ERC20ContractL1 string     `toml:"erc20_contract_l1"`
	ERC20ContractL2 string     `toml:"erc20_contract_l2"`
}

// BatcherKnobs defines batcher configuration. Array values trigger sweep runs.
type BatcherKnobs struct {
	MaxL1TxSize           SweepValue `toml:"max_l1_tx_size"`
	MaxChannelDuration    SweepValue `toml:"max_channel_duration"`
	MaxBlocksPerSpanBatch SweepValue `toml:"max_blocks_per_span_batch"`
	MaxPendingTx          SweepValue `toml:"max_pending_tx"`
	BatchType             *uint      `toml:"batch_type"`
	CompressionAlgo       string     `toml:"compression_algo"`
	SubSafetyMargin       *uint64    `toml:"sub_safety_margin"`
	BatcherMetricsURL     string     `toml:"batcher_metrics_url"`
}

// MeasurementConfig selects which measurements to collect.
type MeasurementConfig struct {
	EnableAll        bool `toml:"enable_all"`
	TPS              bool `toml:"tps"`
	L1TxCost         bool `toml:"l1_tx_cost"`
	L2TxCost         bool `toml:"l2_tx_cost"`
	TotalTxCost      bool `toml:"total_tx_cost"`
	DepositCost      bool `toml:"deposit_cost"`
	WithdrawalCost   bool `toml:"withdrawal_cost"`
	CompressionRatio bool `toml:"compression_ratio"`
	DataPerTx        bool `toml:"data_per_tx"`
}

// CurveValue is a TOML value that supports multiple forms:
//
// Scalar (constant):
//
//	rate = 10.0
//
// Array (piecewise-linear curve):
//
//	rate = [1.0, 10.0, 50.0, 100.0]
//
// Table with random mode:
//
//	rate = { random = true, min = 0.0, max = 100.0 }
//
// Table with base curve + jitter:
//
//	rate = { base = [1.0, 50.0], jitter = 5.0 }
//
// Table combining base curve + random cap (syntactic sugar for random with min from base):
//
//	rate = { random = true, cap = 100.0 }     # same as min=0, max=100
type CurveValue struct {
	Values []float64
	Random bool
	Min    float64
	Max    float64
	Jitter float64

	Adaptive      bool
	TargetPending int
	MaxRate       float64
}

// IsAdaptive returns true if this curve uses the adaptive max-pressure mode.
func (cv *CurveValue) IsAdaptive() bool {
	return cv != nil && cv.Adaptive
}

func (cv *CurveValue) UnmarshalTOML(data interface{}) error {
	switch v := data.(type) {
	case int64:
		cv.Values = []float64{float64(v)}
	case float64:
		cv.Values = []float64{v}
	case string:
		if v == "max" {
			cv.Adaptive = true
			return nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("curve string value must be a number or \"max\", got %q: %w", v, err)
		}
		cv.Values = []float64{f}
	case []interface{}:
		cv.Values = make([]float64, 0, len(v))
		for _, item := range v {
			switch n := item.(type) {
			case int64:
				cv.Values = append(cv.Values, float64(n))
			case float64:
				cv.Values = append(cv.Values, n)
			default:
				return fmt.Errorf("curve array element must be a number, got %T", item)
			}
		}
	case map[string]interface{}:
		// Table form: { random = true, min = 0, max = 100 }
		//          or { base = [1, 50], jitter = 5 }
		//          or { random = true, cap = 100 }
		//          or { adaptive = true, target_pending = 100, max_rate = 5000 }
		if a, ok := v["adaptive"]; ok {
			if ab, ok := a.(bool); ok && ab {
				cv.Adaptive = true
				if tp, ok := v["target_pending"]; ok {
					cv.TargetPending = int(toFloat(tp))
				}
				if mr, ok := v["max_rate"]; ok {
					cv.MaxRate = toFloat(mr)
				}
				return nil
			}
		}
		if r, ok := v["random"]; ok {
			if rb, ok := r.(bool); ok && rb {
				cv.Random = true
			}
		}
		if mn, ok := v["min"]; ok {
			cv.Min = toFloat(mn)
		}
		if mx, ok := v["max"]; ok {
			cv.Max = toFloat(mx)
		}
		// "cap" is syntactic sugar for max with min=0
		if cap, ok := v["cap"]; ok {
			cv.Max = toFloat(cap)
			if cv.Min == 0 && !hasKey(v, "min") {
				cv.Min = 0
			}
			cv.Random = true
		}
		if j, ok := v["jitter"]; ok {
			cv.Jitter = toFloat(j)
		}
		// "base" provides the deterministic curve points
		if base, ok := v["base"]; ok {
			switch b := base.(type) {
			case int64:
				cv.Values = []float64{float64(b)}
			case float64:
				cv.Values = []float64{b}
			case []interface{}:
				cv.Values = make([]float64, 0, len(b))
				for _, item := range b {
					cv.Values = append(cv.Values, toFloat(item))
				}
			}
		}
		// Apply sensible defaults for random mode
		if cv.Random && cv.Min == 0 && cv.Max == 0 {
			cv.Max = 100.0 // default random cap
		}
	default:
		return fmt.Errorf("curve value must be a number, array, or table, got %T", data)
	}
	return nil
}

// ToCurve converts the CurveValue to a Curve for interpolation.
// For adaptive mode this returns a constant seed rate; the runner overrides it.
func (cv *CurveValue) ToCurve() Curve {
	if cv == nil {
		return NewConstantCurve(0)
	}
	if cv.Adaptive {
		return NewConstantCurve(adaptiveSeedRate)
	}
	if cv.Random {
		return NewRandomCurve(cv.Min, cv.Max)
	}
	var c Curve
	if len(cv.Values) == 0 {
		c = NewConstantCurve(0)
	} else {
		c = NewCurve(cv.Values)
	}
	if cv.Jitter > 0 {
		c = c.WithJitter(cv.Jitter)
	}
	return c
}

// toFloat coerces a TOML value to float64.
func toFloat(v interface{}) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	default:
		return 0
	}
}

// hasKey checks if a map contains a given key.
func hasKey(m map[string]interface{}, key string) bool {
	_, ok := m[key]
	return ok
}

// SweepValue is a TOML value that can be a single integer or an array of integers.
// Single values produce one experiment run. Arrays produce multiple runs (sweep).
type SweepValue struct {
	Values []uint64
}

func (sv *SweepValue) UnmarshalTOML(data interface{}) error {
	switch v := data.(type) {
	case int64:
		sv.Values = []uint64{uint64(v)}
	case float64:
		sv.Values = []uint64{uint64(v)}
	case []interface{}:
		sv.Values = make([]uint64, 0, len(v))
		for _, item := range v {
			switch n := item.(type) {
			case int64:
				sv.Values = append(sv.Values, uint64(n))
			case float64:
				sv.Values = append(sv.Values, uint64(n))
			default:
				return fmt.Errorf("sweep array element must be a number, got %T", item)
			}
		}
	default:
		return fmt.Errorf("sweep value must be a number or array, got %T", data)
	}
	return nil
}

// Single returns the single value (or the first value if it's a sweep).
func (sv *SweepValue) Single() uint64 {
	if sv == nil || len(sv.Values) == 0 {
		return 0
	}
	return sv.Values[0]
}

// IsSweep returns true if this value has multiple entries (defines a parameter sweep).
func (sv *SweepValue) IsSweep() bool {
	return sv != nil && len(sv.Values) > 1
}

// LengthMode describes how the simulation length is specified.
type LengthMode int

const (
	LengthTime          LengthMode = iota // wall-clock duration
	LengthDataPoints                      // number of CSV rows
	LengthL1Blocks                        // number of L1 blocks
	LengthL2Blocks                        // number of L2 blocks
	LengthBatcherPosts                    // number of batcher L1 txs
	LengthBatcherBlocks                   // number of L1 blocks containing batcher txs
)

// ParsedLength is a parsed simulation length specification.
type ParsedLength struct {
	Mode     LengthMode
	Duration time.Duration // only valid for LengthTime
	Count    int           // only valid for LengthDataPoints, LengthL1Blocks, LengthL2Blocks
}

var lengthRe = regexp.MustCompile(`^(\d+)\s*(data-points?|l1-blocks?|l2-blocks?|batcher-posts?|batcher-blocks?)$`)

// ParseLength parses a single length string from TOML.
// Supported formats: "10m", "1h30m", "500 data-points", "100 l1-blocks",
// "200 l2-blocks", "50 batcher-posts", "30 batcher-blocks"
func ParseLength(s string) (ParsedLength, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ParsedLength{Mode: LengthTime, Duration: 10 * time.Minute}, nil
	}

	// Try matching count-based formats first
	m := lengthRe.FindStringSubmatch(s)
	if m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return ParsedLength{}, fmt.Errorf("invalid count in length %q: %w", s, err)
		}
		unit := strings.TrimSuffix(m[2], "s") // normalize plural
		switch unit {
		case "data-point":
			return ParsedLength{Mode: LengthDataPoints, Count: n}, nil
		case "l1-block":
			return ParsedLength{Mode: LengthL1Blocks, Count: n}, nil
		case "l2-block":
			return ParsedLength{Mode: LengthL2Blocks, Count: n}, nil
		case "batcher-post":
			return ParsedLength{Mode: LengthBatcherPosts, Count: n}, nil
		case "batcher-block":
			return ParsedLength{Mode: LengthBatcherBlocks, Count: n}, nil
		}
	}

	// Try parsing as a Go duration
	d, err := time.ParseDuration(s)
	if err != nil {
		return ParsedLength{}, fmt.Errorf("invalid length %q: must be a duration (e.g. 10m) or count (e.g. 500 data-points): %w", s, err)
	}
	if d <= 0 {
		return ParsedLength{}, fmt.Errorf("length must be positive, got %v", d)
	}
	return ParsedLength{Mode: LengthTime, Duration: d}, nil
}

// BatcherRunConfig holds the resolved knobs for a single experiment run,
// including both batcher configuration and chain-level parameters.
type BatcherRunConfig struct {
	MaxL1TxSize           uint64
	MaxChannelDuration    uint64
	MaxBlocksPerSpanBatch uint64
	MaxPendingTx          uint64
	BatchType             uint
	CompressionAlgo       string
	SubSafetyMargin       uint64

	GasLimit  uint64
	BlockTime uint64
}

// ExperimentRun pairs batcher/chain knobs with an optional per-run simple_tx rate.
// When SimpleTxRate is nil, the resolved simulation TxRate curve is used.
// When non-nil, that run uses a constant simple tx rate of *SimpleTxRate tx/s.
type ExperimentRun struct {
	Batcher      BatcherRunConfig
	SimpleTxRate *float64
}

// ExpandSweep computes the cartesian product of all batcher and chain sweep
// values, producing one BatcherRunConfig per experiment run. Runs are sorted
// with block_time as the outermost dimension to minimise rollup-node restarts.
func ExpandSweep(b BatcherKnobs, c ChainKnobs) []BatcherRunConfig {
	batchType := uint(0) // SingularBatch default
	if b.BatchType != nil {
		batchType = *b.BatchType
	}
	comprAlgo := "zlib"
	if b.CompressionAlgo != "" {
		comprAlgo = b.CompressionAlgo
	}
	subSafety := uint64(10)
	if b.SubSafetyMargin != nil {
		subSafety = *b.SubSafetyMargin
	}

	txSizes := ensureValues(b.MaxL1TxSize.Values, 120000)
	chanDurs := ensureValues(b.MaxChannelDuration.Values, 0)
	spanBatch := ensureValues(b.MaxBlocksPerSpanBatch.Values, 0)
	pendTx := ensureValues(b.MaxPendingTx.Values, 1)
	gasLimits := ensureValues(c.GasLimit.Values, 0)
	blockTimes := ensureValues(c.BlockTime.Values, 0)

	var runs []BatcherRunConfig
	for _, bt := range blockTimes {
		for _, gl := range gasLimits {
			for _, ts := range txSizes {
				for _, cd := range chanDurs {
					for _, sb := range spanBatch {
						for _, pt := range pendTx {
							runs = append(runs, BatcherRunConfig{
								MaxL1TxSize:           ts,
								MaxChannelDuration:    cd,
								MaxBlocksPerSpanBatch: sb,
								MaxPendingTx:          pt,
								BatchType:             batchType,
								CompressionAlgo:       comprAlgo,
								SubSafetyMargin:       subSafety,
								GasLimit:              gl,
								BlockTime:             bt,
							})
						}
					}
				}
			}
		}
	}
	return runs
}

// ExpandExperimentRuns combines the batcher/chain sweep with optional simple_tx
// rate_runs. When rateRuns is empty, returns one ExperimentRun per batcher config
// (SimpleTxRate nil). When rateRuns is non-empty, returns the cartesian product
// batchers × rateRuns with each SimpleTxRate set to the corresponding value.
func ExpandExperimentRuns(batchers []BatcherRunConfig, rateRuns []float64) []ExperimentRun {
	if len(rateRuns) == 0 {
		out := make([]ExperimentRun, len(batchers))
		for i, b := range batchers {
			out[i] = ExperimentRun{Batcher: b, SimpleTxRate: nil}
		}
		return out
	}
	out := make([]ExperimentRun, 0, len(batchers)*len(rateRuns))
	for _, b := range batchers {
		for i := range rateRuns {
			out = append(out, ExperimentRun{Batcher: b, SimpleTxRate: &rateRuns[i]})
		}
	}
	return out
}

// ExpandBatcherSweep is a backward-compatible wrapper that expands only batcher
// knobs (no chain-level sweep). Use ExpandSweep for the full cartesian product.
func ExpandBatcherSweep(b BatcherKnobs) []BatcherRunConfig {
	return ExpandSweep(b, ChainKnobs{})
}

// ensureValues returns values if non-empty, otherwise a slice with the default.
func ensureValues(vals []uint64, def uint64) []uint64 {
	if len(vals) == 0 {
		return []uint64{def}
	}
	return vals
}

// ValidateBatcherRun checks a BatcherRunConfig against the rollup fork schedule.
// It returns a list of error reasons for configurations that are unsupported.
// An empty slice means the run is fully valid.
// Warnings (non-fatal issues like no-op settings) are returned separately.
func ValidateBatcherRun(run BatcherRunConfig, rollup *monitor.RollupConfig) (errors []string, warnings []string) {
	// SpanBatch (batch_type=1) requires the Delta upgrade.
	if run.BatchType == 1 && !rollup.IsForkActive(rollup.DeltaTime) {
		errors = append(errors, "batch_type=1 (SpanBatch) requires the Delta upgrade, which is not active")
	}

	// Brotli compression requires the Fjord upgrade.
	if strings.Contains(run.CompressionAlgo, "brotli") && !rollup.IsForkActive(rollup.FjordTime) {
		errors = append(errors, fmt.Sprintf("compression_algo=%q requires the Fjord upgrade, which is not active", run.CompressionAlgo))
	}

	// max_blocks_per_span_batch has no effect with SingularBatch.
	if run.MaxBlocksPerSpanBatch > 0 && run.BatchType == 0 {
		warnings = append(warnings, "max_blocks_per_span_batch has no effect when batch_type=0 (SingularBatch)")
	}

	return errors, warnings
}

// FilterBatcherRuns validates each run against the rollup fork schedule and
// returns only the runs with supported configurations. Skipped runs are logged
// as warnings; no-op configuration issues are logged at info level.
func FilterBatcherRuns(runs []BatcherRunConfig, rollup *monitor.RollupConfig, logger *slog.Logger) []BatcherRunConfig {
	var valid []BatcherRunConfig
	for i, run := range runs {
		errs, warns := ValidateBatcherRun(run, rollup)

		for _, w := range warns {
			logger.Info("Batcher run config warning",
				"run", i+1,
				"warning", w,
			)
		}

		if len(errs) > 0 {
			for _, e := range errs {
				logger.Warn("Skipping unsupported batcher run",
					"run", i+1,
					"reason", e,
					"batch_type", run.BatchType,
					"compression_algo", run.CompressionAlgo,
				)
			}
			continue
		}

		valid = append(valid, run)
	}
	return valid
}

// LoadExperimentConfig reads and parses a TOML experiment config file.
func LoadExperimentConfig(path string) (*ExperimentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read experiment config %s: %w", path, err)
	}
	var cfg ExperimentConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse experiment config %s: %w", path, err)
	}

	// Apply defaults
	if cfg.Target.L2RPC == "" {
		cfg.Target.L2RPC = "http://127.0.0.1:8545"
	}
	if cfg.Target.NodeRPC == "" {
		cfg.Target.NodeRPC = "http://127.0.0.1:9545"
	}
	if len(cfg.Simulation.Length.Raw) == 0 {
		cfg.Simulation.Length.Raw = []string{"10m"}
	}
	if cfg.Simulation.SampleInterval == "" {
		cfg.Simulation.SampleInterval = "2s"
	}
	if cfg.Simulation.Accounts <= 0 {
		cfg.Simulation.Accounts = 1
	}

	// batcher_metrics_url can live in either [batcher] or [target]; prefer [batcher].
	if cfg.Batcher.BatcherMetricsURL != "" && cfg.Target.BatcherMetricsURL == "" {
		cfg.Target.BatcherMetricsURL = cfg.Batcher.BatcherMetricsURL
	}

	return &cfg, nil
}

// nodeBatcherTOML is a partial parse of the rollup-node TOML, extracting
// only the [batcher] section fields relevant to experiment sweep defaults.
type nodeBatcherTOML struct {
	Batcher struct {
		MaxL1TxSize           *uint64 `toml:"max_l1_tx_size"`
		MaxChannelDuration    *uint64 `toml:"max_channel_duration"`
		MaxBlocksPerSpanBatch *uint64 `toml:"max_blocks_per_span_batch"`
		MaxPendingTx          *uint64 `toml:"max_pending_transactions"`
		BatchType             *uint   `toml:"batch_type"`
		CompressionAlgo       string  `toml:"compression_algo"`
		SubSafetyMargin       *uint64 `toml:"sub_safety_margin"`
	} `toml:"batcher"`
}

// ReadBatcherFromNodeConfig parses the rollup-node TOML config and returns
// BatcherKnobs populated with only the explicitly set batcher values.
func ReadBatcherFromNodeConfig(path string) (BatcherKnobs, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return BatcherKnobs{}, fmt.Errorf("read node config %s: %w", path, err)
	}
	var nc nodeBatcherTOML
	if err := toml.Unmarshal(data, &nc); err != nil {
		return BatcherKnobs{}, fmt.Errorf("parse node config %s: %w", path, err)
	}
	var bk BatcherKnobs
	if nc.Batcher.MaxL1TxSize != nil {
		bk.MaxL1TxSize.Values = []uint64{*nc.Batcher.MaxL1TxSize}
	}
	if nc.Batcher.MaxChannelDuration != nil {
		bk.MaxChannelDuration.Values = []uint64{*nc.Batcher.MaxChannelDuration}
	}
	if nc.Batcher.MaxBlocksPerSpanBatch != nil {
		bk.MaxBlocksPerSpanBatch.Values = []uint64{*nc.Batcher.MaxBlocksPerSpanBatch}
	}
	if nc.Batcher.MaxPendingTx != nil {
		bk.MaxPendingTx.Values = []uint64{*nc.Batcher.MaxPendingTx}
	}
	bk.BatchType = nc.Batcher.BatchType
	if nc.Batcher.CompressionAlgo != "" {
		bk.CompressionAlgo = nc.Batcher.CompressionAlgo
	}
	bk.SubSafetyMargin = nc.Batcher.SubSafetyMargin
	return bk, nil
}

// FillFromNodeConfig populates unconfigured fields in b with values from
// the rollup-node config. Only fields that are empty/nil in b are filled.
func (b *BatcherKnobs) FillFromNodeConfig(nc BatcherKnobs) {
	if len(b.MaxL1TxSize.Values) == 0 && len(nc.MaxL1TxSize.Values) > 0 {
		b.MaxL1TxSize.Values = nc.MaxL1TxSize.Values
	}
	if len(b.MaxChannelDuration.Values) == 0 && len(nc.MaxChannelDuration.Values) > 0 {
		b.MaxChannelDuration.Values = nc.MaxChannelDuration.Values
	}
	if len(b.MaxBlocksPerSpanBatch.Values) == 0 && len(nc.MaxBlocksPerSpanBatch.Values) > 0 {
		b.MaxBlocksPerSpanBatch.Values = nc.MaxBlocksPerSpanBatch.Values
	}
	if len(b.MaxPendingTx.Values) == 0 && len(nc.MaxPendingTx.Values) > 0 {
		b.MaxPendingTx.Values = nc.MaxPendingTx.Values
	}
	if b.BatchType == nil && nc.BatchType != nil {
		b.BatchType = nc.BatchType
	}
	if b.CompressionAlgo == "" && nc.CompressionAlgo != "" {
		b.CompressionAlgo = nc.CompressionAlgo
	}
	if b.SubSafetyMargin == nil && nc.SubSafetyMargin != nil {
		b.SubSafetyMargin = nc.SubSafetyMargin
	}
}

// ResolvedSimConfig holds the fully parsed simulation configuration.
type ResolvedSimConfig struct {
	Length          []ParsedLength
	Accounts        int
	OnlyMonologues  bool
	SampleInterval  time.Duration
	SampleOnL1Block bool
	DrainOnExit     bool

	TxRateAdaptive      bool
	TxRateTargetPending int
	TxRateMaxRate       float64

	TxRate             Curve
	TxValue            Curve
	TxCalldataSize     int
	ERC20TxRate        Curve
	ERC20TxValue       Curve
	SwapTxRate         Curve
	SwapTxValue        Curve
	SwapRouter         string
	SwapTokenA         string
	SwapTokenB         string
	DepositRBTCRate    Curve
	DepositRBTCValue   Curve
	DepositERC20Rate   Curve
	DepositERC20Value  Curve
	WithdrawRBTCRate   Curve
	WithdrawRBTCValue  Curve
	WithdrawERC20Rate  Curve
	WithdrawERC20Value Curve
	ERC20ContractL1    string
	ERC20ContractL2    string
}

// WithSimpleTxRate returns a shallow copy of the config with a constant simple_tx
// rate and adaptive rate mode disabled. Used for rate_runs experiment expansion.
func (s *ResolvedSimConfig) WithSimpleTxRate(rate float64) *ResolvedSimConfig {
	out := *s
	out.TxRate = NewConstantCurve(rate)
	out.TxRateAdaptive = false
	out.TxRateTargetPending = 0
	out.TxRateMaxRate = 0
	return &out
}

// ResolveSimulation parses the simulation config into resolved form.
func ResolveSimulation(sim SimulationConfig) (*ResolvedSimConfig, error) {
	rawSpecs := sim.Length.Raw
	if len(rawSpecs) == 0 {
		rawSpecs = []string{"10m"}
	}
	var lengths []ParsedLength
	for _, s := range rawSpecs {
		pl, err := ParseLength(s)
		if err != nil {
			return nil, fmt.Errorf("simulation.length %q: %w", s, err)
		}
		lengths = append(lengths, pl)
	}

	var sampleInterval time.Duration
	var sampleOnL1Block bool
	if sim.SampleInterval == "l1_block" {
		sampleOnL1Block = true
		sampleInterval = 2 * time.Second // internal tick cadence for curves/bridges
	} else {
		var parseErr error
		sampleInterval, parseErr = time.ParseDuration(sim.SampleInterval)
		if parseErr != nil || sampleInterval <= 0 {
			sampleInterval = 2 * time.Second
		}
	}

	txRate := sim.SimpleTx.Rate.ToCurve()
	txValue := sim.SimpleTx.Value.ToCurve()

	txRateAdaptive := sim.SimpleTx.Rate.IsAdaptive()
	txRateTargetPending := sim.SimpleTx.Rate.TargetPending
	txRateMaxRate := sim.SimpleTx.Rate.MaxRate
	if txRateAdaptive {
		if txRateTargetPending <= 0 {
			txRateTargetPending = adaptiveDefaultTargetPending
		}
		if txRateMaxRate <= 0 {
			txRateMaxRate = adaptiveDefaultMaxRate
		}
	}

	if len(sim.SimpleTx.RateRuns) > 0 {
		if sim.SimpleTx.Rate.IsAdaptive() {
			return nil, fmt.Errorf(`simulation.simple_tx: "rate_runs" cannot be used with adaptive rate ("max" / adaptive = true)`)
		}
		if sim.SimpleTx.Rate.Random {
			return nil, fmt.Errorf(`simulation.simple_tx: "rate_runs" cannot be used with random rate`)
		}
		if sim.SimpleTx.Rate.Jitter > 0 {
			return nil, fmt.Errorf(`simulation.simple_tx: "rate_runs" cannot be used with jitter`)
		}
		if len(sim.SimpleTx.Rate.Values) > 1 {
			return nil, fmt.Errorf(`simulation.simple_tx: "rate_runs" cannot be combined with a multi-point rate array`)
		}
		if len(sim.SimpleTx.Rate.Values) == 1 && sim.SimpleTx.Rate.Values[0] != 0 {
			return nil, fmt.Errorf(`simulation.simple_tx: when "rate_runs" is set, leave "rate" unset or 0 (got %g); use only rate_runs for per-run rates`, sim.SimpleTx.Rate.Values[0])
		}
		for _, r := range sim.SimpleTx.RateRuns {
			if r < 0 {
				return nil, fmt.Errorf(`simulation.simple_tx: rate_runs values must be >= 0, got %g`, r)
			}
		}
		txRate = NewConstantCurve(sim.SimpleTx.RateRuns[0])
		txRateAdaptive = false
		txRateTargetPending = 0
		txRateMaxRate = 0
	}

	erc20TxRate := sim.ERC20Tx.Rate.ToCurve()
	erc20TxValue := sim.ERC20Tx.Value.ToCurve()
	if len(sim.ERC20Tx.Value.Values) == 0 && !sim.ERC20Tx.Value.Random {
		erc20TxValue = NewConstantCurve(1)
	}

	swapTxRate := sim.SwapTx.Rate.ToCurve()
	swapTxValue := sim.SwapTx.Value.ToCurve()
	if len(sim.SwapTx.Value.Values) == 0 && !sim.SwapTx.Value.Random {
		swapTxValue = NewConstantCurve(1)
	}

	depositRBTCRate := sim.DepositRBTC.Rate.ToCurve()
	depositRBTCValue := sim.DepositRBTC.Value.ToCurve()
	if len(sim.DepositRBTC.Value.Values) == 0 && !sim.DepositRBTC.Value.Random {
		depositRBTCValue = NewConstantCurve(0.001)
	}

	depositERC20Rate := sim.DepositERC20.Rate.ToCurve()
	depositERC20Value := sim.DepositERC20.Value.ToCurve()
	if len(sim.DepositERC20.Value.Values) == 0 && !sim.DepositERC20.Value.Random {
		depositERC20Value = NewConstantCurve(0.001)
	}

	withdrawRBTCRate := sim.WithdrawRBTC.Rate.ToCurve()
	withdrawRBTCValue := sim.WithdrawRBTC.Value.ToCurve()
	if len(sim.WithdrawRBTC.Value.Values) == 0 && !sim.WithdrawRBTC.Value.Random {
		withdrawRBTCValue = NewConstantCurve(0.001)
	}

	withdrawERC20Rate := sim.WithdrawERC20.Rate.ToCurve()
	withdrawERC20Value := sim.WithdrawERC20.Value.ToCurve()
	if len(sim.WithdrawERC20.Value.Values) == 0 && !sim.WithdrawERC20.Value.Random {
		withdrawERC20Value = NewConstantCurve(0.001)
	}

	// ERC20 contract addresses: withdrawal sections can also specify contracts;
	// prefer deposit section addresses, fall back to withdrawal section.
	erc20L1 := sim.DepositERC20.ERC20ContractL1
	if erc20L1 == "" {
		erc20L1 = sim.WithdrawERC20.ERC20ContractL1
	}
	erc20L2 := sim.DepositERC20.ERC20ContractL2
	if erc20L2 == "" {
		erc20L2 = sim.WithdrawERC20.ERC20ContractL2
	}

	drainOnExit := true
	if sim.DrainOnExit != nil {
		drainOnExit = *sim.DrainOnExit
	}

	return &ResolvedSimConfig{
		Length:              lengths,
		Accounts:            sim.Accounts,
		OnlyMonologues:      sim.OnlyMonologues,
		SampleInterval:      sampleInterval,
		SampleOnL1Block:     sampleOnL1Block,
		DrainOnExit:         drainOnExit,
		TxRateAdaptive:      txRateAdaptive,
		TxRateTargetPending: txRateTargetPending,
		TxRateMaxRate:       txRateMaxRate,
		TxRate:              txRate,
		TxValue:             txValue,
		TxCalldataSize:      sim.SimpleTx.CalldataSize,
		ERC20TxRate:         erc20TxRate,
		ERC20TxValue:        erc20TxValue,
		SwapTxRate:          swapTxRate,
		SwapTxValue:         swapTxValue,
		SwapRouter:          sim.SwapTx.Router,
		SwapTokenA:          sim.SwapTx.TokenA,
		SwapTokenB:          sim.SwapTx.TokenB,
		DepositRBTCRate:     depositRBTCRate,
		DepositRBTCValue:    depositRBTCValue,
		DepositERC20Rate:    depositERC20Rate,
		DepositERC20Value:   depositERC20Value,
		WithdrawRBTCRate:    withdrawRBTCRate,
		WithdrawRBTCValue:   withdrawRBTCValue,
		WithdrawERC20Rate:   withdrawERC20Rate,
		WithdrawERC20Value:  withdrawERC20Value,
		ERC20ContractL1:     erc20L1,
		ERC20ContractL2:     erc20L2,
	}, nil
}
