package monitor

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// TimeValue is a single data point with a timestamp and a float64 value.
type TimeValue struct {
	Time  time.Time
	Value float64
}

// RingBuffer is a fixed-capacity circular buffer of TimeValue entries.
type RingBuffer struct {
	data []TimeValue
	cap  int
	pos  int  // next write position
	full bool // true once we've wrapped around
}

// NewRingBuffer creates a ring buffer with the given capacity.
func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 100
	}
	return &RingBuffer{
		data: make([]TimeValue, capacity),
		cap:  capacity,
	}
}

// Add appends a data point. If the buffer is full, the oldest entry is overwritten.
func (rb *RingBuffer) Add(tv TimeValue) {
	rb.data[rb.pos] = tv
	rb.pos++
	if rb.pos >= rb.cap {
		rb.pos = 0
		rb.full = true
	}
}

// Entries returns all stored entries in chronological order (oldest first).
func (rb *RingBuffer) Entries() []TimeValue {
	if !rb.full {
		out := make([]TimeValue, rb.pos)
		copy(out, rb.data[:rb.pos])
		return out
	}
	out := make([]TimeValue, rb.cap)
	copy(out, rb.data[rb.pos:])
	copy(out[rb.cap-rb.pos:], rb.data[:rb.pos])
	return out
}

// Len returns the number of stored entries.
func (rb *RingBuffer) Len() int {
	if rb.full {
		return rb.cap
	}
	return rb.pos
}

// Last returns the most recently added entry, or zero TimeValue if empty.
func (rb *RingBuffer) Last() (TimeValue, bool) {
	if rb.pos == 0 && !rb.full {
		return TimeValue{}, false
	}
	idx := rb.pos - 1
	if idx < 0 {
		idx = rb.cap - 1
	}
	return rb.data[idx], true
}

// LogEntry is a timestamped log message.
type LogEntry struct {
	Time    time.Time
	Message string
}

// SlidingWindow tracks timestamped values within a configurable time window.
// Used for calculating rolling averages like sustained TPS.
type SlidingWindow struct {
	entries []TimeValue
	sum     float64
}

// NewSlidingWindow creates an empty sliding window.
func NewSlidingWindow() *SlidingWindow {
	return &SlidingWindow{
		entries: make([]TimeValue, 0, 64),
	}
}

// Add appends a new value to the window.
func (sw *SlidingWindow) Add(t time.Time, value float64) {
	sw.entries = append(sw.entries, TimeValue{Time: t, Value: value})
	sw.sum += value
}

// Prune removes entries older than the window duration and updates the sum.
func (sw *SlidingWindow) Prune(windowDuration time.Duration) {
	cutoff := time.Now().Add(-windowDuration)
	newStart := 0
	for i, e := range sw.entries {
		if e.Time.After(cutoff) {
			newStart = i
			break
		}
		sw.sum -= e.Value
		newStart = i + 1
	}
	if newStart > 0 {
		sw.entries = sw.entries[newStart:]
	}
}

// Sum returns the total of all values currently in the window.
func (sw *SlidingWindow) Sum() float64 {
	return sw.sum
}

// Rate returns the sum divided by the window duration (values per second).
func (sw *SlidingWindow) Rate(windowDuration time.Duration) float64 {
	if windowDuration <= 0 {
		return 0
	}
	return sw.sum / windowDuration.Seconds()
}

// Len returns the number of entries in the window.
func (sw *SlidingWindow) Len() int {
	return len(sw.entries)
}

// LogBuffer is a fixed-capacity circular buffer for log entries.
type LogBuffer struct {
	data  []LogEntry
	cap   int
	pos   int
	full  bool
	total int // monotonically increasing count of all entries ever added
}

// NewLogBuffer creates a log buffer with the given capacity.
func NewLogBuffer(capacity int) *LogBuffer {
	if capacity <= 0 {
		capacity = 50
	}
	return &LogBuffer{
		data: make([]LogEntry, capacity),
		cap:  capacity,
	}
}

// Add appends a log entry.
func (lb *LogBuffer) Add(entry LogEntry) {
	lb.data[lb.pos] = entry
	lb.pos++
	lb.total++
	if lb.pos >= lb.cap {
		lb.pos = 0
		lb.full = true
	}
}

// Entries returns all stored entries in chronological order (oldest first).
func (lb *LogBuffer) Entries() []LogEntry {
	if !lb.full {
		out := make([]LogEntry, lb.pos)
		copy(out, lb.data[:lb.pos])
		return out
	}
	out := make([]LogEntry, lb.cap)
	copy(out, lb.data[lb.pos:])
	copy(out[lb.cap-lb.pos:], lb.data[:lb.pos])
	return out
}

// Len returns the total number of entries ever added (monotonically increasing).
func (lb *LogBuffer) Len() int {
	return lb.total
}

// EntriesSince returns entries added after the given monotonic index.
// Use Len() to get the current index, then call EntriesSince(prevLen) later
// to get only new entries.
func (lb *LogBuffer) EntriesSince(since int) []LogEntry {
	if since >= lb.total {
		return nil
	}
	all := lb.Entries()
	skip := since - (lb.total - len(all))
	if skip < 0 {
		skip = 0
	}
	if skip >= len(all) {
		return nil
	}
	return all[skip:]
}

// MetricSummary tracks the running peak and average for a single metric.
type MetricSummary struct {
	Peak     float64
	PeakTime time.Time
	PeakCtx  string // context when peak occurred (e.g. "traffic ON @ 96 tx/s")

	Sum          float64
	Count        int64
	NonZeroSum   float64
	NonZeroCount int64
}

// Average returns the running average (including zeros).
func (s *MetricSummary) Average() float64 {
	if s.Count == 0 {
		return 0
	}
	return s.Sum / float64(s.Count)
}

// NonZeroAverage returns the running average excluding zero data points.
func (s *MetricSummary) NonZeroAverage() float64 {
	if s.NonZeroCount == 0 {
		return 0
	}
	return s.NonZeroSum / float64(s.NonZeroCount)
}

// EventStatus represents the lifecycle state of a pending event.
type EventStatus string

const (
	EventPending    EventStatus = "pending"
	EventConfirmed  EventStatus = "confirmed"
	EventProving    EventStatus = "proving"
	EventWaiting    EventStatus = "waiting"
	EventFinalizing EventStatus = "finalizing"
	EventComplete   EventStatus = "complete"
	EventFailed     EventStatus = "failed"
)

// PendingEvent tracks the status of an in-flight bridge operation.
type PendingEvent struct {
	ID        string
	Type      string // "deposit-rbtc", "withdraw-rbtc", "deposit-usdrif", "withdraw-usdrif"
	Amount    string // human-readable, e.g. "0.001 RBTC"
	Status    EventStatus
	TxHash    string // first tx hash (truncated for display)
	StartTime time.Time
	Detail    string // context-dependent, e.g. "L1 #12345"
}

// record updates the summary with a new value. Returns true if a new peak was set.
func (s *MetricSummary) record(value float64, t time.Time, ctx string) bool {
	s.Sum += value
	s.Count++
	if value != 0 {
		s.NonZeroSum += value
		s.NonZeroCount++
	}
	if value > s.Peak {
		s.Peak = value
		s.PeakTime = t
		s.PeakCtx = ctx
		return true
	}
	return false
}

// SummarySnapshot is a read-only copy of a MetricSummary.
type SummarySnapshot struct {
	Peak    float64
	PeakCtx string
	Avg     float64 // NonZeroAverage for cost/speed, Average for others
}

// MetricsStore is the thread-safe central store for all real-time metrics.
type MetricsStore struct {
	mu sync.RWMutex

	TPS                   *RingBuffer // Transactions per second (per L2 block, instantaneous)
	TPSRolling            *RingBuffer // Rolling window TPS (sustained throughput)
	TPSSimulator          *RingBuffer // Simulator TPS (time-to-inclusion based)
	L2TxCost              *RingBuffer // Average tx cost in RBTC (per L2 block)
	L2TxCostMin           *RingBuffer // Min individual tx cost in RBTC (per L2 block)
	L2TxCostMax           *RingBuffer // Max individual tx cost in RBTC (per L2 block)
	L2TxSpeed             *RingBuffer // Tx send-to-receipt latency in seconds
	L2TxCount             *RingBuffer // Number of user txs per L2 block
	L1TxCount             *RingBuffer // Number of txs per L1 block
	PostingFreq           *RingBuffer // Seconds since last batcher posting
	Deposits              *RingBuffer // Number of deposits per L2 block
	Withdrawals           *RingBuffer // Number of withdrawals per L2 block
	SafeHeadLag           *RingBuffer // Blocks between unsafe and safe L2 head
	BatcherPostCost       *RingBuffer // Cost of each batcher L1 post in RBTC
	BatcherL1GasSpendWei  *RingBuffer // gasUsed * effectiveGasPrice (wei), last batcher L1 tx
	BatcherL1GasPriceWei  *RingBuffer // effective gas price (wei) for last batcher L1 tx
	BatcherDataSize       *RingBuffer // Calldata size of each batcher L1 post in bytes
	CompressionRatio      *RingBuffer // Compression ratio from batcher metrics (cumulative avg)
	CompressionRatioDelta *RingBuffer // Per-channel delta compression ratio
	TxPoolPending         *RingBuffer // L2 txpool pending (executable) transaction count
	TxPoolQueued          *RingBuffer // L2 txpool queued (non-executable) transaction count

	// --- L2 blocks per batch (decoded from batcher L1 tx calldata) ---
	L2BlocksPerBatch *RingBuffer // L2 blocks in each completed batch channel
	L2TxsPerBatch    *RingBuffer // L2 txs in each completed batch channel
	L1TxsPerBatch    *RingBuffer // L1 txs used to post each completed batch channel

	// --- Channel open duration (L1 blocks from first to last frame) ---
	ChannelOpenL1Blocks *RingBuffer // L1 blocks a channel was open (lastL1 - firstL1)

	// --- L2 to L1 throughput (computed when channel completes) ---
	L2ToL1Throughput *RingBuffer // L2 txs per second reaching L1 finality
	L2ToL1Latency    *RingBuffer // Seconds from earliest L2 block to L1 confirmation

	// --- Prometheus-sourced metrics ---

	// Process liveness (from Prometheus "up" gauges)
	NodeUp     bool
	BatcherUp  bool
	ProposerUp bool

	// op-node derivation health
	DerivationIdle         bool
	DerivationErrors       *RingBuffer // delta rate (errors/poll)
	PipelineResets         *RingBuffer // delta rate
	SequencingErrors       *RingBuffer // delta rate
	L1HeadLatency          *RingBuffer // seconds behind L1 head
	UnsafePayloadsBuffered *RingBuffer // buffered unsafe payloads count

	// op-batcher pipeline
	BatcherPendingBlocks   *RingBuffer // blocks waiting to be batched
	BatcherPendingBytes    *RingBuffer // bytes pending in batcher
	BatcherChannelTimeouts *RingBuffer // delta rate
	BatcherTxFailed        *RingBuffer // delta rate
	BatcherGasBumps        *RingBuffer // current gas bump count
	BatcherL1BaseFee       *RingBuffer // L1 base fee in gwei
	BatcherBalance         *RingBuffer // batcher balance in RBTC

	// op-proposer
	ProposedSeqNum  *RingBuffer // latest proposed sequence number
	ProposerBalance *RingBuffer // proposer balance in RBTC

	// pprof resource metrics
	NodeHeapMB        *RingBuffer
	NodeGoroutines    *RingBuffer
	BatcherHeapMB     *RingBuffer
	BatcherGoroutines *RingBuffer

	// Previous counter values for delta computation (not exported)
	prevDerivationErrors       float64
	prevPipelineResets         float64
	prevSequencingErrors       float64
	prevBatcherChannelTimeouts float64
	prevBatcherTxFailed        float64
	prevComprRatioSum          float64
	prevComprRatioCount        float64

	// Sliding windows for rolling TPS calculation
	tpsWindow         *SlidingWindow // L2 tx counts for rolling TPS
	simWindow         *SlidingWindow // Simulator confirmations for simulator TPS
	tpsWindowDuration time.Duration  // Configurable window duration (default 30s)

	// Per-metric peak/average summaries
	sumTPS                  MetricSummary
	sumTPSRolling           MetricSummary
	sumTPSSimulator         MetricSummary
	sumL2Cost               MetricSummary
	sumL2CostMin            MetricSummary
	sumL2CostMax            MetricSummary
	sumL2Speed              MetricSummary
	sumL2Count              MetricSummary
	sumL1Count              MetricSummary
	sumPostFreq             MetricSummary
	sumDeposits             MetricSummary
	sumWithdraw             MetricSummary
	sumSafeHeadLag          MetricSummary
	sumBatcherPostCost      MetricSummary
	sumBatcherL1GasSpendWei MetricSummary
	sumBatcherL1GasPriceWei MetricSummary
	sumBatcherDataSize      MetricSummary
	sumCompressionRatio     MetricSummary
	sumTxPoolPending        MetricSummary
	sumTxPoolQueued         MetricSummary
	sumL2BlocksPerBatch     MetricSummary
	sumL2TxsPerBatch        MetricSummary
	sumL1TxsPerBatch        MetricSummary
	sumChannelOpenL1Blocks  MetricSummary
	sumL2ToL1Throughput     MetricSummary
	sumL2ToL1Latency        MetricSummary

	// Current traffic context (set by TUI when traffic state changes)
	trafficCtx string

	// Cumulative counters
	TotalDeposits         int64
	TotalWithdrawals      int64
	TotalL2Txs            int64
	TotalBatcherCostRBTC  float64 // cumulative batcher L1 posting cost
	TotalBatcherDataBytes float64 // cumulative batcher calldata bytes posted to L1
	TotalBatcherPosts     int64   // cumulative count of individual batcher L1 txs
	TotalBatcherBlocks    int64   // cumulative count of L1 blocks containing at least one batcher tx
	TotalL2TxsFinalized   int64   // cumulative count of L2 txs that reached L1 finality

	// Latest block numbers
	LatestL2Block uint64
	LatestL1Block uint64

	// Sync status from op-node (optimism_syncStatus)
	UnsafeL2       uint64    // latest unsafe L2 head from op-node
	SafeL2         uint64    // latest safe L2 head from op-node
	prevSafeL2     uint64    // previous safe L2 head (for velocity calc)
	prevSafeL2Time time.Time // time of previous measurement
	SafeHeadVel    float64   // blocks/sec safe head is advancing

	// Batcher staleness: seconds since last detected batcher post (-1 = never seen)
	BatcherStaleness float64
	LastBatcherPost  time.Time // time of last detected batcher tx (zero = never)

	// Pending txs and balances
	L1PendingTxs    uint64
	L2PendingTxs    uint64
	L1Balance       float64 // in RBTC
	L2Balance       float64 // in RBTC
	L1USDRIFBalance float64 // USDRIF balance on L1 (in token units)
	L2USDRIFBalance float64 // USDRIF balance on L2

	// Bridge and exit cost tracking (set by l1sender after operations)
	LastDepositCostRBTC                float64 // gas cost of last RBTC deposit on L1
	LastWithdrawCostRBTC               float64 // total gas cost of last RBTC withdrawal (L2 init + prove + finalize)
	LastWithdrawL2InitCostRBTC         float64 // gas cost of L2 initiation step
	LastProveCostRBTC                  float64 // gas cost of prove step
	LastFinalizeCostRBTC               float64 // gas cost of finalize step
	LastUSDRIFDepositCostRBTC          float64 // gas cost of last USDRIF deposit (approve + bridge)
	LastUSDRIFWithdrawCostRBTC         float64 // total gas cost of last USDRIF withdrawal (L2 init + prove + finalize)
	LastUSDRIFWithdrawL2InitCostRBTC   float64 // USDRIF withdrawal L2 initiation gas cost
	LastUSDRIFWithdrawProveCostRBTC    float64 // USDRIF withdrawal prove gas cost
	LastUSDRIFWithdrawFinalizeCostRBTC float64 // USDRIF withdrawal finalize gas cost
	ExitCostRBTC                       float64 // exit cost without dispute = L2 init + prove + finalize

	// Bridge operation counters (incremented by l1sender on each operation)
	RBTCDepositsTriggered      int64
	RBTCWithdrawalsTriggered   int64
	USDRIFDepositsTriggered    int64
	USDRIFWithdrawalsTriggered int64

	// Theoretical max TPS from block gas limit
	TheoreticalMaxTPS float64

	// Status message for the TUI status bar (collector status)
	Status string

	// Action log: accumulated messages from all components
	Log *LogBuffer

	// Optional file writer for persisting log entries to disk
	logFile io.Writer

	// Optional CSV writer for stats export
	statsFile io.Writer

	// Pending bridge events
	pendingEvents []PendingEvent
	eventCounter  int64 // monotonic counter for generating unique IDs

	// Funding tx hashes registered by the traffic simulator so the
	// collector can exclude them from user-tx metrics (TPS, cost, etc.).
	fundingTxs map[common.Hash]struct{}

	// Channel for notifying when a new L1 block is observed.
	l1BlockCh chan struct{}

	// Tracked tx send times for accurate L2-to-L1 latency measurement.
	// Key is tx hash, value is the time the tx was sent to L2 mempool.
	// Populated by traffic simulator, consumed by collector when channel completes.
	trackedTxSendTimes map[common.Hash]time.Time
}

// DefaultTPSWindowDuration is the default duration for rolling TPS calculation.
const DefaultTPSWindowDuration = 30 * time.Second

// NewMetricsStore creates a MetricsStore with the given history size for all ring buffers.
func NewMetricsStore(historySize int) *MetricsStore {
	return &MetricsStore{
		TPS:                   NewRingBuffer(historySize),
		TPSRolling:            NewRingBuffer(historySize),
		TPSSimulator:          NewRingBuffer(historySize),
		L2TxCost:              NewRingBuffer(historySize),
		L2TxCostMin:           NewRingBuffer(historySize),
		L2TxCostMax:           NewRingBuffer(historySize),
		L2TxSpeed:             NewRingBuffer(historySize),
		L2TxCount:             NewRingBuffer(historySize),
		L1TxCount:             NewRingBuffer(historySize),
		PostingFreq:           NewRingBuffer(historySize),
		Deposits:              NewRingBuffer(historySize),
		Withdrawals:           NewRingBuffer(historySize),
		SafeHeadLag:           NewRingBuffer(historySize),
		BatcherPostCost:       NewRingBuffer(historySize),
		BatcherL1GasSpendWei:  NewRingBuffer(historySize),
		BatcherL1GasPriceWei:  NewRingBuffer(historySize),
		BatcherDataSize:       NewRingBuffer(historySize),
		CompressionRatio:      NewRingBuffer(historySize),
		CompressionRatioDelta: NewRingBuffer(historySize),
		TxPoolPending:         NewRingBuffer(historySize),
		TxPoolQueued:          NewRingBuffer(historySize),
		L2BlocksPerBatch:      NewRingBuffer(historySize),
		L2TxsPerBatch:         NewRingBuffer(historySize),
		L1TxsPerBatch:         NewRingBuffer(historySize),
		ChannelOpenL1Blocks:   NewRingBuffer(historySize),
		L2ToL1Throughput:      NewRingBuffer(historySize),
		L2ToL1Latency:         NewRingBuffer(historySize),

		l1BlockCh: make(chan struct{}, 1),

		// Prometheus-sourced ring buffers
		DerivationErrors:       NewRingBuffer(historySize),
		PipelineResets:         NewRingBuffer(historySize),
		SequencingErrors:       NewRingBuffer(historySize),
		L1HeadLatency:          NewRingBuffer(historySize),
		UnsafePayloadsBuffered: NewRingBuffer(historySize),
		BatcherPendingBlocks:   NewRingBuffer(historySize),
		BatcherPendingBytes:    NewRingBuffer(historySize),
		BatcherChannelTimeouts: NewRingBuffer(historySize),
		BatcherTxFailed:        NewRingBuffer(historySize),
		BatcherGasBumps:        NewRingBuffer(historySize),
		BatcherL1BaseFee:       NewRingBuffer(historySize),
		BatcherBalance:         NewRingBuffer(historySize),
		ProposedSeqNum:         NewRingBuffer(historySize),
		ProposerBalance:        NewRingBuffer(historySize),
		NodeHeapMB:             NewRingBuffer(historySize),
		NodeGoroutines:         NewRingBuffer(historySize),
		BatcherHeapMB:          NewRingBuffer(historySize),
		BatcherGoroutines:      NewRingBuffer(historySize),

		BatcherStaleness:   -1, // -1 = never seen
		Status:             "Initializing...",
		Log:                NewLogBuffer(50),
		fundingTxs:         make(map[common.Hash]struct{}),
		trackedTxSendTimes: make(map[common.Hash]time.Time),

		// Sliding windows for rolling TPS
		tpsWindow:         NewSlidingWindow(),
		simWindow:         NewSlidingWindow(),
		tpsWindowDuration: DefaultTPSWindowDuration,
	}
}

// LoadFromCSV reads a previously exported stats CSV file and populates the ring buffers,
// metric summaries, and cumulative counters. Returns the last L2 and L1 block numbers
// seen so the collector knows where to resume. Only the last N rows (matching the ring
// buffer capacity) are kept to avoid memory waste on very large files.
func (m *MetricsStore) LoadFromCSV(r io.Reader) (lastL2Block uint64, lastL1Block uint64, rowCount int, err error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1 // allow variable number of fields
	reader.TrimLeadingSpace = true

	// Read all records
	records, err := reader.ReadAll()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read CSV: %w", err)
	}
	if len(records) < 2 {
		return 0, 0, 0, nil // header only or empty
	}

	// Skip header row
	dataRows := records[1:]

	// Only keep the last N rows (matching ring buffer capacity)
	cap := m.TPS.cap
	startIdx := 0
	if len(dataRows) > cap {
		startIdx = len(dataRows) - cap
	}
	dataRows = dataRows[startIdx:]

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, row := range dataRows {
		if len(row) < 11 {
			continue // skip malformed rows
		}

		// Parse timestamp. Use ParseInLocation with time.Local because
		// WriteStatsRow formats with t.Format("...Z") where "Z" is a
		// literal character, not a timezone marker — the actual time is
		// in the local timezone. Parsing with time.Parse would interpret
		// the string as UTC, shifting every timestamp by the local UTC
		// offset and pushing all data into the future on the chart.
		t, terr := time.ParseInLocation("2006-01-02T15:04:05.000Z", row[0], time.Local)
		if terr != nil {
			// Try alternate format without millis
			t, terr = time.ParseInLocation("2006-01-02T15:04:05Z", row[0], time.Local)
			if terr != nil {
				continue
			}
		}

		l2Block := parseUint64(row[1])
		l1Block := parseUint64(row[2])
		tps := parseFloat64(row[3])
		l2TxCost := parseFloat64(row[4])
		l2TxSpeed := parseFloat64(row[5])
		l2TxCount := parseFloat64(row[6])
		l1TxCount := parseFloat64(row[7])
		postFreq := parseFloat64(row[8])
		deposits := parseFloat64(row[9])
		wdls := parseFloat64(row[10])

		// Push into ring buffers (no peak logging during import)
		m.TPS.Add(TimeValue{Time: t, Value: tps})
		m.sumTPS.record(tps, t, "csv-import")
		m.L2TxCost.Add(TimeValue{Time: t, Value: l2TxCost})
		m.sumL2Cost.record(l2TxCost, t, "csv-import")
		m.L2TxSpeed.Add(TimeValue{Time: t, Value: l2TxSpeed})
		m.sumL2Speed.record(l2TxSpeed, t, "csv-import")
		m.L2TxCount.Add(TimeValue{Time: t, Value: l2TxCount})
		m.sumL2Count.record(l2TxCount, t, "csv-import")
		m.L1TxCount.Add(TimeValue{Time: t, Value: l1TxCount})
		m.sumL1Count.record(l1TxCount, t, "csv-import")
		m.PostingFreq.Add(TimeValue{Time: t, Value: postFreq})
		m.sumPostFreq.record(postFreq, t, "csv-import")
		m.Deposits.Add(TimeValue{Time: t, Value: deposits})
		m.sumDeposits.record(deposits, t, "csv-import")
		m.Withdrawals.Add(TimeValue{Time: t, Value: wdls})
		m.sumWithdraw.record(wdls, t, "csv-import")

		// Safe head lag (column 18, optional — may not be present in older CSVs)
		if len(row) >= 19 {
			safeHeadLag := parseFloat64(row[18])
			m.SafeHeadLag.Add(TimeValue{Time: t, Value: safeHeadLag})
			m.sumSafeHeadLag.record(safeHeadLag, t, "csv-import")
		}

		// Batcher post cost, data size, compression ratio (columns 21-23, optional)
		if len(row) >= 22 {
			batcherPostCost := parseFloat64(row[21])
			if batcherPostCost > 0 {
				m.BatcherPostCost.Add(TimeValue{Time: t, Value: batcherPostCost})
				m.sumBatcherPostCost.record(batcherPostCost, t, "csv-import")
			}
		}
		if len(row) >= 23 {
			batcherDataSize := parseFloat64(row[22])
			if batcherDataSize > 0 {
				m.BatcherDataSize.Add(TimeValue{Time: t, Value: batcherDataSize})
				m.sumBatcherDataSize.record(batcherDataSize, t, "csv-import")
			}
		}
		if len(row) >= 24 {
			compressionRatio := parseFloat64(row[23])
			if compressionRatio > 0 {
				m.CompressionRatio.Add(TimeValue{Time: t, Value: compressionRatio})
				m.sumCompressionRatio.record(compressionRatio, t, "csv-import")
			}
		}
		if len(row) >= 25 {
			compressionRatioDelta := parseFloat64(row[24])
			if compressionRatioDelta > 0 {
				m.CompressionRatioDelta.Add(TimeValue{Time: t, Value: compressionRatioDelta})
			}
		}

		// Track block numbers
		if l2Block > lastL2Block {
			lastL2Block = l2Block
		}
		if l1Block > lastL1Block {
			lastL1Block = l1Block
		}

		rowCount++
	}

	// Restore cumulative counters from the last row (columns 11-17 if present)
	if len(dataRows) > 0 {
		lastRow := dataRows[len(dataRows)-1]
		if len(lastRow) >= 14 {
			m.TotalL2Txs = int64(parseUint64(lastRow[11]))
			m.TotalDeposits = int64(parseUint64(lastRow[12]))
			m.TotalWithdrawals = int64(parseUint64(lastRow[13]))
		}
		if len(lastRow) >= 18 {
			m.L1PendingTxs = parseUint64(lastRow[14])
			m.L2PendingTxs = parseUint64(lastRow[15])
			m.L1Balance = parseFloat64(lastRow[16])
			m.L2Balance = parseFloat64(lastRow[17])
		}
		if len(lastRow) >= 36 {
			m.TotalBatcherDataBytes = parseFloat64(lastRow[35])
		}
	}

	// Set latest block numbers
	m.LatestL1Block = lastL1Block
	m.LatestL2Block = lastL2Block

	return lastL2Block, lastL1Block, rowCount, nil
}

// IngestExperimentSample pushes a single experiment data point into the store.
// The values map uses experiment CSV column names as keys (e.g. "tps",
// "l2_tx_cost_rbtc"). This enables the analysis TUI to reuse MetricsStore as
// the per-run data layer when visualising experiment CSV output.
func (m *MetricsStore) IngestExperimentSample(t time.Time, values map[string]float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ctx := "experiment"

	if v, ok := values["tps"]; ok {
		m.TPS.Add(TimeValue{Time: t, Value: v})
		m.sumTPS.record(v, t, ctx)
	}
	if v, ok := values["l2_tx_cost_rbtc"]; ok {
		m.L2TxCost.Add(TimeValue{Time: t, Value: v})
		m.sumL2Cost.record(v, t, ctx)
	}
	if v, ok := values["l2_tx_speed_s"]; ok {
		m.L2TxSpeed.Add(TimeValue{Time: t, Value: v})
		m.sumL2Speed.record(v, t, ctx)
	}
	if v, ok := values["l2_tx_count"]; ok {
		m.L2TxCount.Add(TimeValue{Time: t, Value: v})
		m.sumL2Count.record(v, t, ctx)
	}
	if v, ok := values["l1_tx_count"]; ok {
		m.L1TxCount.Add(TimeValue{Time: t, Value: v})
		m.sumL1Count.record(v, t, ctx)
	}
	if v, ok := values["posting_freq_s"]; ok {
		m.PostingFreq.Add(TimeValue{Time: t, Value: v})
		m.sumPostFreq.record(v, t, ctx)
	}
	if v, ok := values["deposits"]; ok {
		m.Deposits.Add(TimeValue{Time: t, Value: v})
		m.sumDeposits.record(v, t, ctx)
	}
	if v, ok := values["withdrawals"]; ok {
		m.Withdrawals.Add(TimeValue{Time: t, Value: v})
		m.sumWithdraw.record(v, t, ctx)
	}
	if v, ok := values["safe_head_lag"]; ok {
		m.SafeHeadLag.Add(TimeValue{Time: t, Value: v})
		m.sumSafeHeadLag.record(v, t, ctx)
	}
	if v, ok := values["l2_to_l1_throughput"]; ok && v > 0 {
		m.L2ToL1Throughput.Add(TimeValue{Time: t, Value: v})
		m.sumL2ToL1Throughput.record(v, t, ctx)
	}
	if v, ok := values["l2_to_l1_latency"]; ok && v > 0 {
		m.L2ToL1Latency.Add(TimeValue{Time: t, Value: v})
		m.sumL2ToL1Latency.record(v, t, ctx)
	}
	if v, ok := values["total_l2_txs_finalized"]; ok {
		m.TotalL2TxsFinalized = int64(v)
	}
	if v, ok := values["batcher_post_cost_rbtc"]; ok && v > 0 {
		m.BatcherPostCost.Add(TimeValue{Time: t, Value: v})
		m.sumBatcherPostCost.record(v, t, ctx)
	}
	if v, ok := values["batcher_l1_gas_spend_wei"]; ok && v > 0 {
		m.BatcherL1GasSpendWei.Add(TimeValue{Time: t, Value: v})
		m.sumBatcherL1GasSpendWei.record(v, t, ctx)
	}
	if v, ok := values["batcher_l1_gas_price_wei"]; ok && v > 0 {
		m.BatcherL1GasPriceWei.Add(TimeValue{Time: t, Value: v})
		m.sumBatcherL1GasPriceWei.record(v, t, ctx)
	}
	if v, ok := values["batcher_data_size_bytes"]; ok && v > 0 {
		m.BatcherDataSize.Add(TimeValue{Time: t, Value: v})
		m.sumBatcherDataSize.record(v, t, ctx)
	}
	if v, ok := values["compression_ratio"]; ok && v > 0 {
		m.CompressionRatio.Add(TimeValue{Time: t, Value: v})
		m.sumCompressionRatio.record(v, t, ctx)
	}
	if v, ok := values["txpool_pending"]; ok {
		m.TxPoolPending.Add(TimeValue{Time: t, Value: v})
		m.sumTxPoolPending.record(v, t, ctx)
	}
	if v, ok := values["txpool_queued"]; ok {
		m.TxPoolQueued.Add(TimeValue{Time: t, Value: v})
		m.sumTxPoolQueued.record(v, t, ctx)
	}
	if v, ok := values["batcher_pending_blocks"]; ok && v > 0 {
		m.BatcherPendingBlocks.Add(TimeValue{Time: t, Value: v})
	}
	if v, ok := values["batcher_pending_bytes"]; ok && v > 0 {
		m.BatcherPendingBytes.Add(TimeValue{Time: t, Value: v})
	}
	if v, ok := values["batcher_l1_basefee_gwei"]; ok && v > 0 {
		m.BatcherL1BaseFee.Add(TimeValue{Time: t, Value: v})
	}
	if v, ok := values["batcher_balance_rbtc"]; ok && v > 0 {
		m.BatcherBalance.Add(TimeValue{Time: t, Value: v})
	}
	if v, ok := values["batcher_channel_timeouts"]; ok && v > 0 {
		m.BatcherChannelTimeouts.Add(TimeValue{Time: t, Value: v})
	}
	if v, ok := values["batcher_tx_failed"]; ok && v > 0 {
		m.BatcherTxFailed.Add(TimeValue{Time: t, Value: v})
	}
	if v, ok := values["batcher_gas_bumps"]; ok && v > 0 {
		m.BatcherGasBumps.Add(TimeValue{Time: t, Value: v})
	}

	// Update block numbers and cumulative counters.
	if v, ok := values["l2_block"]; ok && uint64(v) > m.LatestL2Block {
		m.LatestL2Block = uint64(v)
	}
	if v, ok := values["l1_block"]; ok && uint64(v) > m.LatestL1Block {
		m.LatestL1Block = uint64(v)
	}
	if v, ok := values["total_l2_txs"]; ok {
		m.TotalL2Txs = int64(v)
	}
	if v, ok := values["total_deposits"]; ok {
		m.TotalDeposits = int64(v)
	}
	if v, ok := values["total_withdrawals"]; ok {
		m.TotalWithdrawals = int64(v)
	}
	if v, ok := values["unsafe_l2"]; ok {
		m.UnsafeL2 = uint64(v)
	}
	if v, ok := values["safe_l2"]; ok {
		m.SafeL2 = uint64(v)
	}
	if v, ok := values["total_batcher_posts"]; ok {
		m.TotalBatcherPosts = int64(v)
	}
	if v, ok := values["total_batcher_blocks"]; ok {
		m.TotalBatcherBlocks = int64(v)
	}
	if v, ok := values["total_batcher_cost_rbtc"]; ok {
		m.TotalBatcherCostRBTC = v
	}
	if v, ok := values["total_batcher_data_bytes"]; ok {
		m.TotalBatcherDataBytes = v
	}
}

// parseFloat64 parses a string to float64, returning 0 on error.
func parseFloat64(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// parseUint64 parses a string to uint64, returning 0 on error.
func parseUint64(s string) uint64 {
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}

// SetTrafficContext updates the traffic context string used to annotate peaks.
func (m *MetricsStore) SetTrafficContext(ctx string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.trafficCtx = ctx
}

// addAndTrack is a helper: adds to ring buffer, updates summary, logs new peaks.
func (m *MetricsStore) addAndTrack(rb *RingBuffer, summary *MetricSummary, name string, t time.Time, value float64) {
	rb.Add(TimeValue{Time: t, Value: value})
	if summary.record(value, t, m.trafficCtx) && value > 0 {
		// New peak — log it (we're already holding the lock, so write directly)
		msg := fmt.Sprintf("[peak] %s new peak: %.4g", name, value)
		if m.trafficCtx != "" {
			msg += " (" + m.trafficCtx + ")"
		}
		m.Log.Add(LogEntry{Time: time.Now(), Message: msg})
		if m.logFile != nil {
			fmt.Fprintf(m.logFile, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05.000"), msg)
		}
	}
}

// AddTPS records a TPS measurement.
func (m *MetricsStore) AddTPS(t time.Time, tps float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.TPS, &m.sumTPS, "TPS", t, tps)
}

// SetTPSWindowDuration updates the rolling window duration for TPS calculation.
func (m *MetricsStore) SetTPSWindowDuration(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d < 5*time.Second {
		d = 5 * time.Second
	}
	if d > 300*time.Second {
		d = 300 * time.Second
	}
	m.tpsWindowDuration = d
}

// GetTPSWindowDuration returns the current rolling window duration.
func (m *MetricsStore) GetTPSWindowDuration() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tpsWindowDuration
}

// AddL2TxCost records an average tx cost.
func (m *MetricsStore) AddL2TxCost(t time.Time, costRBTC float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.L2TxCost, &m.sumL2Cost, "L2 Tx Cost", t, costRBTC)
}

// AddL2TxCostMin records the minimum individual tx cost in the latest block.
func (m *MetricsStore) AddL2TxCostMin(t time.Time, costRBTC float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.L2TxCostMin, &m.sumL2CostMin, "L2 Tx Cost Min", t, costRBTC)
}

// AddL2TxCostMax records the maximum individual tx cost in the latest block.
func (m *MetricsStore) AddL2TxCostMax(t time.Time, costRBTC float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.L2TxCostMax, &m.sumL2CostMax, "L2 Tx Cost Max", t, costRBTC)
}

// AddL2TxSpeed records a tx latency measurement.
func (m *MetricsStore) AddL2TxSpeed(t time.Time, latencySeconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.L2TxSpeed, &m.sumL2Speed, "L2 Tx Speed", t, latencySeconds)
}

// AddL2TxCount records the number of user txs in an L2 block and updates rolling TPS.
func (m *MetricsStore) AddL2TxCount(t time.Time, count float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.L2TxCount, &m.sumL2Count, "Txs/L2 Block", t, count)
	m.TotalL2Txs += int64(count)

	// Update rolling TPS window
	m.tpsWindow.Add(t, count)
	m.tpsWindow.Prune(m.tpsWindowDuration)
	rollingTPS := m.tpsWindow.Rate(m.tpsWindowDuration)
	m.addAndTrack(m.TPSRolling, &m.sumTPSRolling, "TPS Rolling", t, rollingTPS)
}

// RecordSimulatorConfirmation records a confirmed simulator transaction for time-to-inclusion TPS.
func (m *MetricsStore) RecordSimulatorConfirmation(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.simWindow.Add(t, 1)
	m.simWindow.Prune(m.tpsWindowDuration)
	simTPS := m.simWindow.Rate(m.tpsWindowDuration)
	m.addAndTrack(m.TPSSimulator, &m.sumTPSSimulator, "TPS Simulator", t, simTPS)
}

// AddL1TxCount records the number of txs in an L1 block.
func (m *MetricsStore) AddL1TxCount(t time.Time, count float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.L1TxCount, &m.sumL1Count, "Txs/L1 Block", t, count)
}

// AddPostingFreq records seconds since last batcher posting.
func (m *MetricsStore) AddPostingFreq(t time.Time, secondsSinceLast float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.PostingFreq, &m.sumPostFreq, "Posting Freq", t, secondsSinceLast)
}

// AddDeposits records the number of deposits in an L2 block.
func (m *MetricsStore) AddDeposits(t time.Time, count float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.Deposits, &m.sumDeposits, "Deposits", t, count)
	m.TotalDeposits += int64(count)
}

// AddWithdrawals records the number of withdrawals in an L2 block.
func (m *MetricsStore) AddWithdrawals(t time.Time, count float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.Withdrawals, &m.sumWithdraw, "Withdrawals", t, count)
	m.TotalWithdrawals += int64(count)
}

// AddSafeHeadLag records the gap between unsafe and safe L2 heads.
func (m *MetricsStore) AddSafeHeadLag(t time.Time, lagBlocks float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.SafeHeadLag, &m.sumSafeHeadLag, "Safe Head Lag", t, lagBlocks)
}

// AddBatcherPostCost records the cost of a batcher L1 post in RBTC.
func (m *MetricsStore) AddBatcherPostCost(t time.Time, costRBTC float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.BatcherPostCost, &m.sumBatcherPostCost, "Batcher Post Cost", t, costRBTC)
	m.TotalBatcherCostRBTC += costRBTC
	m.TotalBatcherPosts++
}

// AddBatcherL1GasStats records execution gas spend (wei) and effective gas price (wei)
// for a batcher inbox L1 transaction (excludes OP-stack L1 data fee).
func (m *MetricsStore) AddBatcherL1GasStats(t time.Time, gasSpendWei, gasPriceWei float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.BatcherL1GasSpendWei, &m.sumBatcherL1GasSpendWei, "Batcher L1 Gas Spend Wei", t, gasSpendWei)
	m.addAndTrack(m.BatcherL1GasPriceWei, &m.sumBatcherL1GasPriceWei, "Batcher L1 Gas Price Wei", t, gasPriceWei)
}

// AddBatcherBlock records that an L1 block contained at least one batcher tx.
func (m *MetricsStore) AddBatcherBlock() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.TotalBatcherBlocks++
}

// AddBatcherDataSize records the calldata size of a batcher L1 post in bytes.
func (m *MetricsStore) AddBatcherDataSize(t time.Time, bytes float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.BatcherDataSize, &m.sumBatcherDataSize, "Batcher Data Size", t, bytes)
	m.TotalBatcherDataBytes += bytes
}

// AddCompressionRatio records a compression ratio measurement.
func (m *MetricsStore) AddCompressionRatio(t time.Time, ratio float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.CompressionRatio, &m.sumCompressionRatio, "Compression Ratio", t, ratio)
}

// AddCompressionRatioDelta computes the per-channel compression ratio from
// cumulative Prometheus counters (channel_compr_ratio_sum / _count). On each
// scrape it records delta_sum / delta_count, giving the average ratio of
// channels produced since the previous scrape.
func (m *MetricsStore) AddCompressionRatioDelta(t time.Time, sum, count float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.prevComprRatioCount > 0 {
		dSum := sum - m.prevComprRatioSum
		dCount := count - m.prevComprRatioCount
		if dCount > 0 {
			m.CompressionRatioDelta.Add(TimeValue{Time: t, Value: dSum / dCount})
		}
	}
	m.prevComprRatioSum = sum
	m.prevComprRatioCount = count
}

// AddTxPoolStatus records the current L2 txpool pending and queued counts.
func (m *MetricsStore) AddTxPoolStatus(t time.Time, pending, queued float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.TxPoolPending, &m.sumTxPoolPending, "TxPool Pending", t, pending)
	m.addAndTrack(m.TxPoolQueued, &m.sumTxPoolQueued, "TxPool Queued", t, queued)
}

// AddL2BlocksPerBatch records the number of L2 blocks, L2 txs, and L1 txs for a completed batch channel.
func (m *MetricsStore) AddL2BlocksPerBatch(t time.Time, l2Blocks, l2Txs, l1Txs float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.L2BlocksPerBatch, &m.sumL2BlocksPerBatch, "L2 Blocks/Batch", t, l2Blocks)
	m.addAndTrack(m.L2TxsPerBatch, &m.sumL2TxsPerBatch, "L2 Txs/Batch", t, l2Txs)
	m.addAndTrack(m.L1TxsPerBatch, &m.sumL1TxsPerBatch, "L1 Txs/Batch", t, l1Txs)
}

// AddChannelOpenL1Blocks records how many L1 blocks a channel was open (lastL1 - firstL1).
func (m *MetricsStore) AddChannelOpenL1Blocks(t time.Time, blocks float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.ChannelOpenL1Blocks, &m.sumChannelOpenL1Blocks, "Ch Open L1 Blocks", t, blocks)
}

// AddL2ToL1Throughput records L2-to-L1 throughput metrics when a channel completes.
// throughput is txs/second, latency is seconds from earliest L2 block to L1 confirmation.
func (m *MetricsStore) AddL2ToL1Throughput(t time.Time, throughput, latency, txCount float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAndTrack(m.L2ToL1Throughput, &m.sumL2ToL1Throughput, "L2→L1 TPS", t, throughput)
	m.addAndTrack(m.L2ToL1Latency, &m.sumL2ToL1Latency, "L2→L1 Latency", t, latency)
	m.TotalL2TxsFinalized += int64(txCount)
}

// --- Prometheus-sourced metric setters ---

// SetProcessLiveness updates the up/down state for each OP Stack process.
func (m *MetricsStore) SetProcessLiveness(nodeUp, batcherUp, proposerUp bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.NodeUp = nodeUp
	m.BatcherUp = batcherUp
	m.ProposerUp = proposerUp
}

// SetNodeUp updates the op-node liveness state.
func (m *MetricsStore) SetNodeUp(up bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.NodeUp = up
}

// SetBatcherUp updates the op-batcher liveness state.
func (m *MetricsStore) SetBatcherUp(up bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BatcherUp = up
}

// SetProposerUp updates the op-proposer liveness state.
func (m *MetricsStore) SetProposerUp(up bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ProposerUp = up
}

// SetDerivationIdle updates the derivation pipeline idle state.
func (m *MetricsStore) SetDerivationIdle(idle bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DerivationIdle = idle
}

// RecordNodeMetrics records op-node Prometheus counter deltas and gauge values.
// Counters (derivation errors, pipeline resets, sequencing errors) are tracked
// as deltas from the previous scrape.
func (m *MetricsStore) RecordNodeMetrics(t time.Time, derivErrs, pipelineResets, seqErrs, l1Latency, unsafePayloads float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.prevDerivationErrors > 0 {
		delta := derivErrs - m.prevDerivationErrors
		if delta >= 0 {
			m.DerivationErrors.Add(TimeValue{Time: t, Value: delta})
		}
	}
	m.prevDerivationErrors = derivErrs

	if m.prevPipelineResets > 0 {
		delta := pipelineResets - m.prevPipelineResets
		if delta >= 0 {
			m.PipelineResets.Add(TimeValue{Time: t, Value: delta})
		}
	}
	m.prevPipelineResets = pipelineResets

	if m.prevSequencingErrors > 0 {
		delta := seqErrs - m.prevSequencingErrors
		if delta >= 0 {
			m.SequencingErrors.Add(TimeValue{Time: t, Value: delta})
		}
	}
	m.prevSequencingErrors = seqErrs

	m.L1HeadLatency.Add(TimeValue{Time: t, Value: l1Latency})
	m.UnsafePayloadsBuffered.Add(TimeValue{Time: t, Value: unsafePayloads})
}

// RecordBatcherPipelineMetrics records op-batcher pipeline metrics from Prometheus.
func (m *MetricsStore) RecordBatcherPipelineMetrics(t time.Time, pendingBlocks, pendingBytes, channelTimeouts, txFailed, gasBumps, l1BaseFeeWei, balance float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.BatcherPendingBlocks.Add(TimeValue{Time: t, Value: pendingBlocks})
	m.BatcherPendingBytes.Add(TimeValue{Time: t, Value: pendingBytes})

	if m.prevBatcherChannelTimeouts > 0 {
		delta := channelTimeouts - m.prevBatcherChannelTimeouts
		if delta >= 0 {
			m.BatcherChannelTimeouts.Add(TimeValue{Time: t, Value: delta})
		}
	}
	m.prevBatcherChannelTimeouts = channelTimeouts

	if m.prevBatcherTxFailed > 0 {
		delta := txFailed - m.prevBatcherTxFailed
		if delta >= 0 {
			m.BatcherTxFailed.Add(TimeValue{Time: t, Value: delta})
		}
	}
	m.prevBatcherTxFailed = txFailed

	m.BatcherGasBumps.Add(TimeValue{Time: t, Value: gasBumps})

	// Convert wei to gwei for readability
	l1BaseFeeGwei := l1BaseFeeWei / 1e9
	m.BatcherL1BaseFee.Add(TimeValue{Time: t, Value: l1BaseFeeGwei})

	m.BatcherBalance.Add(TimeValue{Time: t, Value: balance})
}

// RecordProposerMetrics records op-proposer Prometheus metrics.
func (m *MetricsStore) RecordProposerMetrics(t time.Time, seqNum, balance float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ProposedSeqNum.Add(TimeValue{Time: t, Value: seqNum})
	m.ProposerBalance.Add(TimeValue{Time: t, Value: balance})
}

// RecordPprofMetrics records heap and goroutine counts from pprof endpoints.
func (m *MetricsStore) RecordPprofMetrics(t time.Time, nodeHeapMB, nodeGoroutines, batcherHeapMB, batcherGoroutines float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if nodeHeapMB >= 0 {
		m.NodeHeapMB.Add(TimeValue{Time: t, Value: nodeHeapMB})
	}
	if nodeGoroutines >= 0 {
		m.NodeGoroutines.Add(TimeValue{Time: t, Value: nodeGoroutines})
	}
	if batcherHeapMB >= 0 {
		m.BatcherHeapMB.Add(TimeValue{Time: t, Value: batcherHeapMB})
	}
	if batcherGoroutines >= 0 {
		m.BatcherGoroutines.Add(TimeValue{Time: t, Value: batcherGoroutines})
	}
}

// SetDepositCost records the gas cost of a deposit operation.
func (m *MetricsStore) SetDepositCost(costRBTC float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LastDepositCostRBTC = costRBTC
	m.RBTCDepositsTriggered++
}

// SetWithdrawCost records the gas cost of RBTC withdrawal steps (L2 init + prove + finalize).
func (m *MetricsStore) SetWithdrawCost(l2InitCost, proveCost, finalizeCost float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LastWithdrawL2InitCostRBTC = l2InitCost
	m.LastProveCostRBTC = proveCost
	m.LastFinalizeCostRBTC = finalizeCost
	m.LastWithdrawCostRBTC = l2InitCost + proveCost + finalizeCost
	m.ExitCostRBTC = l2InitCost + proveCost + finalizeCost
	m.RBTCWithdrawalsTriggered++
}

// SetUSDRIFDepositCost records the gas cost of a USDRIF deposit operation.
func (m *MetricsStore) SetUSDRIFDepositCost(costRBTC float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LastUSDRIFDepositCostRBTC = costRBTC
	m.USDRIFDepositsTriggered++
}

// SetUSDRIFWithdrawCost records the gas cost of USDRIF withdrawal steps (L2 init + prove + finalize).
func (m *MetricsStore) SetUSDRIFWithdrawCost(l2InitCost, proveCost, finalizeCost float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LastUSDRIFWithdrawL2InitCostRBTC = l2InitCost
	m.LastUSDRIFWithdrawProveCostRBTC = proveCost
	m.LastUSDRIFWithdrawFinalizeCostRBTC = finalizeCost
	m.LastUSDRIFWithdrawCostRBTC = l2InitCost + proveCost + finalizeCost
	m.USDRIFWithdrawalsTriggered++
}

// SetTheoreticalMaxTPS records the theoretical max TPS from block gas limit.
func (m *MetricsStore) SetTheoreticalMaxTPS(maxTPS float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.TheoreticalMaxTPS = maxTPS
}

// GetSafeL2 returns the current safe L2 head number under a read lock.
func (m *MetricsStore) GetSafeL2() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.SafeL2
}

// SetSyncStatus updates the unsafe and safe L2 head numbers from op-node.
// It also computes the safe head velocity (blocks/sec) when the safe head advances.
func (m *MetricsStore) SetSyncStatus(unsafeL2, safeL2 uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Compute velocity if we have a previous measurement and safe head advanced.
	// Use an exponential moving average (EMA) to smooth out burst-to-burst
	// variance in safe head advances, which otherwise causes wild ETA swings.
	if m.prevSafeL2 > 0 && !m.prevSafeL2Time.IsZero() && safeL2 > m.prevSafeL2 {
		dt := time.Since(m.prevSafeL2Time).Seconds()
		if dt > 0 {
			instantVel := float64(safeL2-m.prevSafeL2) / dt
			if m.SafeHeadVel == 0 {
				m.SafeHeadVel = instantVel // seed with first observation
			} else {
				const alpha = 0.3
				m.SafeHeadVel = alpha*instantVel + (1-alpha)*m.SafeHeadVel
			}
		}
	}
	// Record reference point when safe head changes
	if safeL2 != m.SafeL2 {
		m.prevSafeL2 = m.SafeL2
		m.prevSafeL2Time = time.Now()
	}
	m.UnsafeL2 = unsafeL2
	m.SafeL2 = safeL2
}

// SetBatcherStaleness updates the batcher staleness (seconds since last post).
// Use -1 to indicate no batcher post has ever been detected.
func (m *MetricsStore) SetBatcherStaleness(seconds float64, lastPost time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BatcherStaleness = seconds
	if !lastPost.IsZero() {
		m.LastBatcherPost = lastPost
	}
}

// SetLatestBlocks updates the latest block numbers.
// When the L1 block advances, a non-blocking signal is sent on L1BlockCh().
func (m *MetricsStore) SetLatestBlocks(l1, l2 uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l1 > 0 && l1 > m.LatestL1Block {
		m.LatestL1Block = l1
		// Notify L1 block observers (non-blocking).
		select {
		case m.l1BlockCh <- struct{}{}:
		default:
		}
	}
	if l2 > 0 {
		m.LatestL2Block = l2
	}
}

// L1BlockCh returns a channel that receives a signal whenever a new L1 block is observed.
// The channel has a buffer of 1; signals may be coalesced if the consumer is slow.
func (m *MetricsStore) L1BlockCh() <-chan struct{} {
	return m.l1BlockCh
}

// SetStatus updates the collector status message (shown in status bar).
func (m *MetricsStore) SetStatus(status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Status = status
}

// SetPendingAndBalances updates the pending tx counts and balances.
func (m *MetricsStore) SetPendingAndBalances(l1Pending, l2Pending uint64, l1Bal, l2Bal float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.L1PendingTxs = l1Pending
	m.L2PendingTxs = l2Pending
	m.L1Balance = l1Bal
	m.L2Balance = l2Bal
}

// SetUSDRIFBalances updates the USDRIF balance fields.
func (m *MetricsStore) SetUSDRIFBalances(l1Bal, l2Bal float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.L1USDRIFBalance = l1Bal
	m.L2USDRIFBalance = l2Bal
}

// AddEvent registers a new pending event and returns its ID.
func (m *MetricsStore) AddEvent(typ, amount string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.eventCounter++
	id := fmt.Sprintf("%s-%d", typ, m.eventCounter)
	m.pendingEvents = append(m.pendingEvents, PendingEvent{
		ID:        id,
		Type:      typ,
		Amount:    amount,
		Status:    EventPending,
		StartTime: time.Now(),
	})
	return id
}

// UpdateEvent updates the status and detail of an existing event.
func (m *MetricsStore) UpdateEvent(id string, status EventStatus, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.pendingEvents {
		if m.pendingEvents[i].ID == id {
			m.pendingEvents[i].Status = status
			m.pendingEvents[i].Detail = detail
			return
		}
	}
}

// UpdateEventTxHash sets the tx hash on an existing event.
func (m *MetricsStore) UpdateEventTxHash(id string, txHash string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.pendingEvents {
		if m.pendingEvents[i].ID == id {
			m.pendingEvents[i].TxHash = txHash
			return
		}
	}
}

// ClearCompletedEvents removes events with status complete or failed.
func (m *MetricsStore) ClearCompletedEvents() {
	m.mu.Lock()
	defer m.mu.Unlock()
	filtered := m.pendingEvents[:0]
	for _, e := range m.pendingEvents {
		if e.Status != EventComplete && e.Status != EventFailed {
			filtered = append(filtered, e)
		}
	}
	m.pendingEvents = filtered
}

// ExpireOldEvents removes completed/failed events older than the given duration.
func (m *MetricsStore) ExpireOldEvents(maxAge time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	filtered := m.pendingEvents[:0]
	for _, e := range m.pendingEvents {
		if (e.Status == EventComplete || e.Status == EventFailed) && now.Sub(e.StartTime) > maxAge {
			continue
		}
		filtered = append(filtered, e)
	}
	m.pendingEvents = filtered
}

// SetLogFile sets the file writer for persisting log entries to disk.
// Must be called before any AppendLog calls (typically right after NewMetricsStore).
func (m *MetricsStore) SetLogFile(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logFile = w
}

// SetStatsFile sets the CSV file writer for stats export. Writes the CSV header.
func (m *MetricsStore) SetStatsFile(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statsFile = w
	// Write CSV header
	fmt.Fprintln(w, "timestamp,l2_block,l1_block,tps,tps_rolling,tps_simulator,tps_window_s,l2_tx_cost_rbtc,l2_tx_speed_s,l2_tx_count,l1_tx_count,posting_freq_s,deposits,withdrawals,total_l2_txs,total_deposits,total_withdrawals,l1_pending_txs,l2_pending_txs,l1_balance_rbtc,l2_balance_rbtc,safe_head_lag,unsafe_l2,safe_l2,batcher_post_cost_rbtc,batcher_data_size_bytes,compression_ratio,compression_ratio_delta,deriv_errors_delta,pipeline_resets_delta,seq_errors_delta,l1_head_latency_s,batcher_pending_blocks,batcher_pending_bytes,batcher_l1_basefee_gwei,batcher_balance_rbtc,proposed_seq_num,proposer_balance_rbtc,total_batcher_data_bytes")
}

// SetStatsFileNoHeader sets the CSV file writer without writing a header (for appending to existing files).
func (m *MetricsStore) SetStatsFileNoHeader(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statsFile = w
}

// WriteStatsRow appends a CSV row with the latest metric values.
// Called from the collector after processing a new L2 block.
func (m *MetricsStore) WriteStatsRow(t time.Time, l2Block uint64, tps, l2TxCost, l2TxSpeed, l2TxCount float64, deposits, withdrawals int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.statsFile == nil {
		return
	}

	// Get latest rolling and simulator TPS from ring buffers
	var tpsRolling, tpsSimulator float64
	if last, ok := m.TPSRolling.Last(); ok {
		tpsRolling = last.Value
	}
	if last, ok := m.TPSSimulator.Last(); ok {
		tpsSimulator = last.Value
	}
	tpsWindowS := m.tpsWindowDuration.Seconds()

	// Get latest L1 tx count, posting freq, and safe head lag from ring buffers
	var l1TxCount, postFreq, safeHeadLag float64
	if last, ok := m.L1TxCount.Last(); ok {
		l1TxCount = last.Value
	}
	if last, ok := m.PostingFreq.Last(); ok {
		postFreq = last.Value
	}
	if last, ok := m.SafeHeadLag.Last(); ok {
		safeHeadLag = last.Value
	}
	var batcherPostCost, batcherDataSize, compressionRatio float64
	if last, ok := m.BatcherPostCost.Last(); ok {
		batcherPostCost = last.Value
	}
	if last, ok := m.BatcherDataSize.Last(); ok {
		batcherDataSize = last.Value
	}
	if last, ok := m.CompressionRatio.Last(); ok {
		compressionRatio = last.Value
	}
	var compressionRatioDelta float64
	if last, ok := m.CompressionRatioDelta.Last(); ok {
		compressionRatioDelta = last.Value
	}

	// Prometheus-sourced metrics (new columns, 0 when not configured)
	var derivErrs, pipeResets, seqErrs, l1Latency float64
	var pendBlocks, pendBytes, l1BaseFee, batcherBal float64
	var propSeqNum, propBal float64
	if last, ok := m.DerivationErrors.Last(); ok {
		derivErrs = last.Value
	}
	if last, ok := m.PipelineResets.Last(); ok {
		pipeResets = last.Value
	}
	if last, ok := m.SequencingErrors.Last(); ok {
		seqErrs = last.Value
	}
	if last, ok := m.L1HeadLatency.Last(); ok {
		l1Latency = last.Value
	}
	if last, ok := m.BatcherPendingBlocks.Last(); ok {
		pendBlocks = last.Value
	}
	if last, ok := m.BatcherPendingBytes.Last(); ok {
		pendBytes = last.Value
	}
	if last, ok := m.BatcherL1BaseFee.Last(); ok {
		l1BaseFee = last.Value
	}
	if last, ok := m.BatcherBalance.Last(); ok {
		batcherBal = last.Value
	}
	if last, ok := m.ProposedSeqNum.Last(); ok {
		propSeqNum = last.Value
	}
	if last, ok := m.ProposerBalance.Last(); ok {
		propBal = last.Value
	}

	fmt.Fprintf(m.statsFile, "%s,%d,%d,%.4f,%.4f,%.4f,%.0f,%.12f,%.4f,%.0f,%.0f,%.2f,%d,%d,%d,%d,%d,%d,%d,%.8f,%.8f,%.0f,%d,%d,%.12f,%.0f,%.4f,%.4f,%.0f,%.0f,%.0f,%.2f,%.0f,%.0f,%.4f,%.8f,%.0f,%.8f,%.0f\n",
		t.Format("2006-01-02T15:04:05.000Z"),
		l2Block,
		m.LatestL1Block,
		tps,
		tpsRolling,
		tpsSimulator,
		tpsWindowS,
		l2TxCost,
		l2TxSpeed,
		l2TxCount,
		l1TxCount,
		postFreq,
		deposits,
		withdrawals,
		m.TotalL2Txs,
		m.TotalDeposits,
		m.TotalWithdrawals,
		m.L1PendingTxs,
		m.L2PendingTxs,
		m.L1Balance,
		m.L2Balance,
		safeHeadLag,
		m.UnsafeL2,
		m.SafeL2,
		batcherPostCost,
		batcherDataSize,
		compressionRatio,
		compressionRatioDelta,
		derivErrs,
		pipeResets,
		seqErrs,
		l1Latency,
		pendBlocks,
		pendBytes,
		l1BaseFee,
		batcherBal,
		propSeqNum,
		propBal,
		m.TotalBatcherDataBytes,
	)
}

// AppendLog adds a timestamped log message to the action log and writes it to the log file.
func (m *MetricsStore) AppendLog(format string, args ...interface{}) {
	now := time.Now()
	msg := fmt.Sprintf(format, args...)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Log.Add(LogEntry{Time: now, Message: msg})
	if m.logFile != nil {
		fmt.Fprintf(m.logFile, "%s  %s\n", now.Format("2006-01-02 15:04:05.000"), msg)
	}
}

// LogLen returns the total number of log entries ever added (monotonically increasing).
func (m *MetricsStore) LogLen() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Log.Len()
}

// LogEntriesSince returns log entries added after the given monotonic index.
func (m *MetricsStore) LogEntriesSince(since int) []LogEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Log.EntriesSince(since)
}

// Snapshot returns a read-only copy of all current metrics data for rendering.
type Snapshot struct {
	TPS                   []TimeValue
	TPSRolling            []TimeValue
	TPSSimulator          []TimeValue
	L2TxCost              []TimeValue
	L2TxSpeed             []TimeValue
	L2TxCount             []TimeValue
	L1TxCount             []TimeValue
	PostingFreq           []TimeValue
	Deposits              []TimeValue
	Withdrawals           []TimeValue
	SafeHeadLag           []TimeValue
	BatcherPostCost       []TimeValue
	BatcherL1GasSpendWei  []TimeValue
	BatcherL1GasPriceWei  []TimeValue
	BatcherDataSize       []TimeValue
	CompressionRatio      []TimeValue
	CompressionRatioDelta []TimeValue
	TxPoolPending         []TimeValue
	TxPoolQueued          []TimeValue

	// L2 blocks per batch (decoded from batcher L1 tx calldata)
	L2BlocksPerBatch []TimeValue
	L2TxsPerBatch    []TimeValue
	L1TxsPerBatch    []TimeValue

	// Channel open duration (L1 blocks from first to last frame)
	ChannelOpenL1Blocks []TimeValue

	// L2 to L1 throughput (computed when channel completes)
	L2ToL1Throughput []TimeValue
	L2ToL1Latency    []TimeValue

	// Prometheus-sourced time series
	DerivationErrors       []TimeValue
	PipelineResets         []TimeValue
	SequencingErrors       []TimeValue
	L1HeadLatency          []TimeValue
	UnsafePayloadsBuffered []TimeValue
	BatcherPendingBlocks   []TimeValue
	BatcherPendingBytes    []TimeValue
	BatcherChannelTimeouts []TimeValue
	BatcherTxFailed        []TimeValue
	BatcherGasBumps        []TimeValue
	BatcherL1BaseFee       []TimeValue
	BatcherBalanceTS       []TimeValue
	ProposedSeqNum         []TimeValue
	ProposerBalanceTS      []TimeValue
	NodeHeapMB             []TimeValue
	NodeGoroutines         []TimeValue
	BatcherHeapMB          []TimeValue
	BatcherGoroutines      []TimeValue

	// Process liveness
	NodeUp     bool
	BatcherUp  bool
	ProposerUp bool

	// Derivation state
	DerivationIdle bool

	// Per-metric summaries (peak + avg)
	SumTPS                  SummarySnapshot
	SumTPSRolling           SummarySnapshot
	SumTPSSimulator         SummarySnapshot
	SumL2Cost               SummarySnapshot
	SumL2Speed              SummarySnapshot
	SumL2Count              SummarySnapshot
	SumL1Count              SummarySnapshot
	SumPostFreq             SummarySnapshot
	SumDeposits             SummarySnapshot
	SumWithdraw             SummarySnapshot
	SumSafeHeadLag          SummarySnapshot
	SumBatcherPostCost      SummarySnapshot
	SumBatcherL1GasSpendWei SummarySnapshot
	SumBatcherL1GasPriceWei SummarySnapshot
	SumBatcherDataSize      SummarySnapshot
	SumCompressionRatio     SummarySnapshot
	SumTxPoolPending        SummarySnapshot
	SumTxPoolQueued         SummarySnapshot
	SumL2BlocksPerBatch     SummarySnapshot
	SumL2TxsPerBatch        SummarySnapshot
	SumL1TxsPerBatch        SummarySnapshot
	SumChannelOpenL1Blocks  SummarySnapshot
	SumL2ToL1Throughput     SummarySnapshot
	SumL2ToL1Latency        SummarySnapshot

	TotalDeposits         int64
	TotalWithdrawals      int64
	TotalL2Txs            int64
	TotalBatcherCostRBTC  float64
	TotalBatcherDataBytes float64
	TotalBatcherPosts     int64
	TotalBatcherBlocks    int64
	TotalL2TxsFinalized   int64

	LatestL1Block uint64
	LatestL2Block uint64

	// Sync status from op-node
	UnsafeL2    uint64
	SafeL2      uint64
	SafeHeadVel float64 // blocks/sec safe head is advancing

	// Batcher staleness: seconds since last post (-1 = never seen)
	BatcherStaleness float64
	LastBatcherPost  time.Time

	L1PendingTxs    uint64
	L2PendingTxs    uint64
	L1Balance       float64
	L2Balance       float64
	L1USDRIFBalance float64
	L2USDRIFBalance float64

	// Bridge and exit costs
	LastDepositCostRBTC                float64
	LastWithdrawCostRBTC               float64
	LastWithdrawL2InitCostRBTC         float64
	LastProveCostRBTC                  float64
	LastFinalizeCostRBTC               float64
	LastUSDRIFDepositCostRBTC          float64
	LastUSDRIFWithdrawCostRBTC         float64
	LastUSDRIFWithdrawL2InitCostRBTC   float64
	LastUSDRIFWithdrawProveCostRBTC    float64
	LastUSDRIFWithdrawFinalizeCostRBTC float64
	ExitCostRBTC                       float64

	// Bridge operation counters
	RBTCDepositsTriggered      int64
	RBTCWithdrawalsTriggered   int64
	USDRIFDepositsTriggered    int64
	USDRIFWithdrawalsTriggered int64

	// Theoretical max TPS
	TheoreticalMaxTPS float64

	// Rolling TPS window duration
	TPSWindowDuration time.Duration

	Status string
	Log    []LogEntry

	PendingEvents []PendingEvent
}

// summarySnap creates a SummarySnapshot from a MetricSummary.
// If useNonZeroAvg is true, uses the non-zero average (for cost/speed metrics
// where 0 means "no data" rather than "zero value").
func summarySnap(s *MetricSummary, useNonZeroAvg bool) SummarySnapshot {
	avg := s.Average()
	if useNonZeroAvg {
		avg = s.NonZeroAverage()
	}
	return SummarySnapshot{
		Peak:    s.Peak,
		PeakCtx: s.PeakCtx,
		Avg:     avg,
	}
}

// MetricValues returns a flat map of current metric values for CSV export.
// Each key matches the enriched CSV column name. Values are sampled under a
// single read lock to ensure consistency.
func (m *MetricsStore) MetricValues() map[string]float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	vals := make(map[string]float64, 24)

	lastVal := func(rb *RingBuffer) float64 {
		if last, ok := rb.Last(); ok {
			return last.Value
		}
		return 0
	}

	vals["tps"] = lastVal(m.TPS)
	vals["l2_tx_cost_rbtc"] = lastVal(m.L2TxCost)
	vals["l2_tx_cost_min_rbtc"] = lastVal(m.L2TxCostMin)
	vals["l2_tx_cost_max_rbtc"] = lastVal(m.L2TxCostMax)
	vals["l2_tx_speed_s"] = lastVal(m.L2TxSpeed)
	vals["l2_tx_count"] = lastVal(m.L2TxCount)
	vals["l1_tx_count"] = lastVal(m.L1TxCount)
	vals["posting_freq_s"] = lastVal(m.PostingFreq)
	vals["deposits"] = lastVal(m.Deposits)
	vals["withdrawals"] = lastVal(m.Withdrawals)
	vals["safe_head_lag"] = lastVal(m.SafeHeadLag)
	vals["batcher_post_cost_rbtc"] = lastVal(m.BatcherPostCost)
	vals["batcher_l1_gas_spend_wei"] = lastVal(m.BatcherL1GasSpendWei)
	vals["batcher_l1_gas_price_wei"] = lastVal(m.BatcherL1GasPriceWei)
	vals["batcher_data_size_bytes"] = lastVal(m.BatcherDataSize)
	vals["compression_ratio"] = lastVal(m.CompressionRatio)
	vals["compression_ratio_delta"] = lastVal(m.CompressionRatioDelta)
	vals["total_l2_txs"] = float64(m.TotalL2Txs)
	vals["total_batcher_cost_rbtc"] = m.TotalBatcherCostRBTC
	vals["total_batcher_data_bytes"] = m.TotalBatcherDataBytes
	vals["total_batcher_posts"] = float64(m.TotalBatcherPosts)
	vals["total_batcher_blocks"] = float64(m.TotalBatcherBlocks)
	vals["total_l2_txs_finalized"] = float64(m.TotalL2TxsFinalized)
	vals["total_deposits"] = float64(m.TotalDeposits)
	vals["total_withdrawals"] = float64(m.TotalWithdrawals)
	vals["unsafe_l2"] = float64(m.UnsafeL2)
	vals["safe_l2"] = float64(m.SafeL2)
	vals["l1_balance_rbtc"] = m.L1Balance
	vals["l2_balance_rbtc"] = m.L2Balance
	vals["latest_l1_block"] = float64(m.LatestL1Block)
	vals["latest_l2_block"] = float64(m.LatestL2Block)
	vals["theoretical_max_tps"] = m.TheoreticalMaxTPS
	vals["last_deposit_cost_rbtc"] = m.LastDepositCostRBTC
	vals["exit_cost_rbtc"] = m.ExitCostRBTC
	vals["txpool_pending"] = lastVal(m.TxPoolPending)
	vals["txpool_queued"] = lastVal(m.TxPoolQueued)
	vals["l2_blocks_per_batch"] = lastVal(m.L2BlocksPerBatch)
	vals["l2_txs_per_batch"] = lastVal(m.L2TxsPerBatch)
	vals["l1_txs_per_batch"] = lastVal(m.L1TxsPerBatch)
	vals["channel_open_l1_blocks"] = lastVal(m.ChannelOpenL1Blocks)
	vals["l2_to_l1_throughput"] = lastVal(m.L2ToL1Throughput)
	vals["l2_to_l1_latency"] = lastVal(m.L2ToL1Latency)

	// Prometheus-sourced metrics
	vals["deriv_errors_delta"] = lastVal(m.DerivationErrors)
	vals["pipeline_resets_delta"] = lastVal(m.PipelineResets)
	vals["seq_errors_delta"] = lastVal(m.SequencingErrors)
	vals["l1_head_latency_s"] = lastVal(m.L1HeadLatency)
	vals["batcher_pending_blocks"] = lastVal(m.BatcherPendingBlocks)
	vals["batcher_pending_bytes"] = lastVal(m.BatcherPendingBytes)
	vals["batcher_l1_basefee_gwei"] = lastVal(m.BatcherL1BaseFee)
	vals["batcher_balance_rbtc"] = lastVal(m.BatcherBalance)
	vals["batcher_channel_timeouts"] = lastVal(m.BatcherChannelTimeouts)
	vals["batcher_tx_failed"] = lastVal(m.BatcherTxFailed)
	vals["batcher_gas_bumps"] = lastVal(m.BatcherGasBumps)
	vals["proposed_seq_num"] = lastVal(m.ProposedSeqNum)
	vals["proposer_balance_rbtc"] = lastVal(m.ProposerBalance)

	return vals
}

// GetSnapshot returns a consistent snapshot of all metrics under a single read lock.
func (m *MetricsStore) GetSnapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Snapshot{
		TPS:                                m.TPS.Entries(),
		TPSRolling:                         m.TPSRolling.Entries(),
		TPSSimulator:                       m.TPSSimulator.Entries(),
		L2TxCost:                           m.L2TxCost.Entries(),
		L2TxSpeed:                          m.L2TxSpeed.Entries(),
		L2TxCount:                          m.L2TxCount.Entries(),
		L1TxCount:                          m.L1TxCount.Entries(),
		PostingFreq:                        m.PostingFreq.Entries(),
		Deposits:                           m.Deposits.Entries(),
		Withdrawals:                        m.Withdrawals.Entries(),
		SafeHeadLag:                        m.SafeHeadLag.Entries(),
		BatcherPostCost:                    m.BatcherPostCost.Entries(),
		BatcherL1GasSpendWei:               m.BatcherL1GasSpendWei.Entries(),
		BatcherL1GasPriceWei:               m.BatcherL1GasPriceWei.Entries(),
		BatcherDataSize:                    m.BatcherDataSize.Entries(),
		CompressionRatio:                   m.CompressionRatio.Entries(),
		CompressionRatioDelta:              m.CompressionRatioDelta.Entries(),
		TxPoolPending:                      m.TxPoolPending.Entries(),
		TxPoolQueued:                       m.TxPoolQueued.Entries(),
		L2BlocksPerBatch:                   m.L2BlocksPerBatch.Entries(),
		L2TxsPerBatch:                      m.L2TxsPerBatch.Entries(),
		L1TxsPerBatch:                      m.L1TxsPerBatch.Entries(),
		ChannelOpenL1Blocks:                m.ChannelOpenL1Blocks.Entries(),
		L2ToL1Throughput:                   m.L2ToL1Throughput.Entries(),
		L2ToL1Latency:                      m.L2ToL1Latency.Entries(),
		DerivationErrors:                   m.DerivationErrors.Entries(),
		PipelineResets:                     m.PipelineResets.Entries(),
		SequencingErrors:                   m.SequencingErrors.Entries(),
		L1HeadLatency:                      m.L1HeadLatency.Entries(),
		UnsafePayloadsBuffered:             m.UnsafePayloadsBuffered.Entries(),
		BatcherPendingBlocks:               m.BatcherPendingBlocks.Entries(),
		BatcherPendingBytes:                m.BatcherPendingBytes.Entries(),
		BatcherChannelTimeouts:             m.BatcherChannelTimeouts.Entries(),
		BatcherTxFailed:                    m.BatcherTxFailed.Entries(),
		BatcherGasBumps:                    m.BatcherGasBumps.Entries(),
		BatcherL1BaseFee:                   m.BatcherL1BaseFee.Entries(),
		BatcherBalanceTS:                   m.BatcherBalance.Entries(),
		ProposedSeqNum:                     m.ProposedSeqNum.Entries(),
		ProposerBalanceTS:                  m.ProposerBalance.Entries(),
		NodeHeapMB:                         m.NodeHeapMB.Entries(),
		NodeGoroutines:                     m.NodeGoroutines.Entries(),
		BatcherHeapMB:                      m.BatcherHeapMB.Entries(),
		BatcherGoroutines:                  m.BatcherGoroutines.Entries(),
		NodeUp:                             m.NodeUp,
		BatcherUp:                          m.BatcherUp,
		ProposerUp:                         m.ProposerUp,
		DerivationIdle:                     m.DerivationIdle,
		SumTPS:                             summarySnap(&m.sumTPS, false),
		SumTPSRolling:                      summarySnap(&m.sumTPSRolling, false),
		SumTPSSimulator:                    summarySnap(&m.sumTPSSimulator, false),
		SumL2Cost:                          summarySnap(&m.sumL2Cost, true),
		SumL2Speed:                         summarySnap(&m.sumL2Speed, true),
		SumL2Count:                         summarySnap(&m.sumL2Count, false),
		SumL1Count:                         summarySnap(&m.sumL1Count, false),
		SumPostFreq:                        summarySnap(&m.sumPostFreq, true),
		SumDeposits:                        summarySnap(&m.sumDeposits, false),
		SumWithdraw:                        summarySnap(&m.sumWithdraw, false),
		SumSafeHeadLag:                     summarySnap(&m.sumSafeHeadLag, false),
		SumBatcherPostCost:                 summarySnap(&m.sumBatcherPostCost, true),
		SumBatcherL1GasSpendWei:            summarySnap(&m.sumBatcherL1GasSpendWei, true),
		SumBatcherL1GasPriceWei:            summarySnap(&m.sumBatcherL1GasPriceWei, true),
		SumBatcherDataSize:                 summarySnap(&m.sumBatcherDataSize, true),
		SumCompressionRatio:                summarySnap(&m.sumCompressionRatio, true),
		SumTxPoolPending:                   summarySnap(&m.sumTxPoolPending, false),
		SumTxPoolQueued:                    summarySnap(&m.sumTxPoolQueued, false),
		SumL2BlocksPerBatch:                summarySnap(&m.sumL2BlocksPerBatch, false),
		SumL2TxsPerBatch:                   summarySnap(&m.sumL2TxsPerBatch, false),
		SumL1TxsPerBatch:                   summarySnap(&m.sumL1TxsPerBatch, false),
		SumChannelOpenL1Blocks:             summarySnap(&m.sumChannelOpenL1Blocks, false),
		SumL2ToL1Throughput:                summarySnap(&m.sumL2ToL1Throughput, false),
		SumL2ToL1Latency:                   summarySnap(&m.sumL2ToL1Latency, false),
		TotalDeposits:                      m.TotalDeposits,
		TotalWithdrawals:                   m.TotalWithdrawals,
		TotalL2Txs:                         m.TotalL2Txs,
		TotalBatcherCostRBTC:               m.TotalBatcherCostRBTC,
		TotalBatcherDataBytes:              m.TotalBatcherDataBytes,
		TotalBatcherPosts:                  m.TotalBatcherPosts,
		TotalBatcherBlocks:                 m.TotalBatcherBlocks,
		TotalL2TxsFinalized:                m.TotalL2TxsFinalized,
		LatestL1Block:                      m.LatestL1Block,
		LatestL2Block:                      m.LatestL2Block,
		UnsafeL2:                           m.UnsafeL2,
		SafeL2:                             m.SafeL2,
		SafeHeadVel:                        m.SafeHeadVel,
		BatcherStaleness:                   m.BatcherStaleness,
		LastBatcherPost:                    m.LastBatcherPost,
		L1PendingTxs:                       m.L1PendingTxs,
		L2PendingTxs:                       m.L2PendingTxs,
		L1Balance:                          m.L1Balance,
		L2Balance:                          m.L2Balance,
		L1USDRIFBalance:                    m.L1USDRIFBalance,
		L2USDRIFBalance:                    m.L2USDRIFBalance,
		LastDepositCostRBTC:                m.LastDepositCostRBTC,
		LastWithdrawCostRBTC:               m.LastWithdrawCostRBTC,
		LastWithdrawL2InitCostRBTC:         m.LastWithdrawL2InitCostRBTC,
		LastProveCostRBTC:                  m.LastProveCostRBTC,
		LastFinalizeCostRBTC:               m.LastFinalizeCostRBTC,
		LastUSDRIFDepositCostRBTC:          m.LastUSDRIFDepositCostRBTC,
		LastUSDRIFWithdrawCostRBTC:         m.LastUSDRIFWithdrawCostRBTC,
		LastUSDRIFWithdrawL2InitCostRBTC:   m.LastUSDRIFWithdrawL2InitCostRBTC,
		LastUSDRIFWithdrawProveCostRBTC:    m.LastUSDRIFWithdrawProveCostRBTC,
		LastUSDRIFWithdrawFinalizeCostRBTC: m.LastUSDRIFWithdrawFinalizeCostRBTC,
		ExitCostRBTC:                       m.ExitCostRBTC,
		RBTCDepositsTriggered:              m.RBTCDepositsTriggered,
		RBTCWithdrawalsTriggered:           m.RBTCWithdrawalsTriggered,
		USDRIFDepositsTriggered:            m.USDRIFDepositsTriggered,
		USDRIFWithdrawalsTriggered:         m.USDRIFWithdrawalsTriggered,
		TheoreticalMaxTPS:                  m.TheoreticalMaxTPS,
		TPSWindowDuration:                  m.tpsWindowDuration,
		Status:                             m.Status,
		Log:                                m.Log.Entries(),
		PendingEvents:                      append([]PendingEvent(nil), m.pendingEvents...),
	}
}

const maxFundingTxs = 10000

// RegisterFundingTx records a transaction hash as an internal funding transfer
// so the collector can exclude it from user-tx metrics.
func (m *MetricsStore) RegisterFundingTx(hash common.Hash) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.fundingTxs) >= maxFundingTxs {
		// Crude pruning: clear the set when it gets too large.
		// Funding hashes are only relevant for a few seconds between send
		// and block inclusion, so stale entries are harmless to discard.
		m.fundingTxs = make(map[common.Hash]struct{}, maxFundingTxs/2)
	}
	m.fundingTxs[hash] = struct{}{}
}

// IsFundingTx reports whether the given tx hash was registered as an internal
// funding transfer by the traffic simulator.
func (m *MetricsStore) IsFundingTx(hash common.Hash) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.fundingTxs[hash]
	return ok
}

const maxTrackedTxs = 50000

// RegisterTxSendTime records the send time for a tx hash. Called by traffic
// simulator when a tx is submitted to L2. Used for accurate L2-to-L1 latency.
func (m *MetricsStore) RegisterTxSendTime(hash common.Hash, sendTime time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.trackedTxSendTimes) >= maxTrackedTxs {
		// Prune old entries when map gets too large
		m.trackedTxSendTimes = make(map[common.Hash]time.Time, maxTrackedTxs/2)
	}
	m.trackedTxSendTimes[hash] = sendTime
}

// GetTxSendTime returns the send time for a tx hash, if tracked.
func (m *MetricsStore) GetTxSendTime(hash common.Hash) (time.Time, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.trackedTxSendTimes[hash]
	return t, ok
}

// RemoveTrackedTx removes a tx hash from the tracking map after it's been processed.
func (m *MetricsStore) RemoveTrackedTx(hash common.Hash) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.trackedTxSendTimes, hash)
}
