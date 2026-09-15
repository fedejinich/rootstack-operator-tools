package dashboard

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// BatcherSettings holds the editable batcher configuration.
type BatcherSettings struct {
	MaxL1TxSize            uint64        `toml:"max_l1_tx_size"`
	MaxChannelDuration     uint64        `toml:"max_channel_duration"`
	SubSafetyMargin        uint64        `toml:"sub_safety_margin"`
	MaxPendingTransactions uint64        `toml:"max_pending_transactions"`
	PollInterval           time.Duration `toml:"poll_interval"`
	Compressor             string        `toml:"compressor"`
	CompressionAlgo        string        `toml:"compression_algo"`
	TargetNumFrames        int           `toml:"target_num_frames"`
	ApproxComprRatio       float64       `toml:"approx_compr_ratio"`
	BatchType              uint          `toml:"batch_type"`
	DataAvailabilityType   string        `toml:"data_availability_type"`
}

// SetDefaults applies the standard RSK/OP Stack defaults.
func (s *BatcherSettings) SetDefaults() {
	s.MaxL1TxSize = 120000
	s.MaxChannelDuration = 0
	s.SubSafetyMargin = 10
	s.MaxPendingTransactions = 1
	s.PollInterval = 2 * time.Second
	s.Compressor = "shadow"
	s.CompressionAlgo = "zlib"
	s.TargetNumFrames = 1
	s.ApproxComprRatio = 0.4
	s.BatchType = 0
	s.DataAvailabilityType = "calldata"
}

// SettingsField describes one editable field in the settings panel.
type SettingsField struct {
	Key     string
	Label   string
	Value   string
	Type    string   // "uint64", "float64", "string", "duration"
	Options []string // for string type: valid options
}

// Fields returns the settings as an ordered slice of editable fields.
func (s *BatcherSettings) Fields() []SettingsField {
	return []SettingsField{
		{Key: "max_l1_tx_size", Label: "Max L1 Tx Size", Value: fmt.Sprintf("%d", s.MaxL1TxSize), Type: "uint64"},
		{Key: "max_channel_duration", Label: "Max Channel Duration", Value: fmt.Sprintf("%d", s.MaxChannelDuration), Type: "uint64"},
		{Key: "sub_safety_margin", Label: "Sub-Safety Margin", Value: fmt.Sprintf("%d", s.SubSafetyMargin), Type: "uint64"},
		{Key: "max_pending_transactions", Label: "Max Pending Txs", Value: fmt.Sprintf("%d", s.MaxPendingTransactions), Type: "uint64"},
		{Key: "poll_interval", Label: "Poll Interval", Value: s.PollInterval.String(), Type: "duration"},
		{Key: "compressor", Label: "Compressor", Value: s.Compressor, Type: "string", Options: []string{"shadow", "ratio", "none"}},
		{Key: "compression_algo", Label: "Compression Algo", Value: s.CompressionAlgo, Type: "string", Options: []string{"zlib", "brotli", "brotli-9", "brotli-10", "brotli-11"}},
		{Key: "target_num_frames", Label: "Target Num Frames", Value: fmt.Sprintf("%d", s.TargetNumFrames), Type: "uint64"},
		{Key: "approx_compr_ratio", Label: "Approx Compr Ratio", Value: fmt.Sprintf("%.2f", s.ApproxComprRatio), Type: "float64"},
		{Key: "batch_type", Label: "Batch Type", Value: fmt.Sprintf("%d", s.BatchType), Type: "uint64"},
		{Key: "data_availability_type", Label: "DA Type", Value: s.DataAvailabilityType, Type: "string", Options: []string{"calldata", "blobs", "auto"}},
	}
}

// SetField updates a field by key from a string value.
func (s *BatcherSettings) SetField(key, value string) error {
	switch key {
	case "max_l1_tx_size":
		v, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return err
		}
		s.MaxL1TxSize = v
	case "max_channel_duration":
		v, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return err
		}
		s.MaxChannelDuration = v
	case "sub_safety_margin":
		v, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return err
		}
		s.SubSafetyMargin = v
	case "max_pending_transactions":
		v, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return err
		}
		s.MaxPendingTransactions = v
	case "poll_interval":
		d, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		s.PollInterval = d
	case "compressor":
		s.Compressor = value
	case "compression_algo":
		s.CompressionAlgo = value
	case "target_num_frames":
		v, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return err
		}
		s.TargetNumFrames = int(v)
	case "approx_compr_ratio":
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return err
		}
		s.ApproxComprRatio = v
	case "batch_type":
		v, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return err
		}
		s.BatchType = uint(v)
	case "data_availability_type":
		s.DataAvailabilityType = value
	default:
		return fmt.Errorf("unknown field: %s", key)
	}
	return nil
}

// SettingsPanel manages the interactive batcher settings editor.
type SettingsPanel struct {
	Settings  BatcherSettings
	fields    []SettingsField
	selected  int
	editing   bool
	editBuf   string
	Collapsed bool

	batcherRPC string // admin RPC URL for stop/start

	width  int
	height int

	statusMsg string
	statusAt  time.Time
}

func NewSettingsPanel(settings BatcherSettings, batcherRPC string) *SettingsPanel {
	sp := &SettingsPanel{
		Settings:   settings,
		batcherRPC: batcherRPC,
		Collapsed:  true,
	}
	sp.fields = sp.Settings.Fields()
	return sp
}

func (sp *SettingsPanel) SetSize(width, height int) {
	sp.width = width
	sp.height = height
}

// MoveUp moves selection up.
func (sp *SettingsPanel) MoveUp() {
	if sp.editing {
		return
	}
	sp.selected--
	if sp.selected < 0 {
		sp.selected = len(sp.fields) - 1
	}
}

// MoveDown moves selection down.
func (sp *SettingsPanel) MoveDown() {
	if sp.editing {
		return
	}
	sp.selected++
	if sp.selected >= len(sp.fields) {
		sp.selected = 0
	}
}

// StartEdit enters edit mode for the selected field.
func (sp *SettingsPanel) StartEdit() {
	if sp.selected < len(sp.fields) {
		sp.editing = true
		sp.editBuf = sp.fields[sp.selected].Value
	}
}

// CancelEdit exits edit mode without saving.
func (sp *SettingsPanel) CancelEdit() {
	sp.editing = false
	sp.editBuf = ""
}

// TypeChar adds a character to the edit buffer.
func (sp *SettingsPanel) TypeChar(ch rune) {
	if sp.editing {
		sp.editBuf += string(ch)
	}
}

// Backspace removes the last character from the edit buffer.
func (sp *SettingsPanel) Backspace() {
	if sp.editing && len(sp.editBuf) > 0 {
		sp.editBuf = sp.editBuf[:len(sp.editBuf)-1]
	}
}

// CycleOption cycles through options for string fields.
func (sp *SettingsPanel) CycleOption() {
	if sp.selected >= len(sp.fields) {
		return
	}
	f := sp.fields[sp.selected]
	if len(f.Options) == 0 {
		return
	}
	// Find current and cycle. Every field that declares Options is a plain
	// string field, and the value comes from that same list, so SetField
	// cannot reject it — unlike CommitEdit, which parses operator input.
	for i, opt := range f.Options {
		if opt == f.Value {
			next := f.Options[(i+1)%len(f.Options)]
			_ = sp.Settings.SetField(f.Key, next)
			sp.fields = sp.Settings.Fields()
			return
		}
	}
	// Not found, set first
	_ = sp.Settings.SetField(f.Key, f.Options[0])
	sp.fields = sp.Settings.Fields()
}

// CommitEdit saves the edited value.
func (sp *SettingsPanel) CommitEdit() string {
	if !sp.editing || sp.selected >= len(sp.fields) {
		return ""
	}
	f := sp.fields[sp.selected]
	if err := sp.Settings.SetField(f.Key, sp.editBuf); err != nil {
		sp.editing = false
		sp.editBuf = ""
		return fmt.Sprintf("invalid value for %s: %v", f.Key, err)
	}
	sp.fields = sp.Settings.Fields()
	sp.editing = false
	sp.editBuf = ""
	return ""
}

// IsEditing returns whether the panel is in edit mode.
func (sp *SettingsPanel) IsEditing() bool {
	return sp.editing
}

// Apply reports that the edited values cannot be persisted, and why.
//
// This used to write a [batcher] section into rollup-node.toml. That is no
// longer possible in either direction: the batcher does not read the file, so
// the values would reach nothing, and the deploy tooling rejects the retired
// [batcher] keys, so the write would leave rollup-node.toml unloadable for
// `just deploy` and `just up`. Set these as oprsk-batcher flags instead.
//
// PAYROLLUP-146 decides what becomes of the panel; until then it does not
// pretend to save.
func (sp *SettingsPanel) Apply() string {
	msg := "batcher settings are oprsk-batcher flags now, not TOML (docs/config-tuning.md)"
	sp.setStatus(msg)
	return msg
}

func (sp *SettingsPanel) setStatus(msg string) {
	sp.statusMsg = msg
	sp.statusAt = time.Now()
}

// --- Rendering ---

var (
	settingsTitle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99"))
	settingsKey      = lipgloss.NewStyle().Foreground(lipgloss.Color("248")).Width(24)
	settingsVal      = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	settingsSelected = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
	settingsEditVal  = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Underline(true)
	settingsHelp     = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	settingsStatus   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)

// Height returns the total number of lines this panel will render.
func (sp *SettingsPanel) Height() int {
	if sp.Collapsed {
		return 1
	}
	return len(sp.fields) + 2 // title + fields + status line
}

// Render returns the settings panel as exactly sp.Height() lines.
func (sp *SettingsPanel) Render() string {
	if sp.Collapsed {
		return settingsTitle.Render("▶ Batcher Settings") +
			settingsHelp.Render("  (Tab to expand)")
	}

	lines := make([]string, 0, sp.Height())

	// Title line
	lines = append(lines,
		settingsTitle.Render("▼ Batcher Settings")+
			settingsHelp.Render("  (Tab:collapse  ↑/↓:select  Enter:edit  Space:cycle  A:apply+restart)"))

	// Field lines
	for i, f := range sp.fields {
		prefix := "  "
		keyStyle := settingsKey
		valStyle := settingsVal

		if i == sp.selected {
			prefix = "▸ "
			keyStyle = settingsSelected
			valStyle = settingsSelected
		}

		displayVal := f.Value
		if i == sp.selected && sp.editing {
			displayVal = sp.editBuf + "█"
			valStyle = settingsEditVal
		}

		opts := ""
		if len(f.Options) > 0 && i == sp.selected {
			opts = settingsHelp.Render(fmt.Sprintf(" [%s]", strings.Join(f.Options, "|")))
		}

		lines = append(lines, fmt.Sprintf("%s%s %s%s",
			prefix,
			keyStyle.Render(f.Label+":"),
			valStyle.Render(displayVal),
			opts,
		))
	}

	// Status line (always present for stable height; blank if no message)
	statusLine := ""
	if sp.statusMsg != "" && time.Since(sp.statusAt) < 10*time.Second {
		statusLine = settingsStatus.Render("  " + sp.statusMsg)
	}
	lines = append(lines, statusLine)

	return strings.Join(lines, "\n")
}
