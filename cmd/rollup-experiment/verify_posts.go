package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"text/tabwriter"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/fedejinich/rootstack-operator-tools/cmd/rollup-monitor/monitor"
	"github.com/urfave/cli/v2"
)

func runVerifyPosts(cliCtx *cli.Context) error {
	logLevel := slog.LevelInfo
	switch cliCtx.String("log-level") {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))

	dir := cliCtx.String("dir")
	workdir := cliCtx.String("workdir")
	l1RPC := cliCtx.String("l1-rpc")
	filterRun := cliCtx.Int("run")

	rollupCfg, err := monitor.LoadRollupConfig(workdir)
	if err != nil {
		return fmt.Errorf("load rollup.json from %s: %w", workdir, err)
	}
	batchInbox := rollupCfg.BatchInbox()
	if batchInbox == (common.Address{}) {
		return fmt.Errorf("batch_inbox_address is zero in rollup.json")
	}
	logger.Info("Loaded rollup config", "batch_inbox", batchInbox.Hex())

	results, err := CollectRunResults(dir)
	if err != nil {
		return fmt.Errorf("collect run results from %s: %w", dir, err)
	}
	if len(results) == 0 {
		return fmt.Errorf("no experiment_run_*.csv files found in %s", dir)
	}

	client, err := ethclient.Dial(l1RPC)
	if err != nil {
		return fmt.Errorf("dial L1 RPC: %w", err)
	}
	defer client.Close()

	ctx := context.Background()

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "Run\tL1 Range\tCSV Posts\tOn-Chain Confirmed\tOn-Chain Failed\tMatch")

	for _, r := range results {
		if filterRun > 0 && r.RunID != filterRun {
			continue
		}

		firstL1, lastL1, csvPosts, err := readRunL1Range(r.CSVPath)
		if err != nil {
			logger.Error("Failed to read CSV", "run", r.RunID, "err", err)
			continue
		}

		logger.Info("Scanning L1 blocks", "run", r.RunID, "from", firstL1, "to", lastL1, "csv_posts", csvPosts)

		confirmed, failed, err := countBatcherPosts(ctx, client, batchInbox, firstL1, lastL1, logger)
		if err != nil {
			logger.Error("L1 scan failed", "run", r.RunID, "err", err)
			continue
		}

		match := "OK"
		if confirmed != csvPosts {
			match = "MISMATCH"
		}

		fmt.Fprintf(tw, "%d\t%d..%d\t%d\t%d\t%d\t%s\n",
			r.RunID, firstL1, lastL1, csvPosts, confirmed, failed, match)
	}

	tw.Flush()
	return nil
}

// readRunL1Range parses a run CSV and returns the first L1 block, last L1 block,
// and the final total_batcher_posts value.
func readRunL1Range(csvPath string) (firstL1, lastL1, csvPosts uint64, err error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read CSV: %w", err)
	}
	if len(records) < 2 {
		return 0, 0, 0, fmt.Errorf("CSV has no data rows")
	}

	header := records[0]
	data := records[1:]

	var first, last ExperimentSample
	if err := first.FromCSVRow(header, data[0]); err != nil {
		return 0, 0, 0, fmt.Errorf("parse first row: %w", err)
	}
	if err := last.FromCSVRow(header, data[len(data)-1]); err != nil {
		return 0, 0, 0, fmt.Errorf("parse last row: %w", err)
	}

	return first.Measurements.L1Block, last.Measurements.L1Block, last.Measurements.TotalBatcherPosts, nil
}

// countBatcherPosts scans L1 blocks [fromBlock, toBlock] and counts transactions
// sent to the batch inbox address, distinguishing confirmed vs failed receipts.
func countBatcherPosts(ctx context.Context, client *ethclient.Client, batchInbox common.Address, fromBlock, toBlock uint64, logger *slog.Logger) (confirmed, failed uint64, err error) {
	for blockNum := fromBlock; blockNum <= toBlock; blockNum++ {
		block, err := client.BlockByNumber(ctx, new(big.Int).SetUint64(blockNum))
		if err != nil {
			return confirmed, failed, fmt.Errorf("get block %d: %w", blockNum, err)
		}

		for _, tx := range block.Transactions() {
			to := tx.To()
			if to == nil || *to != batchInbox {
				continue
			}

			receipt, err := client.TransactionReceipt(ctx, tx.Hash())
			if err != nil {
				logger.Warn("Failed to get receipt", "tx", tx.Hash().Hex(), "block", blockNum, "err", err)
				failed++
				continue
			}

			if receipt.Status == types.ReceiptStatusSuccessful {
				confirmed++
				logger.Debug("Confirmed batcher post", "tx", tx.Hash().Hex(), "block", blockNum)
			} else {
				failed++
				logger.Debug("Failed batcher post", "tx", tx.Hash().Hex(), "block", blockNum)
			}
		}
	}
	return confirmed, failed, nil
}
