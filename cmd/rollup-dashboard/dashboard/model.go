package dashboard

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Bubble Tea message types
type (
	tickMsg         time.Time
	logDrainMsg     struct{}
	nodeExitedMsg   struct{ err error }
	restartDoneMsg  struct{ err error }
	shutdownDoneMsg struct{}
	fundDoneMsg     struct {
		role string
		err  error
	}
	controlResultMsg struct {
		component string // "sequencer", "batcher", "proposer"
		action    string // "start", "stop", "flush"
		err       error
	}
)

// Pane focus targets
type paneID int

const (
	paneLog paneID = iota
	paneSettings
)

// Model is the top-level Bubble Tea model for the dashboard.
type Model struct {
	ctx    context.Context
	cancel context.CancelFunc
	cfg    Config

	process   *NodeProcess
	logBuffer *LogRingBuffer
	logCh     chan LogEntry
	logPane   *LogPane
	health    *HealthCollector
	settings  *SettingsPanel
	accounts  *AccountsPanel

	focusedPane paneID
	showHelp    bool
	helpPage    int

	width  int
	height int

	statusMsg     string
	statusAt      time.Time
	statusPersist bool // when true, status never expires
	attachMode    bool // when true, don't start/manage a subprocess
	quitting      bool // true while graceful shutdown is in progress
}

func NewModel(ctx context.Context, cfg Config, batcherSettings BatcherSettings, attachMode bool) Model {
	ctx, cancel := context.WithCancel(ctx)
	logBuffer := NewLogRingBuffer(cfg.LogBufferSize)
	logCh := make(chan LogEntry, 1000)

	return Model{
		ctx:        ctx,
		cancel:     cancel,
		cfg:        cfg,
		logBuffer:  logBuffer,
		logCh:      logCh,
		logPane:    NewLogPane(logBuffer),
		health:     NewHealthCollector(cfg),
		settings:   NewSettingsPanel(batcherSettings, cfg.BatcherRPC),
		accounts:   NewAccountsPanel(cfg.MasterPrivateKey),
		process:    NewNodeProcess(cfg.RollupNodeBin, cfg.RollupNodeConfig, logCh, nil),
		attachMode: attachMode,
	}
}

// Init starts the rollup-node subprocess (unless in attach mode) and health collector.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		m.startHealthCollector(),
		m.startAccountsPoller(),
		m.tickCmd(),
		m.drainLogsCmd(),
	}
	if m.attachMode {
		m.logBuffer.Add(LogEntry{
			Time: time.Now(), Level: LevelInfo, Context: CtxGeneral,
			Message: "attach mode: monitoring existing rollup-node (no subprocess)",
			Raw:     "[DASHBOARD] attach mode: monitoring existing rollup-node (no subprocess)",
		})
	} else {
		cmds = append(cmds, m.startNode())
	}
	return tea.Batch(cmds...)
}

func (m Model) startNode() tea.Cmd {
	return func() tea.Msg {
		if err := m.process.Start(m.ctx); err != nil {
			return nodeExitedMsg{err: err}
		}
		return nil
	}
}

func (m Model) startHealthCollector() tea.Cmd {
	return func() tea.Msg {
		m.health.Start(m.ctx)
		return nil
	}
}

func (m Model) startAccountsPoller() tea.Cmd {
	return func() tea.Msg {
		if m.accounts == nil {
			return nil
		}
		ticker := time.NewTicker(m.cfg.PollInterval * 5)
		defer ticker.Stop()
		m.accounts.PollBalances(m.ctx, m.cfg.L1RPC, m.cfg.L2RPC)
		for {
			select {
			case <-m.ctx.Done():
				return nil
			case <-ticker.C:
				m.accounts.PollBalances(m.ctx, m.cfg.L1RPC, m.cfg.L2RPC)
			}
		}
	}
}

func (m Model) tickCmd() tea.Cmd {
	return tea.Tick(m.cfg.PollInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m Model) drainLogsCmd() tea.Cmd {
	return func() tea.Msg {
		// Drain log channel in batch, or block briefly
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()

		for {
			select {
			case entry := <-m.logCh:
				m.logBuffer.Add(entry)
			case <-timer.C:
				return logDrainMsg{}
			}
		}
	}
}

// Update handles Bubble Tea messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tickMsg:
		return m, m.tickCmd()

	case logDrainMsg:
		return m, m.drainLogsCmd()

	case nodeExitedMsg:
		if msg.err != nil {
			errStr := fmt.Sprintf("node error: %v", msg.err)
			m.setPersistentStatus(errStr)
			m.logBuffer.Add(LogEntry{
				Time: time.Now(), Level: LevelError, Context: CtxGeneral,
				Message: errStr,
				Raw:     "[DASHBOARD] " + errStr,
			})
		}
		return m, nil

	case restartDoneMsg:
		if msg.err != nil {
			errStr := fmt.Sprintf("restart failed: %v", msg.err)
			m.setPersistentStatus(errStr)
			m.logBuffer.Add(LogEntry{
				Time: time.Now(), Level: LevelError, Context: CtxGeneral,
				Message: errStr,
				Raw:     "[DASHBOARD] " + errStr,
			})
		} else {
			m.setStatus("settings applied — rollup-node restarted (node + batcher + proposer)")
		}
		// Recreate health collector here (on the live Model), not in the
		// tea.Cmd goroutine which operates on a stale copy.
		m.health.Stop()
		m.health = NewHealthCollector(m.cfg)
		return m, m.startHealthCollector()

	case shutdownDoneMsg:
		return m, tea.Quit

	case fundDoneMsg:
		if m.accounts != nil {
			if msg.err != nil {
				m.accounts.SetStatus(fmt.Sprintf("funding failed: %v", msg.err))
			} else {
				m.accounts.SetStatus(fmt.Sprintf("funded %s successfully", msg.role))
			}
			m.accounts.CancelFunding()
		}
		return m, nil

	case controlResultMsg:
		if msg.err != nil {
			m.setStatus(fmt.Sprintf("%s %s failed: %v", msg.component, msg.action, msg.err))
			m.logBuffer.Add(LogEntry{
				Time: time.Now(), Level: LevelError, Context: CtxGeneral,
				Message: fmt.Sprintf("%s %s failed: %v", msg.component, msg.action, msg.err),
				Raw:     fmt.Sprintf("[DASHBOARD] %s %s failed: %v", msg.component, msg.action, msg.err),
			})
		} else {
			actionVerb := msg.action
			switch msg.action {
			case "stop":
				actionVerb = "stopped (paused)"
				m.health.SetActiveState(msg.component, false)
			case "start":
				actionVerb = "started (resumed)"
				m.health.SetActiveState(msg.component, true)
			case "flush":
				actionVerb = "flush triggered"
			}
			m.setStatus(fmt.Sprintf("%s %s", msg.component, actionVerb))
			m.logBuffer.Add(LogEntry{
				Time: time.Now(), Level: LevelInfo, Context: CtxGeneral,
				Message: fmt.Sprintf("%s %s", msg.component, actionVerb),
				Raw:     fmt.Sprintf("[DASHBOARD] %s %s", msg.component, actionVerb),
			})
		}
		return m, nil
	}

	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Global keys that work regardless of focus
	switch key {
	case "ctrl+c", "q":
		if m.quitting {
			return m, tea.Quit
		}
		if m.settings.IsEditing() {
			m.settings.CancelEdit()
			return m, nil
		}
		if m.showHelp {
			m.showHelp = false
			return m, nil
		}
		if m.accounts != nil && m.accounts.Visible {
			if m.accounts.IsFunding() {
				m.accounts.CancelFunding()
			} else {
				m.accounts.Visible = false
			}
			return m, nil
		}
		if m.logPane.Expanded {
			m.logPane.Expanded = false
			m.layout()
			return m, nil
		}
		m.quitting = true
		m.setPersistentStatus("shutting down... (press q/Ctrl+C again to force-quit)")
		return m, m.gracefulShutdown()

	case "esc":
		if m.accounts != nil && m.accounts.Visible {
			if m.accounts.IsFunding() {
				m.accounts.CancelFunding()
			} else {
				m.accounts.Visible = false
			}
			return m, nil
		}

	case "?":
		if m.showHelp {
			m.showHelp = false
		} else {
			m.showHelp = true
			m.helpPage = 0
		}
		return m, nil

	case "$":
		if m.accounts != nil && !m.settings.IsEditing() {
			m.accounts.Visible = !m.accounts.Visible
			return m, nil
		}

	case "tab":
		if m.settings.IsEditing() {
			return m, nil
		}
		if m.focusedPane == paneLog {
			m.focusedPane = paneSettings
			m.settings.Collapsed = false
		} else {
			m.focusedPane = paneLog
			m.settings.Collapsed = true
		}
		m.layout()
		return m, nil

	case "l":
		if !m.settings.IsEditing() {
			m.logPane.Expanded = !m.logPane.Expanded
			m.layout()
			return m, nil
		}

	case "S":
		if !m.settings.IsEditing() {
			return m, m.toggleSequencer()
		}

	case "B":
		if !m.settings.IsEditing() {
			return m, m.toggleBatcher()
		}

	case "P":
		if !m.settings.IsEditing() {
			return m, m.toggleProposer()
		}

	case "F":
		if !m.settings.IsEditing() {
			return m, m.flushBatcher()
		}
	}

	// Help overlay key handling (intercepts all keys except those handled above)
	if m.showHelp {
		switch key {
		case "left", "h":
			if m.helpPage > 0 {
				m.helpPage--
			}
		case "right", "l":
			if m.helpPage < helpPageCount-1 {
				m.helpPage++
			}
		default:
			m.showHelp = false
		}
		return m, nil
	}

	// Accounts popover key handling (when visible, intercept all keys)
	if m.accounts != nil && m.accounts.Visible {
		return m.handleAccountsKey(msg)
	}

	// Pane-specific key handling
	if m.focusedPane == paneSettings && !m.settings.Collapsed {
		return m.handleSettingsKey(msg)
	}
	return m.handleLogKey(msg)
}

func (m *Model) handleAccountsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	if m.accounts.IsFunding() {
		switch key {
		case "enter":
			addr, amountWei, err := m.accounts.FundAmount()
			if err != nil {
				m.accounts.SetStatus(fmt.Sprintf("invalid: %v", err))
				return m, nil
			}
			role := m.accounts.SelectedRole()
			masterKey := m.accounts.MasterKey()
			l1RPC := m.cfg.L1RPC
			m.accounts.SetStatus(fmt.Sprintf("sending %s to %s...", m.accounts.fundBuf, role))
			return m, func() tea.Msg {
				_, err := SendFunding(m.ctx, l1RPC, masterKey, addr, amountWei)
				return fundDoneMsg{role: role, err: err}
			}
		default:
			m.accounts.HandleFundingKey(key)
		}
		return m, nil
	}

	switch key {
	case "j", "down":
		m.accounts.MoveDown()
	case "k", "up":
		m.accounts.MoveUp()
	case "f":
		m.accounts.StartFunding()
	}
	return m, nil
}

func (m *Model) handleLogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	switch key {
	// Level filters: 1-6
	case "1":
		m.logPane.ToggleLevel(LevelTrace)
	case "2":
		m.logPane.ToggleLevel(LevelDebug)
	case "3":
		m.logPane.ToggleLevel(LevelInfo)
	case "4":
		m.logPane.ToggleLevel(LevelWarn)
	case "5":
		m.logPane.ToggleLevel(LevelError)
	case "6":
		m.logPane.ToggleLevel(LevelCrit)

	// Context filters
	case "e":
		m.logPane.ToggleContext(CtxExecution)
	case "c":
		m.logPane.ToggleContext(CtxConsensus)
	case "b":
		m.logPane.ToggleContext(CtxBatcher)
	case "p":
		m.logPane.ToggleContext(CtxProposer)
	case "g":
		m.logPane.ToggleContext(CtxGeneral)

	// Scrolling
	case "j", "down":
		m.logPane.ScrollDown(1)
	case "k", "up":
		m.logPane.ScrollUp(1)
	case "pgdown":
		m.logPane.ScrollDown(10)
	case "pgup":
		m.logPane.ScrollUp(10)
	case "home":
		m.logPane.ScrollToTop()
	case "end":
		m.logPane.ScrollToBottom()
	case "G":
		m.logPane.ScrollToBottom()
	}

	return m, nil
}

func (m *Model) handleSettingsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	if m.settings.IsEditing() {
		switch key {
		case "enter":
			if errMsg := m.settings.CommitEdit(); errMsg != "" {
				m.setStatus(errMsg)
			}
		case "esc":
			m.settings.CancelEdit()
		case "backspace":
			m.settings.Backspace()
		default:
			if len(key) == 1 {
				m.settings.TypeChar(rune(key[0]))
			}
		}
		return m, nil
	}

	switch key {
	case "j", "down":
		m.settings.MoveDown()
	case "k", "up":
		m.settings.MoveUp()
	case "enter":
		m.settings.StartEdit()
	case " ":
		m.settings.CycleOption()
	case "A":
		return m, m.applyAndRestart()
	case "tab":
		m.settings.Collapsed = true
		m.focusedPane = paneLog
		m.layout()
	}

	return m, nil
}

func (m *Model) applyAndRestart() tea.Cmd {
	return func() tea.Msg {
		errMsg := m.settings.Apply()
		if errMsg != "" {
			return restartDoneMsg{err: fmt.Errorf("%s", errMsg)}
		}

		// Config changes require a full node restart because the batcher
		// admin RPC (stop/start) only pauses submission — it does not
		// re-read the config from disk. All embedded components (node,
		// batcher, proposer) will restart.
		m.logBuffer.Add(LogEntry{
			Time: time.Now(), Level: LevelInfo, Context: CtxGeneral,
			Message: "config saved — restarting rollup-node to apply new batcher settings...",
			Raw:     "[DASHBOARD] restarting rollup-node",
		})

		if err := m.process.Restart(m.ctx); err != nil {
			return restartDoneMsg{err: err}
		}

		// NOTE: health collector recreation happens in the restartDoneMsg
		// handler inside Update(), not here. This closure runs on a
		// background goroutine with a stale Model copy — any pointer
		// reassignment (m.health = ...) would be invisible to the live
		// Model that Bubble Tea uses.
		m.logBuffer.Add(LogEntry{
			Time: time.Now(), Level: LevelInfo, Context: CtxGeneral,
			Message: "rollup-node restarted (node + batcher + proposer all reinitialized)",
			Raw:     "[DASHBOARD] rollup-node restarted",
		})
		return restartDoneMsg{}
	}
}

// borderChrome is the number of lines added by a rounded border (top + bottom).
const borderChrome = 2

// healthContentLines is the fixed number of content lines in the health bar
// (1 title + 3 health data lines).
const healthContentLines = 4

// boxWidth returns the total width for bordered sections.
// Uses m.width-1 to leave a 1-column right margin that prevents
// terminal wrapping from ANSI escape code width miscalculations.
func (m *Model) boxWidth() int {
	w := m.width - 1
	if w < 12 {
		w = 12
	}
	return w
}

func (m *Model) layout() {
	if m.width == 0 || m.height == 0 {
		return
	}

	healthTotal := healthContentLines + borderChrome // 6
	footerHeight := 1
	settingsHeight := m.settings.Height()

	logContent := m.height - healthTotal - settingsHeight - footerHeight - borderChrome
	if logContent < 3 {
		logContent = 3
	}

	if m.logPane.Expanded {
		logContent = m.height - footerHeight - borderChrome
	}

	innerWidth := m.boxWidth() - 2 // inside left+right border chars
	if innerWidth < 10 {
		innerWidth = 10
	}

	m.logPane.SetSize(innerWidth, logContent)
}

func (m *Model) setStatus(msg string) {
	m.statusMsg = msg
	m.statusAt = time.Now()
	m.statusPersist = false
}

func (m *Model) setPersistentStatus(msg string) {
	m.statusMsg = msg
	m.statusAt = time.Now()
	m.statusPersist = true
}

func (m *Model) gracefulShutdown() tea.Cmd {
	return func() tea.Msg {
		m.health.Stop()
		// Best-effort: we are exiting either way, and the TUI is already torn
		// down, so there is nowhere left to report a stop failure.
		_ = m.process.Stop()
		m.cancel()
		return shutdownDoneMsg{}
	}
}

func (m *Model) toggleSequencer() tea.Cmd {
	m.health.SetPending("sequencer", true)
	m.setStatus("sequencer: sending command...")
	return func() tea.Msg {
		defer m.health.SetPending("sequencer", false)
		snap := m.health.Health().Snapshot()
		if snap.SequencerActive {
			err := m.health.StopSequencer(m.ctx)
			if err != nil && strings.Contains(err.Error(), "already stopped") {
				return controlResultMsg{component: "sequencer", action: "stop", err: nil}
			}
			return controlResultMsg{component: "sequencer", action: "stop", err: err}
		}
		// Need unsafe head hash to start sequencer
		hash, err := m.health.GetUnsafeHeadHash(m.ctx)
		if err != nil {
			return controlResultMsg{component: "sequencer", action: "start", err: fmt.Errorf("get unsafe head: %w", err)}
		}
		err = m.health.StartSequencer(m.ctx, hash)
		if err != nil && strings.Contains(err.Error(), "already running") {
			return controlResultMsg{component: "sequencer", action: "start", err: nil}
		}
		return controlResultMsg{component: "sequencer", action: "start", err: err}
	}
}

func (m *Model) toggleBatcher() tea.Cmd {
	m.health.SetPending("batcher", true)
	m.setStatus("batcher: sending command...")
	return func() tea.Msg {
		defer m.health.SetPending("batcher", false)
		snap := m.health.Health().Snapshot()
		if snap.BatcherActive {
			err := m.health.StopBatcher(m.ctx)
			if err != nil && strings.Contains(err.Error(), "already stopped") {
				return controlResultMsg{component: "batcher", action: "stop", err: nil}
			}
			return controlResultMsg{component: "batcher", action: "stop", err: err}
		}
		err := m.health.StartBatcher(m.ctx)
		if err != nil && strings.Contains(err.Error(), "already started") {
			return controlResultMsg{component: "batcher", action: "start", err: nil}
		}
		return controlResultMsg{component: "batcher", action: "start", err: err}
	}
}

func (m *Model) toggleProposer() tea.Cmd {
	m.health.SetPending("proposer", true)
	m.setStatus("proposer: sending command...")
	return func() tea.Msg {
		defer m.health.SetPending("proposer", false)
		snap := m.health.Health().Snapshot()
		if snap.ProposerActive {
			err := m.health.StopProposer(m.ctx)
			if err != nil && strings.Contains(err.Error(), "already stopped") {
				return controlResultMsg{component: "proposer", action: "stop", err: nil}
			}
			return controlResultMsg{component: "proposer", action: "stop", err: err}
		}
		err := m.health.StartProposer(m.ctx)
		if err != nil && strings.Contains(err.Error(), "already started") {
			return controlResultMsg{component: "proposer", action: "start", err: nil}
		}
		return controlResultMsg{component: "proposer", action: "start", err: err}
	}
}

func (m *Model) flushBatcher() tea.Cmd {
	m.health.SetPending("batcher", true)
	m.setStatus("batcher: flushing...")
	return func() tea.Msg {
		defer m.health.SetPending("batcher", false)
		err := m.health.FlushBatcher(m.ctx)
		return controlResultMsg{component: "batcher", action: "flush", err: err}
	}
}

// View renders the full TUI.
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}

	if m.showHelp {
		return m.renderHelp()
	}

	if m.accounts != nil && m.accounts.Visible {
		return m.accounts.Render(m.width, m.height)
	}

	if m.logPane.Expanded {
		return m.renderExpandedLog()
	}

	// Build layout top to bottom.
	// Join with \n directly — NOT lipgloss.JoinVertical, which pads all
	// lines to the widest line across sections and can trigger terminal wrapping.
	snap := m.health.Health().Snapshot()

	var parts []string
	parts = append(parts, m.renderHealthSection(snap))
	parts = append(parts, m.renderLogSection())
	parts = append(parts, m.settings.Render())
	parts = append(parts, m.renderFooter())

	return fixedHeight(strings.Join(parts, "\n"), m.height)
}

// fixedHeight guarantees the output has exactly n newline-delimited lines.
// Excess lines are dropped from the bottom; missing lines are padded.
func fixedHeight(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	for len(lines) < n {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// --- Section renderers ---
//
// All borders are drawn manually with box-drawing characters to guarantee
// exact line/column counts. No lipgloss Width/Border/Height is used for
// layout-critical sections, avoiding wrapping/padding surprises.

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("99"))
	footerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	borderColor = lipgloss.NewStyle().Foreground(lipgloss.Color("62"))
	focusColor  = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))
)

// manualBox wraps content lines in a box of exactly `width` columns and
// len(contentLines)+2 rows. Each content line is hard-truncated to `width-2`
// visible characters so no terminal wrapping can occur.
func manualBox(contentLines []string, width int, color lipgloss.Style) string {
	inner := width - 2
	if inner < 0 {
		inner = 0
	}

	bl := color.Render("│")
	br := color.Render("│")

	out := make([]string, 0, len(contentLines)+2)
	out = append(out, color.Render("╭"+strings.Repeat("─", inner)+"╮"))

	for _, line := range contentLines {
		// Hard-truncate: walk runes, skip ANSI escapes in width count
		truncated := truncateVisual(line, inner)
		vw := lipgloss.Width(truncated)
		pad := ""
		if vw < inner {
			pad = strings.Repeat(" ", inner-vw)
		}
		out = append(out, bl+truncated+pad+br)
	}

	out = append(out, color.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return strings.Join(out, "\n")
}

// truncateVisual truncates s to at most maxWidth visible characters,
// preserving ANSI escape sequences. This avoids relying on lipgloss MaxWidth.
func truncateVisual(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	var b strings.Builder
	visible := 0
	inEsc := false

	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '\x1b' {
			inEsc = true
			b.WriteByte(ch)
			continue
		}
		if inEsc {
			b.WriteByte(ch)
			if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || ch == '~' {
				inEsc = false
			}
			continue
		}
		if visible >= maxWidth {
			break
		}
		b.WriteByte(ch)
		visible++
	}
	return b.String()
}

func (m Model) renderHealthSection(h HealthSnapshot) string {
	bw := m.boxWidth()
	inner := bw - 2

	var title string
	if m.attachMode {
		title = " Rollup Dashboard — attached (monitoring external node) "
	} else {
		running := "running"
		runStyle := healthOK
		if !m.process.IsRunning() {
			running = "stopped"
			runStyle = healthBad
		}
		title = fmt.Sprintf(" Rollup Dashboard — node: %s (pid %d) ",
			runStyle.Render(running), m.process.PID())
	}

	healthLines := RenderHealthBar(h, inner)

	lines := make([]string, 0, healthContentLines)
	lines = append(lines, titleStyle.Render(title))
	lines = append(lines, strings.Split(healthLines, "\n")...)
	for len(lines) < healthContentLines {
		lines = append(lines, "")
	}
	lines = lines[:healthContentLines]

	return manualBox(lines, bw, borderColor)
}

func (m Model) renderLogSection() string {
	contentLines := strings.Split(m.logPane.Render(), "\n")
	color := borderColor
	if m.focusedPane == paneLog {
		color = focusColor
	}
	return manualBox(contentLines, m.boxWidth(), color)
}

func (m Model) renderExpandedLog() string {
	expandedContent := m.height - 1 - borderChrome // 1 footer
	if expandedContent < 3 {
		expandedContent = 3
	}
	bw := m.boxWidth()
	inner := bw - 2
	if inner < 10 {
		inner = 10
	}
	m.logPane.SetSize(inner, expandedContent)
	contentLines := strings.Split(m.logPane.Render(), "\n")

	footer := footerStyle.Render(" [l] collapse  [1-6] level  [e/c/b/p/g] context  [j/k/PgUp/PgDn] scroll  [q] quit")
	return fixedHeight(manualBox(contentLines, bw, focusColor)+"\n"+footer, m.height)
}

func (m Model) renderFooter() string {
	var parts []string

	if m.statusMsg != "" && (m.statusPersist || time.Since(m.statusAt) < 10*time.Second) {
		parts = append(parts, lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render(m.statusMsg))
	}

	help := "[?] help  [$] accounts  [S/B/P] toggle seq/batch/prop  [F] flush  [l] log  [q] quit"
	if m.focusedPane == paneSettings && !m.settings.Collapsed {
		help = "[?] help  [$] accounts  [Tab] switch pane  [A] apply+restart  [↑/↓] select  [Enter] edit  [q] quit"
	}

	parts = append(parts, footerStyle.Render(help))
	return strings.Join(parts, "  ")
}

const helpPageCount = 3

var helpPages = [helpPageCount]struct {
	title string
	body  string
}{
	{
		title: "Glossary",
		body: `  LED Indicators (like car dashboard)
  ────────────────────────────────────
    ● green       Active / running normally
    ◑ yellow      Pending (command in progress)
    ◐ yellow      Paused (use S/B/P to toggle)
    ○ red/gray    Down / idle

    SEQ           Sequencer (L2 block production)
    BATCH         Batcher (posting batches to L1)
    PROP          Proposer (posting output roots)
    DERIV         Derivation pipeline

  Block Numbers
  ─────────────
    L1            Latest L1 (RSK) block seen
    L2 / unsafe   L2 chain tip — latest sequenced block
    safe          Highest L2 block derived from L1 batch data
    finalized     Highest L2 block from finalized L1 data
    lag           unsafe − safe: sequenced but not yet confirmed

  Batcher / Proposer
  ──────────────────
    pending       Blocks queued but not yet posted to L1
    bumps         Gas-price bumps on stuck L1 transactions
    bal           Account balance (funds L1 tx fees)`,
	},
	{
		title: "Keyboard Shortcuts",
		body: `  General
    ?           Toggle this help
    $           Accounts & balances (fund roles)
    Tab         Switch between Log / Settings pane
    l           Expand/collapse log overlay
    q / Ctrl+C  Quit (graceful; press again to force)

  Node Controls (uppercase)
    S           Toggle sequencer (pause/resume L2 blocks)
    B           Toggle batcher (pause/resume batch posting)
    P           Toggle proposer (pause/resume proposals)
    F           Flush batcher (force post pending data)

  Log Pane (when focused)
    1-6         Toggle log level (Trace..Crit)
    e/c/b/p/g   Toggle context (Exec/Cons/Batch/Prop/Gen)
    j/k ↑/↓     Scroll          PgUp/PgDn  Page scroll
    G / End     Jump to bottom   Home       Jump to top

  Settings Pane (when focused)
    ↑ / ↓       Select field     Enter  Edit value
    Space       Cycle option     Esc    Cancel edit
    A           Apply + restart  Tab    Back to log pane`,
	},
	{
		title: "Architecture",
		body: `  L1 (RSK)                        L2 (op-geth)
  ─────────                       ────────────
  Provides data availability      Executes L2 transactions
  and finality anchor             and maintains chain state

        ┌──────────────┐
        │   Sequencer  │ ─── produces L2 blocks (unsafe)
        └──────┬───────┘
               │ batches
        ┌──────▼───────┐
        │   Batcher    │ ─── compresses & posts batches to L1
        └──────┬───────┘
               │ calldata on L1
        ┌──────▼───────┐
        │  Derivation  │ ─── re-derives L2 from L1 data (safe)
        └──────┬───────┘
               │
        ┌──────▼───────┐
        │  Proposer    │ ─── posts output roots to L1
        └──────────────┘

  Block lifecycle:  unsafe → safe → finalized
    unsafe     Sequencer produced it, not yet on L1
    safe       Batch data posted to L1, re-derivable
    finalized  Derived from finalized L1 blocks`,
	},
}

func (m Model) renderHelp() string {
	page := helpPages[m.helpPage]

	header := fmt.Sprintf("  Rollup Dashboard — %s  [%d/%d]",
		page.title, m.helpPage+1, helpPageCount)
	nav := "  ◀ ▶ change page    any other key to close"

	content := header + "\n  " + strings.Repeat("═", len(page.title)+14) + "\n" + page.body + "\n\n" + nav

	return lipgloss.Place(m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		lipgloss.NewStyle().
			Padding(1, 2).
			Border(lipgloss.DoubleBorder()).
			BorderForeground(lipgloss.Color("99")).
			Render(content),
		lipgloss.WithWhitespaceChars(" "),
	)
}
