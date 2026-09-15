package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum-optimism/optimism/op-node/rollup/derive"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/fedejinich/rootstack-operator-tools/rsk/opdeployer/redact"
)

const weiPerRBTC = 1e18

// L2ToL1MessagePasser predeploy address (receives withdrawal txs).
var l2ToL1MessagePasserAddr = common.HexToAddress("0x4200000000000000000000000000000000000016")

// maxRLPBytesPerChannelBedrock is the Bedrock-era limit for decoded channel data.
const maxRLPBytesPerChannelBedrock = 10_000_000

// pendingChannel tracks frames for a channel that is being assembled across
// one or more L1 transactions.
type pendingChannel struct {
	channel  *derive.Channel
	l1Txs    int    // number of L1 txs that contributed frames
	firstL1  uint64 // first L1 block number containing a frame
	lastL1   uint64 // most recent L1 block containing a frame
	openedAt time.Time
}

// Collector polls L1 and L2 for new blocks and extracts metrics.
type Collector struct {
	ctx     context.Context
	cfg     *Config
	metrics *MetricsStore
	l1      *ethclient.Client
	l2      *ethclient.Client
	nodeRPC *rpc.Client // op-node RPC for optimism_syncStatus

	lastL2Block uint64
	lastL1Block uint64

	// For posting frequency tracking
	lastBatcherTxTime time.Time

	// L1 addresses for batcher detection
	batchInbox    common.Address
	batcherAddr   common.Address
	depositPortal common.Address
	// Additional rollup-related L1 addresses used to filter l1_tx_count to
	// rollup traffic only (batcher posts, deposits, withdrawals, proposer outputs).
	l1StdBridge   common.Address
	disputeFactor common.Address

	// Account address for balance/pending tracking (derived from private key)
	account common.Address

	// Backfill configuration (set before Run is called)
	startL2Block  uint64 // if > 0, resume from this L2 block (from CSV)
	startL1Block  uint64 // if > 0, resume from this L1 block (from CSV)
	historyBlocks int    // if > 0, backfill the last N L2 blocks on startup

	// Enhanced diagnostics for batch inbox detection
	l1DiagBlocks         int                    // L1 blocks with txs processed for diagnostics
	l1DiagTxsScanned     int                    // total L1 txs scanned for diagnostics
	l1DiagDestCounts     map[common.Address]int // tx destination counts for diagnostics
	l1DiagSummaryLogged  bool                   // whether the diagnostic summary has been logged
	batcherWarningLogged bool                   // whether the "no batcher" warning has been logged

	// Channel assembly for L2-blocks-per-batch decoding
	pendingChannels map[derive.ChannelID]*pendingChannel
}

// NewCollector creates a new Collector. Dials L1 and L2 RPCs.
func NewCollector(ctx context.Context, cfg *Config, metrics *MetricsStore) (*Collector, error) {
	c := &Collector{
		ctx:              ctx,
		cfg:              cfg,
		metrics:          metrics,
		l1DiagDestCounts: make(map[common.Address]int),
		pendingChannels:  make(map[derive.ChannelID]*pendingChannel),
	}

	// Dial L2
	l2, err := ethclient.DialContext(ctx, cfg.L2RPC)
	if err != nil {
		return nil, fmt.Errorf("dial L2 (%s): %w", cfg.L2RPC, err)
	}
	c.l2 = l2

	// Dial L1 (optional: some metrics won't work without it)
	if cfg.L1RPC != "" {
		l1, err := ethclient.DialContext(ctx, cfg.L1RPC)
		if err != nil {
			metrics.SetStatus(fmt.Sprintf("L2 connected, L1 dial failed: %v", err))
		} else {
			c.l1 = l1
		}
	}

	// Dial op-node RPC for sync status (optional: safe head tracking won't work without it)
	if cfg.NodeRPC != "" {
		nodeClient, err := rpc.DialContext(ctx, cfg.NodeRPC)
		if err != nil {
			metrics.AppendLog("[collector] op-node dial failed (%s): %v — safe head tracking disabled", redact.URL(cfg.NodeRPC), err)
		} else {
			c.nodeRPC = nodeClient
			metrics.AppendLog("[collector] op-node connected (%s) — safe head tracking enabled", redact.URL(cfg.NodeRPC))
		}
	}

	// Set up batcher detection addresses from rollup config
	if cfg.Rollup != nil {
		c.batchInbox = cfg.Rollup.BatchInbox()
		c.batcherAddr = cfg.Rollup.BatcherAddress()
		c.depositPortal = cfg.Rollup.DepositContract()
		if c.batchInbox == (common.Address{}) {
			metrics.AppendLog("[collector] WARNING: batch_inbox_address is zero — rollup.json may be missing or malformed")
		} else {
			metrics.AppendLog("[collector] batchInbox=%s batcher=%s", c.batchInbox.Hex(), c.batcherAddr.Hex())
		}
	} else {
		metrics.AppendLog("[collector] no rollup config: batcher detection disabled")
	}
	// L1 contracts (from l1.json) used to filter l1_tx_count to rollup-related txs.
	c.l1StdBridge = cfg.L1Addrs.L1StandardBridge()
	c.disputeFactor = cfg.L1Addrs.DisputeGameFactory()

	// Derive account address from private key for balance/pending tracking
	if cfg.PrivateKey != "" {
		key, err := crypto.HexToECDSA(cfg.PrivateKey)
		if err == nil {
			c.account = crypto.PubkeyToAddress(key.PublicKey)
		}
	}

	metrics.SetStatus("Connected. Waiting for blocks...")
	return c, nil
}

// SetStartBlocks sets the starting block numbers (typically from a CSV import).
// Must be called before Run.
func (c *Collector) SetStartBlocks(l2, l1 uint64) {
	c.startL2Block = l2
	c.startL1Block = l1
}

// SetHistoryBlocks sets the number of L2 blocks to backfill on startup.
// Must be called before Run.
func (c *Collector) SetHistoryBlocks(n int) {
	c.historyBlocks = n
}

// Run starts the main polling loops. Blocks until ctx is cancelled.
// If start blocks or history blocks are configured, it backfills
// historical data before entering the live polling loop.
func (c *Collector) Run() {
	// Perform backfill before live polling
	c.backfill()

	l2Ticker := time.NewTicker(c.cfg.PollInterval)
	defer l2Ticker.Stop()

	// L1 polls less frequently (5x poll interval)
	l1Interval := c.cfg.PollInterval * 5
	l1Ticker := time.NewTicker(l1Interval)
	defer l1Ticker.Stop()

	// Prometheus metrics poll on L2 cadence (fast), pprof on 30s cadence (slow)
	pprofInterval := 30 * time.Second
	pprofTicker := time.NewTicker(pprofInterval)
	defer pprofTicker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-l2Ticker.C:
			c.pollL2()
			c.pollNodeMetrics()
			c.pollBatcherMetrics()
		case <-l1Ticker.C:
			c.pollL1()
			c.pollProposerMetrics()
		case <-pprofTicker.C:
			c.pollPprof()
		}
	}
}

// backfill replays historical L2 (and L1) blocks on startup so the charts
// start with data instead of being empty.
func (c *Collector) backfill() {
	// Query theoretical max TPS on startup
	c.queryTheoreticalMaxTPS()

	// Get current chain heights
	l2Height, err := c.l2.BlockNumber(c.ctx)
	if err != nil {
		c.metrics.AppendLog("[backfill] failed to get L2 height: %v", err)
		return
	}

	var l1Height uint64
	if c.l1 != nil {
		h, err := c.l1.BlockNumber(c.ctx)
		if err == nil {
			l1Height = h
		}
	}

	// Determine L2 start block. When both CSV (startL2Block) and
	// history_blocks are configured, use the wider (earlier) range so
	// the user gets the full requested replay depth.
	var fromL2 uint64
	hasCSV := c.startL2Block > 0
	hasHistory := c.historyBlocks > 0

	if hasCSV || hasHistory {
		fromL2 = l2Height // start high, then pick the earlier candidate

		if hasCSV {
			csvFrom := c.startL2Block + 1
			if csvFrom < fromL2 {
				fromL2 = csvFrom
			}
		}
		if hasHistory {
			var histFrom uint64
			if l2Height > uint64(c.historyBlocks) {
				histFrom = l2Height - uint64(c.historyBlocks)
			} else {
				histFrom = 1
			}
			if histFrom < fromL2 {
				fromL2 = histFrom
			}
		}
	} else {
		// No backfill configured — set baseline at current height
		c.lastL2Block = l2Height
		c.metrics.SetLatestBlocks(l1Height, l2Height)
		if c.l1 != nil {
			c.lastL1Block = l1Height
		}
		return
	}

	if fromL2 > l2Height {
		// CSV is ahead of (or at) chain head — nothing to backfill
		c.lastL2Block = l2Height
		c.metrics.SetLatestBlocks(l1Height, l2Height)
		if c.l1 != nil {
			c.lastL1Block = l1Height
		}
		return
	}

	numL2Blocks := l2Height - fromL2 + 1
	c.metrics.AppendLog("[backfill] replaying L2 #%d..#%d (%d blocks)", fromL2, l2Height, numL2Blocks)
	c.metrics.SetStatus(fmt.Sprintf("Backfilling L2 #%d..#%d...", fromL2, l2Height))

	// Process L2 blocks
	for blockNum := fromL2; blockNum <= l2Height; blockNum++ {
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		c.processL2Block(blockNum)

		// Log progress every 100 blocks
		if (blockNum-fromL2)%100 == 99 {
			c.metrics.SetStatus(fmt.Sprintf("Backfilling L2 #%d/%d...", blockNum, l2Height))
		}
	}
	c.lastL2Block = l2Height
	c.metrics.SetLatestBlocks(0, l2Height)

	// Determine L1 start block and backfill
	if c.l1 != nil && l1Height > 0 {
		var fromL1 uint64
		if c.startL1Block > 0 {
			fromL1 = c.startL1Block + 1
		} else if c.historyBlocks > 0 {
			// Estimate L1 range: L1 block time ~30s on RSK, L2 block time ~2s
			// So for N L2 blocks, we need roughly N * (L2blockTime/L1blockTime) L1 blocks
			estimatedL1Blocks := numL2Blocks * 2 / 30
			if estimatedL1Blocks < 50 {
				estimatedL1Blocks = 50
			}
			if l1Height > estimatedL1Blocks {
				fromL1 = l1Height - estimatedL1Blocks
			} else {
				fromL1 = 1
			}
		}

		if fromL1 > 0 && fromL1 <= l1Height {
			c.metrics.AppendLog("[backfill] replaying L1 #%d..#%d (%d blocks)", fromL1, l1Height, l1Height-fromL1+1)
			for blockNum := fromL1; blockNum <= l1Height; blockNum++ {
				select {
				case <-c.ctx.Done():
					return
				default:
				}
				c.processL1Block(blockNum)
			}
		}
		c.lastL1Block = l1Height
		c.metrics.SetLatestBlocks(l1Height, 0)
	}

	c.metrics.AppendLog("[backfill] done. Entering live polling.")
	c.fetchPendingAndBalances()
}

// BackfillL1 processes the last n L1 blocks to populate batcher metrics
// (posting frequency, data size, post cost) without replaying L2 blocks.
// This avoids contaminating cumulative L2 counters (total_l2_txs, etc.)
// that would break derived metrics like amortized_l1_cost_per_l2_tx.
// Must be called before Run().
func (c *Collector) BackfillL1(n int) {
	if c.l1 == nil || n <= 0 {
		return
	}

	height, err := c.l1.BlockNumber(c.ctx)
	if err != nil {
		c.metrics.AppendLog("[backfill-l1] failed to get L1 height: %v", err)
		return
	}
	if height == 0 {
		return
	}

	var from uint64
	if height > uint64(n) {
		from = height - uint64(n)
	} else {
		from = 1
	}

	c.metrics.AppendLog("[backfill-l1] replaying L1 #%d..#%d (%d blocks)", from, height, height-from+1)
	for blockNum := from; blockNum <= height; blockNum++ {
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		c.processL1Block(blockNum)
	}
	c.lastL1Block = height
	c.metrics.AppendLog("[backfill-l1] done — batcher metrics populated from %d L1 blocks", height-from+1)
}

// Close closes L1, L2, and op-node clients.
func (c *Collector) Close() {
	if c.l1 != nil {
		c.l1.Close()
	}
	if c.l2 != nil {
		c.l2.Close()
	}
	if c.nodeRPC != nil {
		c.nodeRPC.Close()
	}
}

// pollL2 fetches new L2 blocks and extracts metrics.
func (c *Collector) pollL2() {
	height, err := c.l2.BlockNumber(c.ctx)
	if err != nil {
		c.metrics.SetStatus(fmt.Sprintf("L2 error: %v", err))
		return
	}

	// On first poll, just set the baseline
	if c.lastL2Block == 0 {
		c.lastL2Block = height
		c.metrics.SetLatestBlocks(0, height)
		c.metrics.SetStatus(fmt.Sprintf("L2 synced at #%d. Polling...", height))
		return
	}

	// Process new blocks
	for blockNum := c.lastL2Block + 1; blockNum <= height; blockNum++ {
		c.processL2Block(blockNum)
	}

	c.lastL2Block = height
	c.metrics.SetLatestBlocks(0, height)

	// Fetch pending txs and balances (best-effort, don't block on errors)
	c.fetchPendingAndBalances()

	// Poll L2 txpool depth (pending + queued counts)
	c.pollTxPoolStatus()

	statusL1 := ""
	if c.lastL1Block > 0 {
		statusL1 = fmt.Sprintf("  L1: #%d", c.lastL1Block)
	}
	c.metrics.SetStatus(fmt.Sprintf("Polling L2: #%d%s", height, statusL1))
}

// processL2Block extracts metrics from a single L2 block.
func (c *Collector) processL2Block(blockNum uint64) {
	block, err := c.l2.BlockByNumber(c.ctx, new(big.Int).SetUint64(blockNum))
	if err != nil {
		return
	}

	blockTime := time.Unix(int64(block.Time()), 0)

	var userTxCount int
	var depositCount int
	var withdrawCount int
	var totalCost big.Int
	var costCount int
	var minCost, maxCost *big.Int

	for _, tx := range block.Transactions() {
		// Deposit txs (system txs from L1 -> L2)
		if tx.Type() == types.DepositTxType {
			depositCount++
			continue
		}

		// Skip internal funding transfers from the traffic simulator.
		if c.metrics.IsFundingTx(tx.Hash()) {
			continue
		}

		userTxCount += 1

		// Check for withdrawals (txs to L2ToL1MessagePasser)
		if tx.To() != nil && *tx.To() == l2ToL1MessagePasserAddr {
			withdrawCount += 1
		}

		// Get receipt for tx cost
		receipt, err := c.l2.TransactionReceipt(c.ctx, tx.Hash())
		if err != nil || receipt == nil {
			userTxCount -= 1 // should not include failed tx;
			continue
		}

		cost := txCostFromReceipt(tx, receipt)
		if cost != nil {
			totalCost.Add(&totalCost, cost)
			costCount++
			if minCost == nil || cost.Cmp(minCost) < 0 {
				minCost = new(big.Int).Set(cost)
			}
			if maxCost == nil || cost.Cmp(maxCost) > 0 {
				maxCost = new(big.Int).Set(cost)
			}
		}
	}

	// TPS: user txs / block time
	blockTimeSec := float64(c.blockTimeSeconds())
	if blockTimeSec <= 0 {
		blockTimeSec = float64(c.cfg.Rollup.BlockTime)
	}
	tps := float64(userTxCount) / blockTimeSec
	c.metrics.AddTPS(blockTime, tps)

	// Tx count
	c.metrics.AddL2TxCount(blockTime, float64(userTxCount))

	// Tx cost (average, min, max for this block). Only push when we have actual
	// cost data; pushing 0 for "no measurement" creates invisible chart lines
	// since real costs (~0.00000002 RBTC) get dwarfed by the 0-to-max y-axis range.
	if costCount > 0 {
		avgCost := new(big.Int).Div(&totalCost, big.NewInt(int64(costCount)))
		costRBTC := weiToFloat(avgCost)
		c.metrics.AddL2TxCost(blockTime, costRBTC)
		c.metrics.AddL2TxCostMin(blockTime, weiToFloat(minCost))
		c.metrics.AddL2TxCostMax(blockTime, weiToFloat(maxCost))
	}

	// L2 Tx Speed: actual latency measurements are pushed by the traffic
	// simulator when active. Don't push 0 from the collector — it would
	// flatten the chart when no traffic is running.

	// Deposits: subtract 1 for the L1 attributes system deposit that every
	// OP Stack L2 block includes as its first transaction.
	userDeposits := max(depositCount-1, 0)
	c.metrics.AddDeposits(blockTime, float64(userDeposits))

	// Withdrawals (always push, even 0, for continuous chart)
	c.metrics.AddWithdrawals(blockTime, float64(withdrawCount))

	// Write stats CSV row for analytics
	var avgCostRBTC float64
	if costCount > 0 {
		avgCost := new(big.Int).Div(&totalCost, big.NewInt(int64(costCount)))
		avgCostRBTC = weiToFloat(avgCost)
	}
	c.metrics.WriteStatsRow(blockTime, blockNum, tps, avgCostRBTC, 0, float64(userTxCount), userDeposits, withdrawCount)
}

// pollL1 fetches new L1 blocks and extracts batcher tx metrics.
func (c *Collector) pollL1() {
	if c.l1 == nil {
		return
	}

	height, err := c.l1.BlockNumber(c.ctx)
	if err != nil {
		return
	}

	// On first poll, set baseline
	if c.lastL1Block == 0 {
		c.lastL1Block = height
		c.metrics.SetLatestBlocks(height, 0)
		c.pollSyncStatus()
		return
	}

	// Process new blocks
	for blockNum := c.lastL1Block + 1; blockNum <= height; blockNum++ {
		c.processL1Block(blockNum)
	}

	c.lastL1Block = height
	c.metrics.SetLatestBlocks(height, 0)

	// Update batcher staleness
	c.updateBatcherStaleness()

	// Poll op-node sync status for safe head tracking
	c.pollSyncStatus()

	// Scrape batcher Prometheus for compression ratio (optional)
	c.pollBatcherMetrics()
}

// processL1Block extracts metrics from a single L1 block.
func (c *Collector) processL1Block(blockNum uint64) {
	block, err := c.l1.BlockByNumber(c.ctx, new(big.Int).SetUint64(blockNum))
	if err != nil {
		return
	}

	blockTime := time.Unix(int64(block.Time()), 0)
	txCount := len(block.Transactions())

	// Count only rollup-related txs (batcher posts, deposits, withdrawals,
	// bridge ops, dispute games). General L1 traffic on the shared L1 is not
	// useful for evaluating the rollup itself.
	rollupTxCount := 0
	for _, tx := range block.Transactions() {
		if c.isRollupTx(tx) {
			rollupTxCount++
		}
	}
	c.metrics.AddL1TxCount(blockTime, float64(rollupTxCount))

	// Detect batcher txs for posting frequency.
	// Match on tx.To() == batchInbox. The batch inbox is a unique address that
	// only the batcher sends to, so sender verification is not needed (and
	// sender recovery fails on RSK due to non-standard signatures).
	if c.batchInbox == (common.Address{}) {
		return
	}

	foundBatcher := false
	for _, tx := range block.Transactions() {
		to := tx.To()

		// Enhanced diagnostics: track all tx destinations in first 3 L1 blocks with txs
		if txCount > 0 && c.l1DiagBlocks < 3 {
			c.l1DiagTxsScanned++
			if to != nil {
				c.l1DiagDestCounts[*to]++
			}
		}

		if to == nil || *to != c.batchInbox {
			continue
		}

		foundBatcher = true

		// Batcher tx found -- record posting frequency
		if !c.lastBatcherTxTime.IsZero() {
			interval := blockTime.Sub(c.lastBatcherTxTime).Seconds()
			c.metrics.AddPostingFreq(blockTime, interval)
			c.metrics.AppendLog("[batcher] post detected L1 #%d, interval %.1fs", blockNum, interval)
		} else {
			c.metrics.AppendLog("[batcher] first post detected L1 #%d", blockNum)
		}
		c.lastBatcherTxTime = blockTime
		c.metrics.SetBatcherStaleness(0, blockTime)

		// Track batcher tx cost and calldata size
		dataSize := float64(len(tx.Data()))
		c.metrics.AddBatcherDataSize(blockTime, dataSize)

		receipt, err := c.l1.TransactionReceipt(c.ctx, tx.Hash())
		if err == nil && receipt != nil {
			cost := txCostFromReceipt(tx, receipt)
			if cost != nil {
				costRBTC := weiToFloat(cost)
				c.metrics.AddBatcherPostCost(blockTime, costRBTC)
				gp, gSpend := gasPriceAndSpendFromReceipt(tx, receipt)
				c.metrics.AddBatcherL1GasStats(blockTime, bigIntWeiToFloat64(gSpend), bigIntWeiToFloat64(gp))
				c.metrics.AppendLog("[batcher] L1 post cost: %.8f RBTC, data: %.0f bytes, gas: %.0f wei @ %.0f wei/gas",
					costRBTC, dataSize, bigIntWeiToFloat64(gSpend), bigIntWeiToFloat64(gp))
			}
		}

		// Parse frames from calldata and feed into channel assembler
		c.ingestBatcherTx(tx.Data(), blockNum, blockTime)
	}
	if foundBatcher {
		c.metrics.AddBatcherBlock()
	}

	// Prune stale pending channels (older than 5 minutes) to prevent memory leaks.
	c.pruneStaleChannels()

	// Enhanced diagnostics: after processing an L1 block with txs, log all unique destinations
	if txCount > 0 && c.l1DiagBlocks < 3 {
		c.l1DiagBlocks++
		if c.l1DiagBlocks == 3 {
			c.logDiagnosticSummary(blockNum)
		}
	}

	// Don't push 0 for posting freq when there's no batcher tx in this block.
	// Pushing 0 flattens the chart (real intervals like 30s become invisible
	// against the zero baseline). Only push actual intervals.
}

// logDiagnosticSummary logs a summary of L1 tx destinations after scanning the first few blocks.
func (c *Collector) logDiagnosticSummary(upToBlock uint64) {
	if c.l1DiagSummaryLogged {
		return
	}
	c.l1DiagSummaryLogged = true

	// Sort destinations by count (descending)
	type destCount struct {
		addr  common.Address
		count int
	}
	var dests []destCount
	for addr, count := range c.l1DiagDestCounts {
		dests = append(dests, destCount{addr, count})
	}
	sort.Slice(dests, func(i, j int) bool { return dests[i].count > dests[j].count })

	matched := 0
	for _, d := range dests {
		if d.addr == c.batchInbox {
			matched = d.count
		}
	}

	c.metrics.AppendLog("[diag] scanned %d L1 txs in %d blocks up to #%d, %d matched batch inbox %s",
		c.l1DiagTxsScanned, c.l1DiagBlocks, upToBlock, matched, c.batchInbox.Hex()[:10]+"...")

	// Show top 5 destinations
	limit := 5
	if len(dests) < limit {
		limit = len(dests)
	}
	for i := 0; i < limit; i++ {
		c.metrics.AppendLog("[diag]   %s (%d txs)", dests[i].addr.Hex(), dests[i].count)
	}

	// Free the map — no longer needed
	c.l1DiagDestCounts = nil
}

// updateBatcherStaleness computes how long since the last batcher post and updates metrics.
func (c *Collector) updateBatcherStaleness() {
	if c.lastBatcherTxTime.IsZero() {
		// Never seen a batcher post
		c.metrics.SetBatcherStaleness(-1, time.Time{})

		// Warn after processing 10+ L1 blocks with no match
		if c.l1DiagBlocks >= 3 && !c.batcherWarningLogged {
			c.batcherWarningLogged = true
			c.metrics.AppendLog("[batcher] WARNING: no batcher posts detected after scanning %d L1 blocks. Verify batcher is running and batch inbox matches.", c.l1DiagBlocks)
		}
		return
	}

	staleness := time.Since(c.lastBatcherTxTime).Seconds()
	c.metrics.SetBatcherStaleness(staleness, c.lastBatcherTxTime)
}

// SyncStatusResult holds the relevant fields from optimism_syncStatus response.
// Exported so other tools (e.g. rollup-dashboard) can reuse it via the monitor
// package instead of re-declaring the same shape.
type SyncStatusResult struct {
	UnsafeL2 struct {
		Number uint64 `json:"number"`
	} `json:"unsafe_l2"`
	SafeL2 struct {
		Number uint64 `json:"number"`
	} `json:"safe_l2"`
	FinalizedL2 struct {
		Number uint64 `json:"number"`
	} `json:"finalized_l2"`
}

// pollSyncStatus queries the op-node for sync status and updates safe head lag.
func (c *Collector) pollSyncStatus() {
	if c.nodeRPC == nil {
		return
	}

	var raw json.RawMessage
	err := c.nodeRPC.CallContext(c.ctx, &raw, "optimism_syncStatus")
	if err != nil {
		c.metrics.AppendLog("[sync] optimism_syncStatus error: %v", err)
		return
	}

	var status SyncStatusResult
	if err := json.Unmarshal(raw, &status); err != nil {
		c.metrics.AppendLog("[sync] failed to parse syncStatus: %v", err)
		return
	}

	unsafeL2 := status.UnsafeL2.Number
	safeL2 := status.SafeL2.Number

	// Log when safe head advances
	prevSafe := c.metrics.GetSafeL2()
	c.metrics.SetSyncStatus(unsafeL2, safeL2)
	if safeL2 > prevSafe && prevSafe > 0 {
		c.metrics.AppendLog("[sync] safe head advanced: #%d -> #%d (+%d blocks)", prevSafe, safeL2, safeL2-prevSafe)
	}

	var lag float64
	if unsafeL2 > safeL2 {
		lag = float64(unsafeL2 - safeL2)
	}
	c.metrics.AddSafeHeadLag(time.Now(), lag)
}

// fetchPendingAndBalances queries L1 and L2 for pending tx counts and balances.
func (c *Collector) fetchPendingAndBalances() {
	if c.account == (common.Address{}) {
		return
	}

	var l1Pending, l2Pending uint64
	var l1Bal, l2Bal float64

	// L2 pending: difference between pending nonce and confirmed nonce
	if pendingNonce, err := c.l2.PendingNonceAt(c.ctx, c.account); err == nil {
		if confirmedNonce, err := c.l2.NonceAt(c.ctx, c.account, nil); err == nil {
			if pendingNonce > confirmedNonce {
				l2Pending = pendingNonce - confirmedNonce
			}
		}
	}

	// L2 balance
	if bal, err := c.l2.BalanceAt(c.ctx, c.account, nil); err == nil {
		l2Bal = weiToFloat(bal)
	}

	// L1 pending and balance
	if c.l1 != nil {
		if pendingNonce, err := c.l1.PendingNonceAt(c.ctx, c.account); err == nil {
			if confirmedNonce, err := c.l1.NonceAt(c.ctx, c.account, nil); err == nil {
				if pendingNonce > confirmedNonce {
					l1Pending = pendingNonce - confirmedNonce
				}
			}
		}
		if bal, err := c.l1.BalanceAt(c.ctx, c.account, nil); err == nil {
			l1Bal = weiToFloat(bal)
		}
	}

	c.metrics.SetPendingAndBalances(l1Pending, l2Pending, l1Bal, l2Bal)

	// Query USDRIF ERC20 balances if token addresses are configured.
	// balanceOf(address) = 0x70a08231 + padded address
	if c.cfg.USDRIFTokenL1 != "" || c.cfg.USDRIFTokenL2 != "" {
		var l1USDRIFBal, l2USDRIFBal float64
		balOfSelector := crypto.Keccak256([]byte("balanceOf(address)"))[:4]
		paddedAddr := common.LeftPadBytes(c.account.Bytes(), 32)
		callData := append(balOfSelector, paddedAddr...)

		if c.cfg.USDRIFTokenL1 != "" && c.l1 != nil {
			l1Token := common.HexToAddress(c.cfg.USDRIFTokenL1)
			result, err := c.l1.CallContract(c.ctx, ethereum.CallMsg{To: &l1Token, Data: callData}, nil)
			if err == nil && len(result) >= 32 {
				bal := new(big.Int).SetBytes(result[:32])
				l1USDRIFBal = weiToFloat(bal)
			}
		}

		if c.cfg.USDRIFTokenL2 != "" {
			l2Token := common.HexToAddress(c.cfg.USDRIFTokenL2)
			result, err := c.l2.CallContract(c.ctx, ethereum.CallMsg{To: &l2Token, Data: callData}, nil)
			if err == nil && len(result) >= 32 {
				bal := new(big.Int).SetBytes(result[:32])
				l2USDRIFBal = weiToFloat(bal)
			}
		}

		c.metrics.SetUSDRIFBalances(l1USDRIFBal, l2USDRIFBal)
	}
}

// txPoolStatusResult holds the parsed response from txpool_status RPC.
type txPoolStatusResult struct {
	Pending uint64
	Queued  uint64
}

// UnmarshalJSON handles the hex-encoded integers returned by txpool_status.
func (t *txPoolStatusResult) UnmarshalJSON(data []byte) error {
	var raw struct {
		Pending string `json:"pending"`
		Queued  string `json:"queued"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Pending != "" {
		v, err := strconv.ParseUint(strings.TrimPrefix(raw.Pending, "0x"), 16, 64)
		if err != nil {
			return fmt.Errorf("parse pending %q: %w", raw.Pending, err)
		}
		t.Pending = v
	}
	if raw.Queued != "" {
		v, err := strconv.ParseUint(strings.TrimPrefix(raw.Queued, "0x"), 16, 64)
		if err != nil {
			return fmt.Errorf("parse queued %q: %w", raw.Queued, err)
		}
		t.Queued = v
	}
	return nil
}

// pollTxPoolStatus queries the L2 txpool_status RPC for pending and queued counts.
func (c *Collector) pollTxPoolStatus() {
	var status txPoolStatusResult
	err := c.l2.Client().CallContext(c.ctx, &status, "txpool_status")
	if err != nil {
		return
	}
	c.metrics.AddTxPoolStatus(time.Now(), float64(status.Pending), float64(status.Queued))
}

// pollBatcherMetrics scrapes the op-batcher Prometheus endpoint for compression
// ratio and additional pipeline health metrics.
func (c *Collector) pollBatcherMetrics() {
	if c.cfg.BatcherMetricsURL == "" {
		return
	}

	result, err := ScrapePrometheus(c.ctx, c.cfg.BatcherMetricsURL)
	if err != nil {
		c.metrics.SetBatcherUp(false)
		return
	}

	now := time.Now()

	// Process liveness
	if up, ok := result.Get("_up"); ok {
		c.metrics.SetBatcherUp(up == 1)
	} else {
		c.metrics.SetBatcherUp(true) // reachable = up
	}

	// Compression ratio (backward-compatible with original behavior)
	comprSum, hasSum := result.Get("channel_compr_ratio_sum")
	comprCount, hasCount := result.Get("channel_compr_ratio_count")
	if hasSum && hasCount && comprCount > 0 {
		ratio := comprSum / comprCount
		c.metrics.AddCompressionRatio(now, ratio)
		c.metrics.AddCompressionRatioDelta(now, comprSum, comprCount)
	}

	// Pipeline metrics
	pendingBlocks, _ := result.BatcherPendingBlocksCount()
	pendingBytes, _ := result.Get("pending_blocks_bytes_current")
	channelTimeouts, _ := result.GetLabeled("channel_total", "stage", "timed_out")
	txFailed, _ := result.GetLabeled("batcher_tx_total", "stage", "failed")
	gasBumps, _ := result.Get("txmgr_tx_gas_bump")
	baseFee, _ := result.Get("txmgr_basefee_wei")
	balance, _ := result.Get("_balance")

	c.metrics.RecordBatcherPipelineMetrics(now, pendingBlocks, pendingBytes, channelTimeouts, txFailed, gasBumps, baseFee, balance)
}

// pollNodeMetrics scrapes the op-node Prometheus endpoint for derivation and
// sequencer health metrics.
func (c *Collector) pollNodeMetrics() {
	if c.cfg.NodeMetricsURL == "" {
		return
	}

	result, err := ScrapePrometheus(c.ctx, c.cfg.NodeMetricsURL)
	if err != nil {
		c.metrics.SetNodeUp(false)
		return
	}

	now := time.Now()

	if up, ok := result.Get("_up"); ok {
		c.metrics.SetNodeUp(up == 1)
	} else {
		c.metrics.SetNodeUp(true)
	}

	if idle, ok := result.Get("derivation_idle"); ok {
		c.metrics.SetDerivationIdle(idle == 1)
	}

	derivErrs, _ := result.Get("derivation_errors_total")
	pipelineResets, _ := result.Get("pipeline_resets_total")
	seqErrs, _ := result.Get("sequencing_errors_total")
	l1Latency, _ := result.GetLabeled("refs_latency", "layer", "l1", "type", "head")
	unsafePayloads, _ := result.Get("unsafe_payloads_buffer_len")

	// l1Latency is negative (seconds behind), take absolute value for display
	if l1Latency < 0 {
		l1Latency = -l1Latency
	}

	c.metrics.RecordNodeMetrics(now, derivErrs, pipelineResets, seqErrs, l1Latency, unsafePayloads)
}

// pollProposerMetrics scrapes the op-proposer Prometheus endpoint.
func (c *Collector) pollProposerMetrics() {
	if c.cfg.ProposerMetricsURL == "" {
		return
	}

	result, err := ScrapePrometheus(c.ctx, c.cfg.ProposerMetricsURL)
	if err != nil {
		c.metrics.SetProposerUp(false)
		return
	}

	now := time.Now()

	if up, ok := result.Get("_up"); ok {
		c.metrics.SetProposerUp(up == 1)
	} else {
		c.metrics.SetProposerUp(true)
	}

	seqNum, _ := result.Get("proposed_sequence_number")
	balance, _ := result.Get("_balance")

	c.metrics.RecordProposerMetrics(now, seqNum, balance)
}

// pollPprof fetches heap and goroutine counts from pprof endpoints.
// Called on a slow cadence (every 30s) to avoid overhead.
func (c *Collector) pollPprof() {
	now := time.Now()
	var nodeHeapMB, nodeGoroutines, batcherHeapMB, batcherGoroutines float64 = -1, -1, -1, -1

	if c.cfg.NodePprofURL != "" {
		if heap, err := ScrapePprofHeapInuse(c.ctx, c.cfg.NodePprofURL); err == nil {
			nodeHeapMB = float64(heap) / (1024 * 1024)
		}
		if gr, err := ScrapePprofGoroutines(c.ctx, c.cfg.NodePprofURL); err == nil {
			nodeGoroutines = float64(gr)
		}
	}

	if c.cfg.BatcherPprofURL != "" {
		if heap, err := ScrapePprofHeapInuse(c.ctx, c.cfg.BatcherPprofURL); err == nil {
			batcherHeapMB = float64(heap) / (1024 * 1024)
		}
		if gr, err := ScrapePprofGoroutines(c.ctx, c.cfg.BatcherPprofURL); err == nil {
			batcherGoroutines = float64(gr)
		}
	}

	c.metrics.RecordPprofMetrics(now, nodeHeapMB, nodeGoroutines, batcherHeapMB, batcherGoroutines)
}

// queryTheoreticalMaxTPS queries the L2 block gas limit and computes theoretical max TPS.
// Called once on startup after connecting to L2.
func (c *Collector) queryTheoreticalMaxTPS() {
	block, err := c.l2.BlockByNumber(c.ctx, nil) // latest block
	if err != nil {
		return
	}
	gasLimit := block.GasLimit()
	blockTime := float64(c.blockTimeSeconds())
	if blockTime <= 0 {
		blockTime = 2.0
	}
	// Simple transfer = 21000 gas
	maxTPS := float64(gasLimit) / 21000.0 / blockTime
	c.metrics.SetTheoreticalMaxTPS(maxTPS)
	c.metrics.AppendLog("[collector] theoretical max TPS: %.1f (gasLimit=%d, blockTime=%.0fs)", maxTPS, gasLimit, blockTime)
}

// blockTimeSeconds returns the L2 block time from config, or 2 as default.
func (c *Collector) blockTimeSeconds() uint64 {
	if c.cfg.Rollup != nil && c.cfg.Rollup.BlockTime > 0 {
		return c.cfg.Rollup.BlockTime
	}
	return 2
}

// txCostFromReceipt computes the total tx cost from receipt (gas + L1 fee).
func txCostFromReceipt(tx *types.Transaction, receipt *types.Receipt) *big.Int {
	gasPrice := tx.GasPrice()
	if gasPrice == nil {
		gasPrice = big.NewInt(0)
	}
	if receipt.EffectiveGasPrice != nil && receipt.EffectiveGasPrice.Sign() > 0 {
		gasPrice = receipt.EffectiveGasPrice
	}
	cost := new(big.Int).Mul(gasPrice, big.NewInt(int64(receipt.GasUsed)))
	if receipt.L1Fee != nil && receipt.L1Fee.Sign() > 0 {
		cost.Add(cost, receipt.L1Fee)
	}
	return cost
}

// gasPriceAndSpendFromReceipt returns effective gas price and execution gas cost
// (gasUsed * effectiveGasPrice) in wei, excluding OP-stack L1 data fee.
func gasPriceAndSpendFromReceipt(tx *types.Transaction, receipt *types.Receipt) (gasPrice *big.Int, gasSpend *big.Int) {
	gasPrice = tx.GasPrice()
	if gasPrice == nil {
		gasPrice = big.NewInt(0)
	}
	if receipt != nil && receipt.EffectiveGasPrice != nil && receipt.EffectiveGasPrice.Sign() > 0 {
		gasPrice = new(big.Int).Set(receipt.EffectiveGasPrice)
	}
	gasSpend = new(big.Int).Mul(gasPrice, new(big.Int).SetUint64(receipt.GasUsed))
	return gasPrice, gasSpend
}

func bigIntWeiToFloat64(w *big.Int) float64 {
	if w == nil || w.Sign() == 0 {
		return 0
	}
	f, _ := new(big.Float).SetInt(w).Float64()
	return f
}

// ingestBatcherTx parses frames from a batcher calldata transaction and feeds
// them into the channel assembler. When a channel becomes complete, it decodes
// the batches to count L2 blocks and records the result.
func (c *Collector) ingestBatcherTx(data []byte, l1BlockNum uint64, blockTime time.Time) {
	if len(data) == 0 {
		return
	}

	frames, err := derive.ParseFrames(data)
	if err != nil {
		c.metrics.AppendLog("[batch] WARN: ParseFrames failed at L1 #%d (%d bytes): %v", l1BlockNum, len(data), err)
		return
	}
	c.metrics.AppendLog("[batch] L1 #%d parsed %d frame(s) from %d-byte tx", l1BlockNum, len(frames), len(data))

	l1Ref := eth.L1BlockRef{Number: l1BlockNum}

	for _, frame := range frames {
		pc, ok := c.pendingChannels[frame.ID]
		if !ok {
			ch := derive.NewChannel(frame.ID, l1Ref, false)
			pc = &pendingChannel{
				channel:  ch,
				firstL1:  l1BlockNum,
				openedAt: blockTime,
			}
			c.pendingChannels[frame.ID] = pc
		}

		if err := pc.channel.AddFrame(frame, l1Ref); err != nil {
			c.metrics.AppendLog("[batch] WARN: AddFrame failed for channel %x at L1 #%d: %v", frame.ID[:4], l1BlockNum, err)
			continue
		}
		pc.l1Txs++
		pc.lastL1 = l1BlockNum

		if !pc.channel.IsReady() {
			c.metrics.AppendLog("[batch] channel %x frame #%d added (is_last=%v) at L1 #%d, channel still open", frame.ID[:4], frame.FrameNumber, frame.IsLast, l1BlockNum)
			continue
		}
		c.metrics.AppendLog("[batch] channel %x READY after frame #%d at L1 #%d", frame.ID[:4], frame.FrameNumber, l1BlockNum)

		l2Blocks, l2Txs, earliestTS, latestTS, earliestSendTime := c.countBatchesAndTxsInChannel(pc.channel)
		openL1Blocks := pc.lastL1 - pc.firstL1
		c.metrics.AddChannelOpenL1Blocks(blockTime, float64(openL1Blocks))

		if l2Blocks > 0 {
			c.metrics.AddL2BlocksPerBatch(blockTime, float64(l2Blocks), float64(l2Txs), float64(pc.l1Txs))

			// Calculate L2-to-L1 throughput using the most accurate timing available:
			// - If we have tracked tx send times, use earliestSendTime (includes L2 mempool wait)
			// - Otherwise fall back to earliestTS (L2 block timestamp)
			if l2Txs > 0 {
				var latencySeconds float64
				if !earliestSendTime.IsZero() {
					// Accurate: from original tx send to L1 confirmation
					latencySeconds = blockTime.Sub(earliestSendTime).Seconds()
				} else if earliestTS > 0 {
					// Fallback: from L2 block inclusion to L1 confirmation
					l1ConfirmTime := uint64(blockTime.Unix())
					if l1ConfirmTime > earliestTS {
						latencySeconds = float64(l1ConfirmTime - earliestTS)
					}
				}
				if latencySeconds > 0 {
					throughput := float64(l2Txs) / latencySeconds
					c.metrics.AddL2ToL1Throughput(blockTime, throughput, latencySeconds, float64(l2Txs))
				}
			}

			logMsg := fmt.Sprintf("[batch] channel complete: %d L2 blocks, %d L2 txs, %d L1 txs, open %d L1 blocks (L1 #%d..#%d, L2 ts %d..%d)",
				l2Blocks, l2Txs, pc.l1Txs, openL1Blocks, pc.firstL1, pc.lastL1, earliestTS, latestTS)
			if !earliestSendTime.IsZero() {
				logMsg += fmt.Sprintf(" [tracked: latency=%.1fs]", blockTime.Sub(earliestSendTime).Seconds())
			}
			c.metrics.AppendLog("%s", logMsg)
		}
		delete(c.pendingChannels, frame.ID)
	}
}

// countBatchesAndTxsInChannel decompresses and counts the number of batches (L2 blocks)
// and transactions in a completed channel. Also returns the earliest and latest L2 block
// timestamps from the batches, and the earliest tracked tx send time for accurate latency.
// Returns zeros on any decoding error.
func (c *Collector) countBatchesAndTxsInChannel(ch *derive.Channel) (blocks, txs int, earliestTS, latestTS uint64, earliestSendTime time.Time) {
	reader := ch.Reader()
	// Fjord enables brotli-compressed batches. We assume Fjord-or-later for any
	// chain whose rollup.json schedules Fjord, since the monitor only inspects
	// channels posted after we connected (i.e. always at or beyond head).
	isFjord := c.cfg.Rollup != nil && c.cfg.Rollup.IsForkActive(c.cfg.Rollup.FjordTime)
	batchFn, err := derive.BatchReader(reader, maxRLPBytesPerChannelBedrock, isFjord)
	if err != nil {
		c.metrics.AppendLog("[batch] WARN: BatchReader init failed (isFjord=%v): %v", isFjord, err)
		return 0, 0, 0, 0, time.Time{}
	}

	var trackedCount int
	for {
		batchData, err := batchFn()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		switch batchData.GetBatchType() {
		case derive.SingularBatchType:
			blocks++
			if sb, err := derive.GetSingularBatch(batchData); err == nil {
				txs += len(sb.Transactions)
				ts := sb.GetTimestamp()
				if earliestTS == 0 || ts < earliestTS {
					earliestTS = ts
				}
				if ts > latestTS {
					latestTS = ts
				}

				// Decode tx hashes and look up send times for accurate latency
				for _, txBytes := range sb.Transactions {
					var tx types.Transaction
					if err := tx.UnmarshalBinary(txBytes); err != nil {
						continue
					}
					txHash := tx.Hash()
					if sendTime, ok := c.metrics.GetTxSendTime(txHash); ok {
						trackedCount++
						if earliestSendTime.IsZero() || sendTime.Before(earliestSendTime) {
							earliestSendTime = sendTime
						}
						c.metrics.RemoveTrackedTx(txHash)
					}
				}
			}
		case derive.SpanBatchType:
			blockTime, genesisTime, chainID := c.spanBatchParams()
			if chainID == nil {
				c.metrics.AppendLog("[batch] WARN: SpanBatch seen but rollup config is incomplete (blockTime=%d genesisTime=%d) — skipping", blockTime, genesisTime)
				continue
			}
			sp, err := derive.DeriveSpanBatch(batchData, blockTime, genesisTime, chainID)
			if err != nil {
				c.metrics.AppendLog("[batch] WARN: DeriveSpanBatch failed (blockTime=%d genesisTime=%d chainID=%s): %v", blockTime, genesisTime, chainID, err)
				continue
			}
			n := sp.GetBlockCount()
			blocks += n
			for i := 0; i < n; i++ {
				ts := sp.GetBlockTimestamp(i)
				if earliestTS == 0 || ts < earliestTS {
					earliestTS = ts
				}
				if ts > latestTS {
					latestTS = ts
				}
				blockTxs := sp.GetBlockTransactions(i)
				txs += len(blockTxs)
				for _, txBytes := range blockTxs {
					var tx types.Transaction
					if err := tx.UnmarshalBinary(txBytes); err != nil {
						continue
					}
					txHash := tx.Hash()
					if sendTime, ok := c.metrics.GetTxSendTime(txHash); ok {
						trackedCount++
						if earliestSendTime.IsZero() || sendTime.Before(earliestSendTime) {
							earliestSendTime = sendTime
						}
						c.metrics.RemoveTrackedTx(txHash)
					}
				}
			}
		}
	}
	return blocks, txs, earliestTS, latestTS, earliestSendTime
}

// isRollupTx returns true if the L1 tx interacts with this rollup: batcher
// posts to the batch inbox, deposits to OptimismPortal, bridge ops to
// L1StandardBridge, or dispute-game txs to DisputeGameFactory.
//
// Note: this misses txs sent to dispute game *instances* (CREATE2'd by the
// factory) — capturing those would require tracking factory-emitted events.
func (c *Collector) isRollupTx(tx *types.Transaction) bool {
	to := tx.To()
	if to == nil {
		return false
	}
	switch *to {
	case c.batchInbox, c.depositPortal, c.l1StdBridge, c.disputeFactor:
		return *to != (common.Address{})
	}
	return false
}

// spanBatchParams returns (blockTime, genesisL2Time, chainID) needed to derive
// SpanBatches. Returns chainID == nil when the rollup config is unavailable or
// incomplete, in which case SpanBatch decoding must be skipped.
func (c *Collector) spanBatchParams() (uint64, uint64, *big.Int) {
	if c.cfg.Rollup == nil || c.cfg.Rollup.L2ChainID == 0 || c.cfg.Rollup.BlockTime == 0 {
		return 0, 0, nil
	}
	return c.cfg.Rollup.BlockTime, c.cfg.Rollup.Genesis.L2Time, new(big.Int).SetUint64(c.cfg.Rollup.L2ChainID)
}

// pruneStaleChannels removes pending channels that have been open longer than
// the channel's lifetime could plausibly be on this chain. Channels can legitimately
// stay open up to max_channel_duration L1 blocks (configurable on the batcher);
// the prune window must exceed that or live channels get dropped before they seal.
// We don't have the batcher's max_channel_duration here, so we use a generous
// 4-hour ceiling — large enough for slow chains (RSK ~30s blocks with
// max_channel_duration=150 → ~75min) plus the channel_timeout window.
func (c *Collector) pruneStaleChannels() {
	cutoff := time.Now().Add(-4 * time.Hour)
	for id, pc := range c.pendingChannels {
		if pc.openedAt.Before(cutoff) {
			c.metrics.AppendLog("[batch] WARN: pruning stale channel %x (open %s) — never received final frame", id[:4], time.Since(pc.openedAt).Round(time.Second))
			delete(c.pendingChannels, id)
		}
	}
}

// weiToFloat converts wei to RBTC as a float64.
func weiToFloat(wei *big.Int) float64 {
	if wei == nil {
		return 0
	}
	f := new(big.Float).SetInt(wei)
	f.Quo(f, big.NewFloat(weiPerRBTC))
	result, _ := f.Float64()
	return result
}
