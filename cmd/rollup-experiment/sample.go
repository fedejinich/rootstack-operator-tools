package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SampleKnobs holds the simulation input values (knobs) for a single CSV sample.
// These are either fixed per-run (batcher config, accounts) or dynamic per-tick
// (tx_rate, tx_value, deposit/withdraw values).
type SampleKnobs struct {
	TxRate                float64
	TxValue               float64
	Accounts              int
	DepositRBTCRateS      float64
	DepositRBTCValue      float64
	DepositERC20RateS     float64
	DepositERC20Value     float64
	WithdrawRBTCRateS     float64
	WithdrawRBTCValue     float64
	WithdrawERC20RateS    float64
	WithdrawERC20Value    float64
	MaxL1TxSize           uint64
	MaxChannelDuration    uint64
	MaxBlocksPerSpanBatch uint64
	MaxPendingTx          uint64
	GasLimit              uint64
	BlockTime             uint64
	CleanStart            bool   // true if run started with fully drained batcher (sequencer paused + full drain)
	StartUnsafeBlocks     uint64 // L2 unsafe head block number at run start
}

// SampleMeasurements holds the observed output values from the metrics store.
type SampleMeasurements struct {
	L2Block                uint64
	L1Block                uint64
	TPS                    float64
	L2TxCostRBTC           float64
	L2TxCostMinRBTC        float64
	L2TxCostMaxRBTC        float64
	L2TxSpeedS             float64
	L2TxCount              uint64
	L1TxCount              uint64
	PostingFreqS           float64
	Deposits               uint64
	Withdrawals            uint64
	TotalL2Txs             uint64
	TotalDeposits          uint64
	TotalWithdrawals       uint64
	SafeHeadLag            uint64
	UnsafeL2               uint64
	SafeL2                 uint64
	BatcherPostCostRBTC    float64
	BatcherL1GasSpendWei   float64 // gasUsed * effectiveGasPrice for last batcher L1 tx (wei)
	BatcherL1GasPriceWei   float64 // effective gas price for that tx (wei)
	BatcherDataSizeBytes   uint64
	CompressionRatio       float64
	CompressionRatioDelta  float64
	TxPoolPending          uint64
	TxPoolQueued           uint64
	BatcherPendingBlocks   float64
	BatcherPendingBytes    float64
	BatcherL1BaseFeeGwei   float64
	BatcherBalanceRBTC     float64
	BatcherChannelTimeouts float64
	BatcherTxFailed        float64
	BatcherGasBumps        float64
	TotalBatcherPosts      uint64
	TotalBatcherBlocks     uint64
	TotalBatcherCostRBTC   float64
	TotalBatcherDataBytes  float64
	TotalL2TxsFinalized    uint64
	L2BlocksPerBatch       float64
	L2TxsPerBatch          float64
	L1TxsPerBatch          float64
	L2ToL1Throughput       float64
	L2ToL1Latency          float64
	ChannelOpenL1Blocks    float64
}

// DerivedMetrics holds values computed from cumulative measurements.
type DerivedMetrics struct {
	AmortizedL1CostPerL2Tx  float64
	TotalTxCostRBTC         float64
	TotalTxCostMinRBTC      float64
	TotalTxCostMaxRBTC      float64
	CompressedDataPerTxKB   float64
	RawDataPerTxKB          float64
	NormalizedTPS           float64
	DataThroughputBytesPerS float64
}

// ExperimentSample represents a single row of experiment CSV data,
// combining run metadata, simulation knobs, measured outputs, and derived metrics.
type ExperimentSample struct {
	RunID     int
	TSeconds  float64
	Timestamp time.Time

	Knobs        SampleKnobs
	Measurements SampleMeasurements
	Derived      DerivedMetrics
}

// ExperimentSampleCSVHeader returns the canonical CSV column ordering.
func ExperimentSampleCSVHeader() []string {
	return []string{
		// Knob columns
		"run_id",
		"t_seconds",
		"tx_rate",
		"tx_value",
		"accounts",
		"deposit_rbtc_rate_s",
		"deposit_rbtc_value",
		"deposit_erc20_rate_s",
		"deposit_erc20_value",
		"withdraw_rbtc_rate_s",
		"withdraw_rbtc_value",
		"withdraw_erc20_rate_s",
		"withdraw_erc20_value",
		"max_l1_tx_size",
		"max_channel_duration",
		"max_blocks_per_span_batch",
		"max_pending_tx",
		"gas_limit",
		"block_time",
		"clean_start",
		"start_unsafe_blocks",
		// Measurement columns
		"timestamp",
		"l2_block",
		"l1_block",
		"tps",
		"l2_tx_cost_rbtc",
		"l2_tx_cost_min_rbtc",
		"l2_tx_cost_max_rbtc",
		"l2_tx_speed_s",
		"l2_tx_count",
		"l1_tx_count",
		"posting_freq_s",
		"deposits",
		"withdrawals",
		"total_l2_txs",
		"total_deposits",
		"total_withdrawals",
		"safe_head_lag",
		"unsafe_l2",
		"safe_l2",
		"batcher_post_cost_rbtc",
		"batcher_l1_gas_spend_wei",
		"batcher_l1_gas_price_wei",
		"batcher_data_size_bytes",
		"compression_ratio",
		"compression_ratio_delta",
		"txpool_pending",
		"txpool_queued",
		"batcher_pending_blocks",
		"batcher_pending_bytes",
		"batcher_l1_basefee_gwei",
		"batcher_balance_rbtc",
		"batcher_channel_timeouts",
		"batcher_tx_failed",
		"batcher_gas_bumps",
		"total_batcher_posts",
		"total_batcher_blocks",
		"total_batcher_cost_rbtc",
		"total_batcher_data_bytes",
		"total_l2_txs_finalized",
		"l2_blocks_per_batch",
		"l2_txs_per_batch",
		"l1_txs_per_batch",
		"l2_to_l1_throughput",
		"l2_to_l1_latency",
		"channel_open_l1_blocks",
		// Derived columns
		"amortized_l1_cost_per_l2_tx",
		"total_tx_cost_rbtc",
		"total_tx_cost_min_rbtc",
		"total_tx_cost_max_rbtc",
		"compressed_data_per_tx_kb",
		"raw_data_per_tx_kb",
		"normalized_tps",
		"data_throughput_bytes_per_s",
	}
}

// ToCSVRow serializes the sample to a CSV row with the canonical formatting.
func (s *ExperimentSample) ToCSVRow() []string {
	return []string{
		// Knob columns
		strconv.Itoa(s.RunID),
		fmt.Sprintf("%.2f", s.TSeconds),
		fmt.Sprintf("%g", s.Knobs.TxRate),
		rbtcStr(s.Knobs.TxValue),
		strconv.Itoa(s.Knobs.Accounts),
		fmt.Sprintf("%.4f", s.Knobs.DepositRBTCRateS),
		rbtcStr(s.Knobs.DepositRBTCValue),
		fmt.Sprintf("%.4f", s.Knobs.DepositERC20RateS),
		rbtcStr(s.Knobs.DepositERC20Value),
		fmt.Sprintf("%.4f", s.Knobs.WithdrawRBTCRateS),
		rbtcStr(s.Knobs.WithdrawRBTCValue),
		fmt.Sprintf("%.4f", s.Knobs.WithdrawERC20RateS),
		rbtcStr(s.Knobs.WithdrawERC20Value),
		strconv.FormatUint(s.Knobs.MaxL1TxSize, 10),
		strconv.FormatUint(s.Knobs.MaxChannelDuration, 10),
		strconv.FormatUint(s.Knobs.MaxBlocksPerSpanBatch, 10),
		strconv.FormatUint(s.Knobs.MaxPendingTx, 10),
		strconv.FormatUint(s.Knobs.GasLimit, 10),
		strconv.FormatUint(s.Knobs.BlockTime, 10),
		strconv.FormatBool(s.Knobs.CleanStart),
		strconv.FormatUint(s.Knobs.StartUnsafeBlocks, 10),
		// Measurement columns
		s.Timestamp.Format("2006-01-02T15:04:05.000Z"),
		fmt.Sprintf("%.0f", float64(s.Measurements.L2Block)),
		fmt.Sprintf("%.0f", float64(s.Measurements.L1Block)),
		fmt.Sprintf("%.4f", s.Measurements.TPS),
		fmt.Sprintf("%.12f", s.Measurements.L2TxCostRBTC),
		fmt.Sprintf("%.12f", s.Measurements.L2TxCostMinRBTC),
		fmt.Sprintf("%.12f", s.Measurements.L2TxCostMaxRBTC),
		fmt.Sprintf("%.4f", s.Measurements.L2TxSpeedS),
		fmt.Sprintf("%.0f", float64(s.Measurements.L2TxCount)),
		fmt.Sprintf("%.0f", float64(s.Measurements.L1TxCount)),
		fmt.Sprintf("%.2f", s.Measurements.PostingFreqS),
		fmt.Sprintf("%.0f", float64(s.Measurements.Deposits)),
		fmt.Sprintf("%.0f", float64(s.Measurements.Withdrawals)),
		fmt.Sprintf("%.0f", float64(s.Measurements.TotalL2Txs)),
		fmt.Sprintf("%.0f", float64(s.Measurements.TotalDeposits)),
		fmt.Sprintf("%.0f", float64(s.Measurements.TotalWithdrawals)),
		fmt.Sprintf("%.0f", float64(s.Measurements.SafeHeadLag)),
		fmt.Sprintf("%.0f", float64(s.Measurements.UnsafeL2)),
		fmt.Sprintf("%.0f", float64(s.Measurements.SafeL2)),
		fmt.Sprintf("%.12f", s.Measurements.BatcherPostCostRBTC),
		fmt.Sprintf("%.0f", s.Measurements.BatcherL1GasSpendWei),
		fmt.Sprintf("%.0f", s.Measurements.BatcherL1GasPriceWei),
		fmt.Sprintf("%.0f", float64(s.Measurements.BatcherDataSizeBytes)),
		fmt.Sprintf("%.4f", s.Measurements.CompressionRatio),
		fmt.Sprintf("%.4f", s.Measurements.CompressionRatioDelta),
		fmt.Sprintf("%.0f", float64(s.Measurements.TxPoolPending)),
		fmt.Sprintf("%.0f", float64(s.Measurements.TxPoolQueued)),
		fmt.Sprintf("%.0f", s.Measurements.BatcherPendingBlocks),
		fmt.Sprintf("%.0f", s.Measurements.BatcherPendingBytes),
		fmt.Sprintf("%.4f", s.Measurements.BatcherL1BaseFeeGwei),
		fmt.Sprintf("%.12f", s.Measurements.BatcherBalanceRBTC),
		fmt.Sprintf("%.0f", s.Measurements.BatcherChannelTimeouts),
		fmt.Sprintf("%.0f", s.Measurements.BatcherTxFailed),
		fmt.Sprintf("%.0f", s.Measurements.BatcherGasBumps),
		fmt.Sprintf("%.0f", float64(s.Measurements.TotalBatcherPosts)),
		fmt.Sprintf("%.0f", float64(s.Measurements.TotalBatcherBlocks)),
		fmt.Sprintf("%.12f", s.Measurements.TotalBatcherCostRBTC),
		fmt.Sprintf("%.0f", s.Measurements.TotalBatcherDataBytes),
		fmt.Sprintf("%.0f", float64(s.Measurements.TotalL2TxsFinalized)),
		fmt.Sprintf("%.0f", s.Measurements.L2BlocksPerBatch),
		fmt.Sprintf("%.0f", s.Measurements.L2TxsPerBatch),
		fmt.Sprintf("%.0f", s.Measurements.L1TxsPerBatch),
		fmt.Sprintf("%.4f", s.Measurements.L2ToL1Throughput),
		fmt.Sprintf("%.2f", s.Measurements.L2ToL1Latency),
		fmt.Sprintf("%.0f", s.Measurements.ChannelOpenL1Blocks),
		// Derived columns
		fmt.Sprintf("%.12f", s.Derived.AmortizedL1CostPerL2Tx),
		fmt.Sprintf("%.12f", s.Derived.TotalTxCostRBTC),
		fmt.Sprintf("%.12f", s.Derived.TotalTxCostMinRBTC),
		fmt.Sprintf("%.12f", s.Derived.TotalTxCostMaxRBTC),
		fmt.Sprintf("%.6f", s.Derived.CompressedDataPerTxKB),
		fmt.Sprintf("%.6f", s.Derived.RawDataPerTxKB),
		fmt.Sprintf("%.6f", s.Derived.NormalizedTPS),
		fmt.Sprintf("%.2f", s.Derived.DataThroughputBytesPerS),
	}
}

// FromMetricValues populates the Measurements fields from a metrics map
// (as returned by MetricsStore.MetricValues()).
func (s *ExperimentSample) FromMetricValues(mv map[string]float64) {
	s.Measurements = SampleMeasurements{
		L2Block:                uint64(mv["latest_l2_block"]),
		L1Block:                uint64(mv["latest_l1_block"]),
		TPS:                    mv["tps"],
		L2TxCostRBTC:           mv["l2_tx_cost_rbtc"],
		L2TxCostMinRBTC:        mv["l2_tx_cost_min_rbtc"],
		L2TxCostMaxRBTC:        mv["l2_tx_cost_max_rbtc"],
		L2TxSpeedS:             mv["l2_tx_speed_s"],
		L2TxCount:              uint64(mv["l2_tx_count"]),
		L1TxCount:              uint64(mv["l1_tx_count"]),
		PostingFreqS:           mv["posting_freq_s"],
		Deposits:               uint64(mv["deposits"]),
		Withdrawals:            uint64(mv["withdrawals"]),
		TotalL2Txs:             uint64(mv["total_l2_txs"]),
		TotalDeposits:          uint64(mv["total_deposits"]),
		TotalWithdrawals:       uint64(mv["total_withdrawals"]),
		SafeHeadLag:            uint64(mv["safe_head_lag"]),
		UnsafeL2:               uint64(mv["unsafe_l2"]),
		SafeL2:                 uint64(mv["safe_l2"]),
		BatcherPostCostRBTC:    mv["batcher_post_cost_rbtc"],
		BatcherL1GasSpendWei:   mv["batcher_l1_gas_spend_wei"],
		BatcherL1GasPriceWei:   mv["batcher_l1_gas_price_wei"],
		BatcherDataSizeBytes:   uint64(mv["batcher_data_size_bytes"]),
		CompressionRatio:       mv["compression_ratio"],
		CompressionRatioDelta:  mv["compression_ratio_delta"],
		TxPoolPending:          uint64(mv["txpool_pending"]),
		TxPoolQueued:           uint64(mv["txpool_queued"]),
		BatcherPendingBlocks:   mv["batcher_pending_blocks"],
		BatcherPendingBytes:    mv["batcher_pending_bytes"],
		BatcherL1BaseFeeGwei:   mv["batcher_l1_basefee_gwei"],
		BatcherBalanceRBTC:     mv["batcher_balance_rbtc"],
		BatcherChannelTimeouts: mv["batcher_channel_timeouts"],
		BatcherTxFailed:        mv["batcher_tx_failed"],
		BatcherGasBumps:        mv["batcher_gas_bumps"],
		TotalBatcherPosts:      uint64(mv["total_batcher_posts"]),
		TotalBatcherBlocks:     uint64(mv["total_batcher_blocks"]),
		TotalBatcherCostRBTC:   mv["total_batcher_cost_rbtc"],
		TotalBatcherDataBytes:  mv["total_batcher_data_bytes"],
		TotalL2TxsFinalized:    uint64(mv["total_l2_txs_finalized"]),
		L2BlocksPerBatch:       mv["l2_blocks_per_batch"],
		L2TxsPerBatch:          mv["l2_txs_per_batch"],
		L1TxsPerBatch:          mv["l1_txs_per_batch"],
		L2ToL1Throughput:       mv["l2_to_l1_throughput"],
		L2ToL1Latency:          mv["l2_to_l1_latency"],
		ChannelOpenL1Blocks:    mv["channel_open_l1_blocks"],
	}
}

// FromCSVRow parses a CSV row into the sample, using the header to map columns.
func (s *ExperimentSample) FromCSVRow(header []string, row []string) error {
	idx := make(map[string]int, len(header))
	for i, col := range header {
		idx[col] = i
	}

	get := func(col string) string {
		if i, ok := idx[col]; ok && i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}
	getFloat := func(col string) float64 {
		v, _ := strconv.ParseFloat(get(col), 64)
		return v
	}
	getUint := func(col string) uint64 {
		v, _ := strconv.ParseUint(get(col), 10, 64)
		return v
	}
	getInt := func(col string) int {
		v, _ := strconv.Atoi(get(col))
		return v
	}
	getBool := func(col string) bool {
		v, _ := strconv.ParseBool(get(col))
		return v
	}

	s.RunID = getInt("run_id")
	s.TSeconds = getFloat("t_seconds")

	if ts := get("timestamp"); ts != "" {
		t, err := time.Parse("2006-01-02T15:04:05.000Z", ts)
		if err != nil {
			return fmt.Errorf("parse timestamp %q: %w", ts, err)
		}
		s.Timestamp = t
	}

	s.Knobs = SampleKnobs{
		TxRate:                getFloat("tx_rate"),
		TxValue:               getFloat("tx_value"),
		Accounts:              getInt("accounts"),
		DepositRBTCRateS:      getFloat("deposit_rbtc_rate_s"),
		DepositRBTCValue:      getFloat("deposit_rbtc_value"),
		DepositERC20RateS:     getFloat("deposit_erc20_rate_s"),
		DepositERC20Value:     getFloat("deposit_erc20_value"),
		WithdrawRBTCRateS:     getFloat("withdraw_rbtc_rate_s"),
		WithdrawRBTCValue:     getFloat("withdraw_rbtc_value"),
		WithdrawERC20RateS:    getFloat("withdraw_erc20_rate_s"),
		WithdrawERC20Value:    getFloat("withdraw_erc20_value"),
		MaxL1TxSize:           getUint("max_l1_tx_size"),
		MaxChannelDuration:    getUint("max_channel_duration"),
		MaxBlocksPerSpanBatch: getUint("max_blocks_per_span_batch"),
		MaxPendingTx:          getUint("max_pending_tx"),
		GasLimit:              getUint("gas_limit"),
		BlockTime:             getUint("block_time"),
		CleanStart:            getBool("clean_start"),
		StartUnsafeBlocks:     getUint("start_unsafe_blocks"),
	}

	s.Measurements = SampleMeasurements{
		L2Block:                uint64(getFloat("l2_block")),
		L1Block:                uint64(getFloat("l1_block")),
		TPS:                    getFloat("tps"),
		L2TxCostRBTC:           getFloat("l2_tx_cost_rbtc"),
		L2TxCostMinRBTC:        getFloat("l2_tx_cost_min_rbtc"),
		L2TxCostMaxRBTC:        getFloat("l2_tx_cost_max_rbtc"),
		L2TxSpeedS:             getFloat("l2_tx_speed_s"),
		L2TxCount:              uint64(getFloat("l2_tx_count")),
		L1TxCount:              uint64(getFloat("l1_tx_count")),
		PostingFreqS:           getFloat("posting_freq_s"),
		Deposits:               uint64(getFloat("deposits")),
		Withdrawals:            uint64(getFloat("withdrawals")),
		TotalL2Txs:             uint64(getFloat("total_l2_txs")),
		TotalDeposits:          uint64(getFloat("total_deposits")),
		TotalWithdrawals:       uint64(getFloat("total_withdrawals")),
		SafeHeadLag:            uint64(getFloat("safe_head_lag")),
		UnsafeL2:               uint64(getFloat("unsafe_l2")),
		SafeL2:                 uint64(getFloat("safe_l2")),
		BatcherPostCostRBTC:    getFloat("batcher_post_cost_rbtc"),
		BatcherL1GasSpendWei:   getFloat("batcher_l1_gas_spend_wei"),
		BatcherL1GasPriceWei:   getFloat("batcher_l1_gas_price_wei"),
		BatcherDataSizeBytes:   uint64(getFloat("batcher_data_size_bytes")),
		CompressionRatio:       getFloat("compression_ratio"),
		CompressionRatioDelta:  getFloat("compression_ratio_delta"),
		TxPoolPending:          uint64(getFloat("txpool_pending")),
		TxPoolQueued:           uint64(getFloat("txpool_queued")),
		BatcherPendingBlocks:   getFloat("batcher_pending_blocks"),
		BatcherPendingBytes:    getFloat("batcher_pending_bytes"),
		BatcherL1BaseFeeGwei:   getFloat("batcher_l1_basefee_gwei"),
		BatcherBalanceRBTC:     getFloat("batcher_balance_rbtc"),
		BatcherChannelTimeouts: getFloat("batcher_channel_timeouts"),
		BatcherTxFailed:        getFloat("batcher_tx_failed"),
		BatcherGasBumps:        getFloat("batcher_gas_bumps"),
		TotalBatcherPosts:      getUint("total_batcher_posts"),
		TotalBatcherBlocks:     getUint("total_batcher_blocks"),
		TotalBatcherCostRBTC:   getFloat("total_batcher_cost_rbtc"),
		TotalBatcherDataBytes:  getFloat("total_batcher_data_bytes"),
		TotalL2TxsFinalized:    getUint("total_l2_txs_finalized"),
		L2BlocksPerBatch:       getFloat("l2_blocks_per_batch"),
		L2TxsPerBatch:          getFloat("l2_txs_per_batch"),
		L1TxsPerBatch:          getFloat("l1_txs_per_batch"),
		L2ToL1Throughput:       getFloat("l2_to_l1_throughput"),
		L2ToL1Latency:          getFloat("l2_to_l1_latency"),
		ChannelOpenL1Blocks:    getFloat("channel_open_l1_blocks"),
	}

	s.Derived = DerivedMetrics{
		AmortizedL1CostPerL2Tx:  getFloat("amortized_l1_cost_per_l2_tx"),
		TotalTxCostRBTC:         getFloat("total_tx_cost_rbtc"),
		TotalTxCostMinRBTC:      getFloat("total_tx_cost_min_rbtc"),
		TotalTxCostMaxRBTC:      getFloat("total_tx_cost_max_rbtc"),
		CompressedDataPerTxKB:   getFloat("compressed_data_per_tx_kb"),
		RawDataPerTxKB:          getFloat("raw_data_per_tx_kb"),
		NormalizedTPS:           getFloat("normalized_tps"),
		DataThroughputBytesPerS: getFloat("data_throughput_bytes_per_s"),
	}

	return nil
}

// MetricValue returns the value of a named metric field from this sample.
// This is useful for iterating over a set of metric names generically.
func (s *ExperimentSample) MetricValue(name string) float64 {
	switch name {
	// Measurements
	case "tps":
		return s.Measurements.TPS
	case "l2_tx_cost_rbtc":
		return s.Measurements.L2TxCostRBTC
	case "l2_tx_cost_min_rbtc":
		return s.Measurements.L2TxCostMinRBTC
	case "l2_tx_cost_max_rbtc":
		return s.Measurements.L2TxCostMaxRBTC
	case "l2_tx_speed_s":
		return s.Measurements.L2TxSpeedS
	case "l2_tx_count":
		return float64(s.Measurements.L2TxCount)
	case "l1_tx_count":
		return float64(s.Measurements.L1TxCount)
	case "posting_freq_s":
		return s.Measurements.PostingFreqS
	case "deposits":
		return float64(s.Measurements.Deposits)
	case "withdrawals":
		return float64(s.Measurements.Withdrawals)
	case "total_l2_txs":
		return float64(s.Measurements.TotalL2Txs)
	case "total_deposits":
		return float64(s.Measurements.TotalDeposits)
	case "total_withdrawals":
		return float64(s.Measurements.TotalWithdrawals)
	case "safe_head_lag":
		return float64(s.Measurements.SafeHeadLag)
	case "unsafe_l2":
		return float64(s.Measurements.UnsafeL2)
	case "safe_l2":
		return float64(s.Measurements.SafeL2)
	case "batcher_post_cost_rbtc":
		return s.Measurements.BatcherPostCostRBTC
	case "batcher_l1_gas_spend_wei":
		return s.Measurements.BatcherL1GasSpendWei
	case "batcher_l1_gas_price_wei":
		return s.Measurements.BatcherL1GasPriceWei
	case "batcher_data_size_bytes":
		return float64(s.Measurements.BatcherDataSizeBytes)
	case "compression_ratio":
		return s.Measurements.CompressionRatio
	case "compression_ratio_delta":
		return s.Measurements.CompressionRatioDelta
	case "txpool_pending":
		return float64(s.Measurements.TxPoolPending)
	case "txpool_queued":
		return float64(s.Measurements.TxPoolQueued)
	case "batcher_pending_blocks":
		return s.Measurements.BatcherPendingBlocks
	case "batcher_pending_bytes":
		return s.Measurements.BatcherPendingBytes
	case "batcher_l1_basefee_gwei":
		return s.Measurements.BatcherL1BaseFeeGwei
	case "batcher_balance_rbtc":
		return s.Measurements.BatcherBalanceRBTC
	case "batcher_channel_timeouts":
		return s.Measurements.BatcherChannelTimeouts
	case "batcher_tx_failed":
		return s.Measurements.BatcherTxFailed
	case "batcher_gas_bumps":
		return s.Measurements.BatcherGasBumps
	case "total_batcher_posts":
		return float64(s.Measurements.TotalBatcherPosts)
	case "total_batcher_blocks":
		return float64(s.Measurements.TotalBatcherBlocks)
	case "total_batcher_cost_rbtc":
		return s.Measurements.TotalBatcherCostRBTC
	case "total_batcher_data_bytes":
		return s.Measurements.TotalBatcherDataBytes
	case "total_l2_txs_finalized":
		return float64(s.Measurements.TotalL2TxsFinalized)
	case "l2_blocks_per_batch":
		return s.Measurements.L2BlocksPerBatch
	case "l2_txs_per_batch":
		return s.Measurements.L2TxsPerBatch
	case "l1_txs_per_batch":
		return s.Measurements.L1TxsPerBatch
	case "l2_to_l1_throughput":
		return s.Measurements.L2ToL1Throughput
	case "l2_to_l1_latency":
		return s.Measurements.L2ToL1Latency
	case "channel_open_l1_blocks":
		return s.Measurements.ChannelOpenL1Blocks
	case "l2_block":
		return float64(s.Measurements.L2Block)
	case "l1_block":
		return float64(s.Measurements.L1Block)
	// Derived
	case "amortized_l1_cost_per_l2_tx":
		return s.Derived.AmortizedL1CostPerL2Tx
	case "total_tx_cost_rbtc":
		return s.Derived.TotalTxCostRBTC
	case "total_tx_cost_min_rbtc":
		return s.Derived.TotalTxCostMinRBTC
	case "total_tx_cost_max_rbtc":
		return s.Derived.TotalTxCostMaxRBTC
	case "compressed_data_per_tx_kb":
		return s.Derived.CompressedDataPerTxKB
	case "raw_data_per_tx_kb":
		return s.Derived.RawDataPerTxKB
	case "normalized_tps":
		return s.Derived.NormalizedTPS
	case "data_throughput_bytes_per_s":
		return s.Derived.DataThroughputBytesPerS
	default:
		return 0
	}
}
