package monitor

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ethereum/go-ethereum/common"
	"github.com/fedejinich/rootstack-operator-tools/rsk/opdeployer/redact"
)

// --- TOML config structs ---

// GeneralConfig is the [general] section of the TOML config file.
type GeneralConfig struct {
	L1RPC      string `toml:"l1_rpc"`
	L2RPC      string `toml:"l2_rpc"`
	NodeRPC    string `toml:"node_rpc"`
	WorkDir    string `toml:"workdir"`
	PrivateKey string `toml:"private_key"`
}

// MonitorConfig is the [monitor] section of the TOML config file.
type MonitorConfig struct {
	PollInterval       string   `toml:"poll_interval"`
	HistorySize        int      `toml:"history_size"`
	HistoryBlocks      int      `toml:"history_blocks"`
	TrafficRate        float64  `toml:"traffic_rate"`
	TrafficWorkers     int      `toml:"traffic_workers"`
	TrafficAccounts    int      `toml:"traffic_accounts"`
	TPSWindowDuration  string   `toml:"tps_window_duration"` // Duration for rolling TPS window (e.g. "30s")
	BridgeAmountRBTC   string   `toml:"bridge_amount_rbtc"`
	USDRIFTokenL1      string   `toml:"usdrif_token_l1"`
	USDRIFTokenL2      string   `toml:"usdrif_token_l2"`
	BridgeAmountUSDRIF string   `toml:"bridge_amount_usdrif"`
	BatcherMetricsURL  string   `toml:"batcher_metrics_url"`
	ScanKeys           []string `toml:"scan_keys"` // Private keys for derived account scanning

	// Prometheus metrics endpoints for op-node and op-proposer
	NodeMetricsURL     string `toml:"node_metrics_url"`
	ProposerMetricsURL string `toml:"proposer_metrics_url"`

	// pprof endpoints for runtime profiling (heap, goroutines)
	NodePprofURL     string `toml:"node_pprof_url"`
	BatcherPprofURL  string `toml:"batcher_pprof_url"`
	ProposerPprofURL string `toml:"proposer_pprof_url"`
}

// ConfigFile is the full TOML file structure.
type ConfigFile struct {
	General GeneralConfig `toml:"general"`
	Monitor MonitorConfig `toml:"monitor"`
}

// Config is the resolved runtime config after merging TOML + CLI flags.
type Config struct {
	ConfigPath string // Path to the TOML config file (for saving prompted values back)

	L1RPC      string
	L2RPC      string
	NodeRPC    string // op-node RPC for sync status (optimism_syncStatus)
	WorkDir    string
	PrivateKey string

	PollInterval       time.Duration
	HistorySize        int
	HistoryBlocks      int    // Number of L2 blocks to replay on startup (0 = disabled)
	LoadStatsPath      string // Path to CSV stats file to load on startup (empty = auto-detect)
	TrafficRate        float64
	TrafficWorkers     int  // Number of concurrent traffic sender goroutines (default 64)
	TrafficAccounts    int  // Number of funded L2 sender accounts (default 1 = master only)
	OnlyMonologues     bool // When false, simple txs send to random other accounts instead of self
	BridgeAmountRBTC   string
	USDRIFTokenL1      string // L1 USDRIF token address (hex)
	USDRIFTokenL2      string // L2 USDRIF token address (hex)
	BridgeAmountUSDRIF string // Amount of USDRIF for bridge operations (e.g. "0.001")
	BatcherMetricsURL  string // Optional batcher Prometheus metrics URL for compression ratio
	NodeMetricsURL     string // Optional op-node Prometheus metrics URL (e.g. http://127.0.0.1:7301/metrics)
	ProposerMetricsURL string // Optional op-proposer Prometheus metrics URL (e.g. http://127.0.0.1:7302/metrics)
	NodePprofURL       string // Optional op-node pprof base URL (e.g. http://127.0.0.1:6061)
	BatcherPprofURL    string // Optional op-batcher pprof base URL (e.g. http://127.0.0.1:6063)
	ProposerPprofURL   string // Optional op-proposer pprof base URL (e.g. http://127.0.0.1:6062)
	ERC20TrafficToken  string // L2 ERC20 contract address for transfer traffic (hex)
	ERC20TrafficRate   float64
	ERC20TrafficValue  float64       // ERC20 transfer amount (in token units, e.g. 100)
	TxCalldataSize     int           // Bytes of random calldata to attach to simple txs (0 = none)
	ScanKeys           []string      // Private keys for derived account scanning
	TPSWindowDuration  time.Duration // Duration for rolling TPS window (default 30s)

	Rollup       *RollupConfig
	L1Addrs      L1Contracts
	DeployConfig *DeployConfig
}

// DeployConfig holds fault-proof timing parameters from deploy-config.json.
type DeployConfig struct {
	FaultGameMaxClockDuration       int `json:"faultGameMaxClockDuration"`
	FaultGameClockExtension         int `json:"faultGameClockExtension"`
	FaultGameWithdrawalDelay        int `json:"faultGameWithdrawalDelay"`
	PreimageOracleChallengePeriod   int `json:"preimageOracleChallengePeriod"`
	ProofMaturityDelaySeconds       int `json:"proofMaturityDelaySeconds"`
	DisputeGameFinalityDelaySeconds int `json:"disputeGameFinalityDelaySeconds"`
}

// ResolveClaimTimeout returns how long to wait for the game clock to expire
// before giving up on resolveClaim. Defaults to 15 minutes if not configured.
func (d *DeployConfig) ResolveClaimTimeout() time.Duration {
	if d != nil && d.FaultGameMaxClockDuration > 0 {
		// 2x the max clock duration as safety margin
		return time.Duration(d.FaultGameMaxClockDuration*2) * time.Second
	}
	return 15 * time.Minute
}

// FinalizeTimeout returns how long to wait for checkWithdrawal to pass after
// game resolution. Defaults to 15 minutes if not configured.
func (d *DeployConfig) FinalizeTimeout() time.Duration {
	if d != nil {
		// Sum of all delays plus a safety margin
		total := d.FaultGameWithdrawalDelay + d.DisputeGameFinalityDelaySeconds + d.ProofMaturityDelaySeconds
		if total > 0 {
			return time.Duration(total*2) * time.Second
		}
	}
	return 15 * time.Minute
}

// String returns a human-readable representation of GeneralConfig with the
// private key and RPC credentials redacted.
func (g GeneralConfig) String() string {
	return fmt.Sprintf("GeneralConfig{L1RPC:%s L2RPC:%s NodeRPC:%s WorkDir:%s PrivateKey:%s}",
		redact.URL(g.L1RPC), redact.URL(g.L2RPC), redact.URL(g.NodeRPC), g.WorkDir, redact.Key(g.PrivateKey))
}

// String returns a human-readable representation of Config with the
// private key and RPC credentials redacted. This prevents accidental leaking
// of secrets via fmt.Printf("%v", cfg) or similar patterns.
func (c Config) String() string {
	return fmt.Sprintf("Config{L1RPC:%s L2RPC:%s NodeRPC:%s WorkDir:%s PrivateKey:%s}",
		redact.URL(c.L1RPC), redact.URL(c.L2RPC), redact.URL(c.NodeRPC), c.WorkDir, redact.Key(c.PrivateKey))
}

// DefaultConfigFile returns a ConfigFile with sensible defaults.
func DefaultConfigFile() ConfigFile {
	return ConfigFile{
		General: GeneralConfig{
			L1RPC:   "https://public-node.testnet.rsk.co",
			L2RPC:   "http://127.0.0.1:8545",
			NodeRPC: "http://127.0.0.1:9545",
		},
		Monitor: MonitorConfig{
			PollInterval:     "2s",
			HistorySize:      100,
			HistoryBlocks:    500,
			TrafficRate:      0,
			BridgeAmountRBTC: "0.001",
		},
	}
}

// LoadConfigFile reads and parses the TOML config file. If the file does not exist,
// returns defaults.
func LoadConfigFile(path string) (ConfigFile, error) {
	file := DefaultConfigFile()
	if path == "" {
		return file, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return file, nil
		}
		return file, fmt.Errorf("read config %s: %w", path, err)
	}
	if _, err := toml.Decode(string(data), &file); err != nil {
		return file, fmt.Errorf("parse config %s: %w", path, err)
	}
	return file, nil
}

// ResolveConfig merges ConfigFile defaults with CLI flag overrides and loads rollup data
// from the workdir.
func ResolveConfig(file ConfigFile, flagL1RPC, flagL2RPC, flagNodeRPC, flagWorkDir, flagPrivateKey, flagLoadStats string, flagHistoryBlocks int) (*Config, error) {
	cfg := &Config{
		L1RPC:              file.General.L1RPC,
		L2RPC:              file.General.L2RPC,
		NodeRPC:            file.General.NodeRPC,
		WorkDir:            file.General.WorkDir,
		PrivateKey:         NormalizePrivateKey(file.General.PrivateKey),
		HistorySize:        file.Monitor.HistorySize,
		HistoryBlocks:      file.Monitor.HistoryBlocks,
		TrafficRate:        file.Monitor.TrafficRate,
		TrafficWorkers:     file.Monitor.TrafficWorkers,
		TrafficAccounts:    file.Monitor.TrafficAccounts,
		BridgeAmountRBTC:   file.Monitor.BridgeAmountRBTC,
		USDRIFTokenL1:      file.Monitor.USDRIFTokenL1,
		USDRIFTokenL2:      file.Monitor.USDRIFTokenL2,
		BridgeAmountUSDRIF: file.Monitor.BridgeAmountUSDRIF,
		BatcherMetricsURL:  file.Monitor.BatcherMetricsURL,
		NodeMetricsURL:     file.Monitor.NodeMetricsURL,
		ProposerMetricsURL: file.Monitor.ProposerMetricsURL,
		NodePprofURL:       file.Monitor.NodePprofURL,
		BatcherPprofURL:    file.Monitor.BatcherPprofURL,
		ProposerPprofURL:   file.Monitor.ProposerPprofURL,
		ScanKeys:           normalizeScanKeys(file.Monitor.ScanKeys),
	}

	// CLI flag overrides
	if flagL1RPC != "" {
		cfg.L1RPC = flagL1RPC
	}
	if flagL2RPC != "" {
		cfg.L2RPC = flagL2RPC
	}
	if flagNodeRPC != "" {
		cfg.NodeRPC = flagNodeRPC
	}
	if flagWorkDir != "" {
		cfg.WorkDir = flagWorkDir
	}
	if flagPrivateKey != "" {
		cfg.PrivateKey = NormalizePrivateKey(flagPrivateKey)
	}

	// History blocks override
	if flagHistoryBlocks > 0 {
		cfg.HistoryBlocks = flagHistoryBlocks
	}
	// Load stats path override
	if flagLoadStats != "" {
		cfg.LoadStatsPath = flagLoadStats
	}

	// Env overrides
	if v := os.Getenv("ROLLUP_STATS_PRIVATE_KEY"); v != "" && cfg.PrivateKey == "" {
		cfg.PrivateKey = NormalizePrivateKey(v)
	}

	// Parse poll interval
	dur, err := time.ParseDuration(file.Monitor.PollInterval)
	if err != nil || dur <= 0 {
		dur = 2 * time.Second
	}
	cfg.PollInterval = dur

	// Parse TPS window duration
	tpsWin, err := time.ParseDuration(file.Monitor.TPSWindowDuration)
	if err != nil || tpsWin <= 0 {
		tpsWin = DefaultTPSWindowDuration
	}
	cfg.TPSWindowDuration = tpsWin

	// Defaults
	if cfg.HistorySize <= 0 {
		cfg.HistorySize = 100
	}
	// Ensure ring buffer can hold at least history_blocks data points,
	// otherwise replayed blocks and CSV rows get silently truncated.
	if cfg.HistoryBlocks > cfg.HistorySize {
		cfg.HistorySize = cfg.HistoryBlocks
	}
	if cfg.BridgeAmountRBTC == "" {
		cfg.BridgeAmountRBTC = "0.001"
	}
	if cfg.TrafficWorkers <= 0 {
		cfg.TrafficWorkers = 64
	}
	if cfg.TrafficAccounts <= 0 {
		cfg.TrafficAccounts = 1
	}

	// Load rollup config and L1 contracts from workdir (optional — collector needs them)
	if cfg.WorkDir != "" {
		rollup, err := LoadRollupConfig(cfg.WorkDir)
		if err == nil {
			cfg.Rollup = rollup
		}
		addrs, err := LoadL1Contracts(cfg.WorkDir)
		if err == nil {
			cfg.L1Addrs = addrs
		}
		dc, err := LoadDeployConfig(cfg.WorkDir)
		if err == nil {
			cfg.DeployConfig = dc
		}
	}

	return cfg, nil
}

// NormalizePrivateKey strips 0x prefix and whitespace.
func NormalizePrivateKey(pk string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(pk), "0x"))
}

// normalizeScanKeys normalizes an array of private keys.
func normalizeScanKeys(keys []string) []string {
	var result []string
	for _, k := range keys {
		normalized := NormalizePrivateKey(k)
		if normalized != "" {
			result = append(result, normalized)
		}
	}
	return result
}

// --- Rollup config loading (from workdir) ---

// RollupConfig holds the fields from rollup.json that we need.
type RollupConfig struct {
	BatchInboxAddress      string `json:"batch_inbox_address"`
	BlockTime              uint64 `json:"block_time"`
	DepositContractAddress string `json:"deposit_contract_address"`
	Genesis                struct {
		L1 struct {
			Hash   string `json:"hash"`
			Number uint64 `json:"number"`
		} `json:"l1"`
		L2 struct {
			Hash   string `json:"hash"`
			Number uint64 `json:"number"`
		} `json:"l2"`
		L2Time       uint64 `json:"l2_time"`
		SystemConfig struct {
			BatcherAddr string `json:"batcherAddr"`
		} `json:"system_config"`
	} `json:"genesis"`
	L1ChainID uint64 `json:"l1_chain_id"`
	L2ChainID uint64 `json:"l2_chain_id"`

	// Fork activation timestamps (math.MaxUint64 = not scheduled)
	RegolithTime *uint64 `json:"regolith_time,omitempty"`
	CanyonTime   *uint64 `json:"canyon_time,omitempty"`
	DeltaTime    *uint64 `json:"delta_time,omitempty"`
	EcotoneTime  *uint64 `json:"ecotone_time,omitempty"`
	FjordTime    *uint64 `json:"fjord_time,omitempty"`
	GraniteTime  *uint64 `json:"granite_time,omitempty"`
	HoloceneTime *uint64 `json:"holocene_time,omitempty"`
	IsthmusTime  *uint64 `json:"isthmus_time,omitempty"`
}

// IsForkActive returns true if the given fork activation timestamp represents
// a scheduled (active) fork. A fork is considered inactive if the pointer is
// nil or the timestamp equals math.MaxUint64.
func (c *RollupConfig) IsForkActive(forkTime *uint64) bool {
	return forkTime != nil && *forkTime != math.MaxUint64
}

// BatchInbox returns the batch inbox address.
func (c *RollupConfig) BatchInbox() common.Address {
	return common.HexToAddress(c.BatchInboxAddress)
}

// BatcherAddress returns the batcher address from genesis system config.
func (c *RollupConfig) BatcherAddress() common.Address {
	return common.HexToAddress(c.Genesis.SystemConfig.BatcherAddr)
}

// DepositContract returns the OptimismPortal (deposit contract) address.
func (c *RollupConfig) DepositContract() common.Address {
	return common.HexToAddress(c.DepositContractAddress)
}

// L1Contracts maps contract names to hex addresses (from l1.json).
type L1Contracts map[string]string

// L1StandardBridge returns the L1StandardBridge proxy address.
func (l L1Contracts) L1StandardBridge() common.Address {
	return common.HexToAddress(l["L1StandardBridgeProxy"])
}

// OptimismPortal returns the OptimismPortal proxy address.
func (l L1Contracts) OptimismPortal() common.Address {
	return common.HexToAddress(l["OptimismPortalProxy"])
}

// DisputeGameFactory returns the DisputeGameFactory proxy address.
func (l L1Contracts) DisputeGameFactory() common.Address {
	return common.HexToAddress(l["DisputeGameFactoryProxy"])
}

// LoadRollupConfig reads rollup.json from workdir.
func LoadRollupConfig(workDir string) (*RollupConfig, error) {
	path := filepath.Join(workDir, "rollup.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rollup.json: %w", err)
	}
	var cfg RollupConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse rollup.json: %w", err)
	}
	return &cfg, nil
}

// LoadL1Contracts reads l1.json from workdir.
func LoadL1Contracts(workDir string) (L1Contracts, error) {
	path := filepath.Join(workDir, "l1.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read l1.json: %w", err)
	}
	var addrs L1Contracts
	if err := json.Unmarshal(data, &addrs); err != nil {
		return nil, fmt.Errorf("parse l1.json: %w", err)
	}
	return addrs, nil
}

// LoadDeployConfig reads deploy-config.json from workdir.
func LoadDeployConfig(workDir string) (*DeployConfig, error) {
	path := filepath.Join(workDir, "deploy-config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read deploy-config.json: %w", err)
	}
	var dc DeployConfig
	if err := json.Unmarshal(data, &dc); err != nil {
		return nil, fmt.Errorf("parse deploy-config.json: %w", err)
	}
	return &dc, nil
}

// SaveConfigField reads the TOML config file, updates a single field identified by
// its TOML key (e.g. "usdrif_token_l1"), and writes it back. This allows the TUI
// to persist values entered via the prompt overlay.
func SaveConfigField(configPath, key, value string) error {
	if configPath == "" {
		return fmt.Errorf("config path not set")
	}

	file, err := LoadConfigFile(configPath)
	if err != nil {
		return fmt.Errorf("load config for save: %w", err)
	}

	// Map TOML key to the correct struct field.
	switch key {
	// [general] section
	case "l1_rpc":
		file.General.L1RPC = value
	case "l2_rpc":
		file.General.L2RPC = value
	case "node_rpc":
		file.General.NodeRPC = value
	case "workdir":
		file.General.WorkDir = value
	case "private_key":
		file.General.PrivateKey = value

	// [monitor] section
	case "usdrif_token_l1":
		file.Monitor.USDRIFTokenL1 = value
	case "usdrif_token_l2":
		file.Monitor.USDRIFTokenL2 = value
	case "bridge_amount_usdrif":
		file.Monitor.BridgeAmountUSDRIF = value
	case "bridge_amount_rbtc":
		file.Monitor.BridgeAmountRBTC = value
	case "batcher_metrics_url":
		file.Monitor.BatcherMetricsURL = value
	case "node_metrics_url":
		file.Monitor.NodeMetricsURL = value
	case "proposer_metrics_url":
		file.Monitor.ProposerMetricsURL = value
	case "node_pprof_url":
		file.Monitor.NodePprofURL = value
	case "batcher_pprof_url":
		file.Monitor.BatcherPprofURL = value
	case "proposer_pprof_url":
		file.Monitor.ProposerPprofURL = value

	default:
		return fmt.Errorf("unknown config key: %s", key)
	}

	f, err := os.Create(configPath)
	if err != nil {
		return fmt.Errorf("create config file: %w", err)
	}
	defer f.Close()

	enc := toml.NewEncoder(f)
	if err := enc.Encode(file); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return nil
}

// SetConfigValue updates the runtime Config for a given TOML key. Returns true if the
// key was recognized and updated.
func (c *Config) SetConfigValue(key, value string) bool {
	switch key {
	case "l1_rpc":
		c.L1RPC = value
	case "l2_rpc":
		c.L2RPC = value
	case "node_rpc":
		c.NodeRPC = value
	case "workdir":
		c.WorkDir = value
	case "private_key":
		c.PrivateKey = NormalizePrivateKey(value)
	case "usdrif_token_l1":
		c.USDRIFTokenL1 = value
	case "usdrif_token_l2":
		c.USDRIFTokenL2 = value
	case "bridge_amount_usdrif":
		c.BridgeAmountUSDRIF = value
	case "bridge_amount_rbtc":
		c.BridgeAmountRBTC = value
	case "batcher_metrics_url":
		c.BatcherMetricsURL = value
	case "node_metrics_url":
		c.NodeMetricsURL = value
	case "proposer_metrics_url":
		c.ProposerMetricsURL = value
	case "node_pprof_url":
		c.NodePprofURL = value
	case "batcher_pprof_url":
		c.BatcherPprofURL = value
	case "proposer_pprof_url":
		c.ProposerPprofURL = value
	default:
		return false
	}
	return true
}

// DefaultConfigFileContent returns TOML content for a new rollup-monitor.toml config file.
// The config is expected to live inside the deployment workdir (e.g. 31_12_02_2026_93115/).
func DefaultConfigFileContent() string {
	return `# rollup-monitor configuration
# This file lives inside the deployment workdir alongside rollup.json and l1.json.
# Run with: rollup-monitor --workdir <this_directory>

[general]
# L1 RPC URL (e.g. RSK testnet)
l1_rpc = "https://public-node.testnet.rsk.co"
# L2 RPC URL (e.g. local op-geth)
l2_rpc = "http://127.0.0.1:8545"
# op-node RPC URL for sync status (safe/unsafe head tracking)
node_rpc = "http://127.0.0.1:9545"
# Private key (hex) for sending txs; or set env ROLLUP_STATS_PRIVATE_KEY
private_key = ""

[monitor]
# How often to poll for new blocks
poll_interval = "2s"
# Number of data points to keep in charts
history_size = 100
# Number of L2 blocks to replay from chain on startup (0 = disabled)
history_blocks = 500
# L2 traffic rate (txs/second, 0 = off). Toggle with 't' in the TUI.
traffic_rate = 0
# Number of concurrent traffic sender goroutines (higher = more throughput at high rates)
traffic_workers = 64
# Number of funded L2 sender accounts (1 = master only; increase with [ / ] in TUI)
traffic_accounts = 1
# Rolling window duration for TPS calculation (default 30s, adjustable with w/W in TUI)
tps_window_duration = "30s"
# Amount of RBTC for deposit/withdraw operations
bridge_amount_rbtc = "0.01"
# USDRIF L1 token address for ERC20 bridge operations (leave empty to disable)
usdrif_token_l1 = ""
# Amount of USDRIF for bridge operations
bridge_amount_usdrif = "0.001"
# Batcher Prometheus metrics URL for compression ratio (leave empty to disable)
# e.g. "http://127.0.0.1:7300/metrics"
batcher_metrics_url = ""
# op-node Prometheus metrics URL (leave empty to disable)
# e.g. "http://127.0.0.1:7301/metrics"
node_metrics_url = ""
# op-proposer Prometheus metrics URL (leave empty to disable)
# e.g. "http://127.0.0.1:7302/metrics"
proposer_metrics_url = ""
# pprof endpoints for runtime profiling (heap, goroutines). Leave empty to disable.
# node_pprof_url = "http://127.0.0.1:6061"
# batcher_pprof_url = "http://127.0.0.1:6063"
# proposer_pprof_url = "http://127.0.0.1:6062"
`
}
