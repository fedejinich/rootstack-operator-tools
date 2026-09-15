package dashboard

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration wraps time.Duration for TOML string parsing.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalText(text []byte) error {
	var err error
	d.Duration, err = time.ParseDuration(string(text))
	return err
}

// rollupNodeTOML is a minimal parse of the rollup-node TOML to extract
// endpoint information without importing the full rollup-node config package.
type rollupNodeTOML struct {
	L1RPC            *string          `toml:"l1_rpc"`
	MasterPrivateKey *string          `toml:"master_private_key"`
	Workdir          *string          `toml:"workdir"`
	Batcher          *batcherSection  `toml:"batcher"`
	Proposer         *proposerSection `toml:"proposer"`
	Node             *nodeSection     `toml:"node"`

	configDir string
}

type batcherSection struct {
	Enabled                *bool     `toml:"enabled"`
	PollInterval           *Duration `toml:"poll_interval"`
	MaxChannelDuration     *uint64   `toml:"max_channel_duration"`
	MaxL1TxSize            *uint64   `toml:"max_l1_tx_size"`
	SubSafetyMargin        *uint64   `toml:"sub_safety_margin"`
	MaxPendingTransactions *uint64   `toml:"max_pending_transactions"`
	MaxBlocksPerSpanBatch  *int      `toml:"max_blocks_per_span_batch"`
	TargetNumFrames        *int      `toml:"target_num_frames"`
	ApproxComprRatio       *float64  `toml:"approx_compr_ratio"`
	Compressor             *string   `toml:"compressor"`
	CompressionAlgo        *string   `toml:"compression_algo"`
	BatchType              *uint     `toml:"batch_type"`
	DataAvailabilityType   *string   `toml:"data_availability_type"`

	Metrics *metricsTOML `toml:"metrics"`
	Pprof   *pprofTOML   `toml:"pprof"`
	RPC     *rpcTOML     `toml:"rpc"`
	Log     *logTOML     `toml:"log"`
}

type proposerSection struct {
	Enabled *bool        `toml:"enabled"`
	Metrics *metricsTOML `toml:"metrics"`
	Pprof   *pprofTOML   `toml:"pprof"`
	RPC     *rpcTOML     `toml:"rpc"`
}

type nodeSection struct {
	Metrics *metricsTOML `toml:"metrics"`
	Pprof   *pprofTOML   `toml:"pprof"`
	RPC     *rpcTOML     `toml:"rpc"`
}

type metricsTOML struct {
	Enabled    *bool   `toml:"enabled"`
	ListenAddr *string `toml:"listen_addr"`
	ListenPort *int    `toml:"listen_port"`
}

type pprofTOML struct {
	Enabled    *bool   `toml:"enabled"`
	ListenAddr *string `toml:"listen_addr"`
	ListenPort *int    `toml:"listen_port"`
}

type rpcTOML struct {
	ListenAddr  *string `toml:"listen_addr"`
	ListenPort  *int    `toml:"listen_port"`
	EnableAdmin *bool   `toml:"enable_admin"`
}

type logTOML struct {
	Level *string `toml:"level"`
}

// Config holds the resolved runtime configuration for the dashboard.
type Config struct {
	RollupNodeBin    string
	RollupNodeConfig string
	LogBufferSize    int
	PollInterval     time.Duration

	// Master key for deriving batcher/proposer addresses
	MasterPrivateKey string

	// Endpoints derived from the rollup-node config
	L1RPC              string
	L2RPC              string
	NodeRPC            string
	BatcherRPC         string
	BatcherMetricsURL  string
	NodeMetricsURL     string
	ProposerMetricsURL string
	ProposerRPC        string
	NodePprofURL       string
	BatcherPprofURL    string
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		RollupNodeBin: "rollup-node",
		LogBufferSize: 10000,
		PollInterval:  2 * time.Second,
		L2RPC:         "http://127.0.0.1:8545",
		NodeRPC:       "http://127.0.0.1:9545",
		BatcherRPC:    "http://127.0.0.1:8548",
	}
}

// LoadConfig loads the dashboard configuration by parsing the rollup-node TOML
// to discover RPC and Prometheus endpoints.
func LoadConfig(rollupNodeBin, rollupNodeConfig string) (Config, error) {
	cfg := DefaultConfig()
	cfg.RollupNodeBin = rollupNodeBin
	cfg.RollupNodeConfig = rollupNodeConfig

	if rollupNodeConfig == "" {
		return cfg, nil
	}

	absPath, err := filepath.Abs(rollupNodeConfig)
	if err != nil {
		return cfg, fmt.Errorf("resolve config path: %w", err)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}

	var nodeCfg rollupNodeTOML
	if err := toml.Unmarshal(data, &nodeCfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	nodeCfg.configDir = filepath.Dir(absPath)

	if nodeCfg.L1RPC != nil {
		cfg.L1RPC = *nodeCfg.L1RPC
	}
	if nodeCfg.MasterPrivateKey != nil {
		cfg.MasterPrivateKey = *nodeCfg.MasterPrivateKey
	}

	// Derive Prometheus and pprof endpoints from the component configs.
	// The rollup-node uses default ports:
	//   batcher metrics: 7300, pprof: 6063, rpc: 8548
	//   node metrics: 7301, pprof: 6061
	//   proposer metrics: 7302

	if nodeCfg.Batcher != nil {
		if m := nodeCfg.Batcher.Metrics; m != nil && derefBool(m.Enabled) {
			addr := derefString(m.ListenAddr, "127.0.0.1")
			port := derefInt(m.ListenPort, 7300)
			cfg.BatcherMetricsURL = fmt.Sprintf("http://%s:%d/metrics", addr, port)
		}
		if p := nodeCfg.Batcher.Pprof; p != nil && derefBool(p.Enabled) {
			addr := derefString(p.ListenAddr, "127.0.0.1")
			port := derefInt(p.ListenPort, 6063)
			cfg.BatcherPprofURL = fmt.Sprintf("http://%s:%d", addr, port)
		}
		if r := nodeCfg.Batcher.RPC; r != nil {
			addr := derefString(r.ListenAddr, "127.0.0.1")
			port := derefInt(r.ListenPort, 8548)
			cfg.BatcherRPC = fmt.Sprintf("http://%s:%d", addr, port)
		}
	}

	if nodeCfg.Node != nil {
		if m := nodeCfg.Node.Metrics; m != nil && derefBool(m.Enabled) {
			addr := derefString(m.ListenAddr, "127.0.0.1")
			port := derefInt(m.ListenPort, 7301)
			cfg.NodeMetricsURL = fmt.Sprintf("http://%s:%d/metrics", addr, port)
		}
		if p := nodeCfg.Node.Pprof; p != nil && derefBool(p.Enabled) {
			addr := derefString(p.ListenAddr, "127.0.0.1")
			port := derefInt(p.ListenPort, 6061)
			cfg.NodePprofURL = fmt.Sprintf("http://%s:%d", addr, port)
		}
		if r := nodeCfg.Node.RPC; r != nil {
			addr := derefString(r.ListenAddr, "127.0.0.1")
			port := derefInt(r.ListenPort, 9545)
			cfg.NodeRPC = fmt.Sprintf("http://%s:%d", addr, port)
		}
	}

	if nodeCfg.Proposer != nil {
		if m := nodeCfg.Proposer.Metrics; m != nil && derefBool(m.Enabled) {
			addr := derefString(m.ListenAddr, "127.0.0.1")
			port := derefInt(m.ListenPort, 7302)
			cfg.ProposerMetricsURL = fmt.Sprintf("http://%s:%d/metrics", addr, port)
		}
		if r := nodeCfg.Proposer.RPC; r != nil {
			addr := derefString(r.ListenAddr, "127.0.0.1")
			port := derefInt(r.ListenPort, 8560)
			cfg.ProposerRPC = fmt.Sprintf("http://%s:%d", addr, port)
		}
	}

	return cfg, nil
}

// BatcherSettings returns the current batcher settings parsed from the TOML.
func LoadBatcherSettings(rollupNodeConfig string) (BatcherSettings, error) {
	var s BatcherSettings
	s.SetDefaults()

	if rollupNodeConfig == "" {
		return s, nil
	}

	data, err := os.ReadFile(rollupNodeConfig)
	if err != nil {
		return s, fmt.Errorf("read config: %w", err)
	}

	var nodeCfg rollupNodeTOML
	if err := toml.Unmarshal(data, &nodeCfg); err != nil {
		return s, fmt.Errorf("parse config: %w", err)
	}

	if b := nodeCfg.Batcher; b != nil {
		if b.MaxL1TxSize != nil {
			s.MaxL1TxSize = *b.MaxL1TxSize
		}
		if b.MaxChannelDuration != nil {
			s.MaxChannelDuration = *b.MaxChannelDuration
		}
		if b.SubSafetyMargin != nil {
			s.SubSafetyMargin = *b.SubSafetyMargin
		}
		if b.MaxPendingTransactions != nil {
			s.MaxPendingTransactions = *b.MaxPendingTransactions
		}
		if b.PollInterval != nil {
			s.PollInterval = b.PollInterval.Duration
		}
		if b.Compressor != nil {
			s.Compressor = *b.Compressor
		}
		if b.CompressionAlgo != nil {
			s.CompressionAlgo = *b.CompressionAlgo
		}
		if b.TargetNumFrames != nil {
			s.TargetNumFrames = *b.TargetNumFrames
		}
		if b.ApproxComprRatio != nil {
			s.ApproxComprRatio = *b.ApproxComprRatio
		}
		if b.BatchType != nil {
			s.BatchType = *b.BatchType
		}
		if b.DataAvailabilityType != nil {
			s.DataAvailabilityType = *b.DataAvailabilityType
		}
	}

	return s, nil
}

func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

func derefString(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}
	return *p
}

func derefInt(p *int, fallback int) int {
	if p == nil {
		return fallback
	}
	return *p
}
