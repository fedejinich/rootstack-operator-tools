package dashboard

import (
	"fmt"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
)

// LogRingBuffer is a thread-safe circular buffer of LogEntry items.
type LogRingBuffer struct {
	mu    sync.RWMutex
	data  []LogEntry
	cap   int
	pos   int
	full  bool
	total int
}

func NewLogRingBuffer(capacity int) *LogRingBuffer {
	if capacity <= 0 {
		capacity = 10000
	}
	return &LogRingBuffer{
		data: make([]LogEntry, capacity),
		cap:  capacity,
	}
}

func (rb *LogRingBuffer) Add(entry LogEntry) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.data[rb.pos] = entry
	rb.pos++
	rb.total++
	if rb.pos >= rb.cap {
		rb.pos = 0
		rb.full = true
	}
}

func (rb *LogRingBuffer) Entries() []LogEntry {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	if !rb.full {
		out := make([]LogEntry, rb.pos)
		copy(out, rb.data[:rb.pos])
		return out
	}
	out := make([]LogEntry, rb.cap)
	copy(out, rb.data[rb.pos:])
	copy(out[rb.cap-rb.pos:], rb.data[:rb.pos])
	return out
}

func (rb *LogRingBuffer) Len() int {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	return rb.total
}

func (rb *LogRingBuffer) StoredLen() int {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	if rb.full {
		return rb.cap
	}
	return rb.pos
}

// LogPane manages log display state: filtering, scrolling, and rendering.
type LogPane struct {
	buffer *LogRingBuffer

	// Filter state: true = visible
	LevelFilter   [6]bool // indexed by LogLevel (0=Trace..5=Crit)
	ContextFilter [5]bool // indexed by LogContext (0=General..4=Proposer)

	ScrollOffset int  // 0 = bottom (most recent); positive = scrolled up
	Expanded     bool // full-screen overlay mode

	width  int
	height int
}

func NewLogPane(buffer *LogRingBuffer) *LogPane {
	return &LogPane{
		buffer: buffer,
		LevelFilter: [6]bool{
			true, // Trace
			true, // Debug
			true, // Info
			true, // Warn
			true, // Error
			true, // Crit
		},
		ContextFilter: [5]bool{
			true, // General
			true, // Execution
			true, // Consensus
			true, // Batcher
			true, // Proposer
		},
	}
}

func (lp *LogPane) SetSize(width, height int) {
	lp.width = width
	lp.height = height
}

// ToggleLevel toggles visibility of a log level.
func (lp *LogPane) ToggleLevel(level LogLevel) {
	if int(level) < len(lp.LevelFilter) {
		lp.LevelFilter[level] = !lp.LevelFilter[level]
	}
}

// ToggleContext toggles visibility of a log context.
func (lp *LogPane) ToggleContext(ctx LogContext) {
	if int(ctx) < len(lp.ContextFilter) {
		lp.ContextFilter[ctx] = !lp.ContextFilter[ctx]
	}
}

// Visible returns whether a log entry passes current filters.
func (lp *LogPane) Visible(entry LogEntry) bool {
	if int(entry.Level) < len(lp.LevelFilter) && !lp.LevelFilter[entry.Level] {
		return false
	}
	if int(entry.Context) < len(lp.ContextFilter) && !lp.ContextFilter[entry.Context] {
		return false
	}
	return true
}

// ScrollUp scrolls up by n lines.
func (lp *LogPane) ScrollUp(n int) {
	lp.ScrollOffset += n
	filtered := lp.filteredEntries()
	maxScroll := len(filtered) - lp.displayLines()
	if maxScroll < 0 {
		maxScroll = 0
	}
	if lp.ScrollOffset > maxScroll {
		lp.ScrollOffset = maxScroll
	}
}

// ScrollDown scrolls down by n lines.
func (lp *LogPane) ScrollDown(n int) {
	lp.ScrollOffset -= n
	if lp.ScrollOffset < 0 {
		lp.ScrollOffset = 0
	}
}

// ScrollToTop jumps to the oldest visible line.
func (lp *LogPane) ScrollToTop() {
	filtered := lp.filteredEntries()
	lp.ScrollOffset = len(filtered) - lp.displayLines()
	if lp.ScrollOffset < 0 {
		lp.ScrollOffset = 0
	}
}

// ScrollToBottom jumps to the newest line.
func (lp *LogPane) ScrollToBottom() {
	lp.ScrollOffset = 0
}

func (lp *LogPane) displayLines() int {
	// Reserve 2 lines for filter bar header
	lines := lp.height - 2
	if lines < 1 {
		lines = 1
	}
	return lines
}

func (lp *LogPane) filteredEntries() []LogEntry {
	all := lp.buffer.Entries()
	filtered := make([]LogEntry, 0, len(all))
	for _, e := range all {
		if lp.Visible(e) {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// Styles
var (
	logTimeStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	logLevelTrace   = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	logLevelDebug   = lipgloss.NewStyle().Foreground(lipgloss.Color("248"))
	logLevelInfo    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	logLevelWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	logLevelError   = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	logLevelCrit    = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	logCtxStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("105"))
	logMsgStyle     = lipgloss.NewStyle()
	filterActiveOn  = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	filterActiveOff = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	filterLabel     = lipgloss.NewStyle().Foreground(lipgloss.Color("248"))
)

func levelStyle(l LogLevel) lipgloss.Style {
	switch l {
	case LevelTrace:
		return logLevelTrace
	case LevelDebug:
		return logLevelDebug
	case LevelInfo:
		return logLevelInfo
	case LevelWarn:
		return logLevelWarn
	case LevelError:
		return logLevelError
	case LevelCrit:
		return logLevelCrit
	default:
		return logMsgStyle
	}
}

// RenderFilterBar renders the filter toggles header.
func (lp *LogPane) RenderFilterBar() string {
	var b strings.Builder

	b.WriteString(filterLabel.Render("Level: "))
	levels := []struct {
		level LogLevel
		key   string
	}{
		{LevelTrace, "1"}, {LevelDebug, "2"}, {LevelInfo, "3"},
		{LevelWarn, "4"}, {LevelError, "5"}, {LevelCrit, "6"},
	}
	for _, l := range levels {
		style := filterActiveOff
		if lp.LevelFilter[l.level] {
			style = filterActiveOn
		}
		b.WriteString(style.Render(fmt.Sprintf("[%s]%s ", l.key, l.level.String())))
	}

	b.WriteString("  ")
	b.WriteString(filterLabel.Render("Ctx: "))
	contexts := []struct {
		ctx LogContext
		key string
	}{
		{CtxGeneral, "g"}, {CtxExecution, "e"}, {CtxConsensus, "c"},
		{CtxBatcher, "b"}, {CtxProposer, "p"},
	}
	for _, c := range contexts {
		style := filterActiveOff
		if lp.ContextFilter[c.ctx] {
			style = filterActiveOn
		}
		b.WriteString(style.Render(fmt.Sprintf("[%s]%s ", c.key, c.ctx.Short())))
	}

	total := lp.buffer.StoredLen()
	filtered := len(lp.filteredEntries())
	b.WriteString(filterLabel.Render(fmt.Sprintf(" (%d/%d)", filtered, total)))

	return b.String()
}

// RenderLogs renders exactly lp.displayLines() visible log lines,
// padded with empty lines for stable layout (matching rollup-monitor pattern).
func (lp *LogPane) RenderLogs() []string {
	filtered := lp.filteredEntries()
	nLines := lp.displayLines()

	end := len(filtered) - lp.ScrollOffset
	if end < 0 {
		end = 0
	}
	start := end - nLines
	if start < 0 {
		start = 0
	}

	lines := make([]string, 0, nLines)
	for i := start; i < end; i++ {
		entry := filtered[i]
		ts := logTimeStyle.Render(entry.Time.Format("15:04:05"))
		lvl := levelStyle(entry.Level).Render(fmt.Sprintf("%-5s", entry.Level.String()))
		ctx := logCtxStyle.Render(fmt.Sprintf("%-4s", entry.Context.Short()))

		msg := strings.ReplaceAll(entry.Message, "\n", " ")

		prefixWidth := 8 + 1 + 5 + 1 + 4 + 1 // "15:04:05 INFO  EXEC "
		if lp.width > 0 {
			maxMsg := lp.width - prefixWidth
			if maxMsg < 0 {
				maxMsg = 0
			}
			if len(msg) > maxMsg {
				msg = msg[:maxMsg]
			}
		}

		lines = append(lines, fmt.Sprintf("%s %s %s %s", ts, lvl, ctx, msg))
	}

	if len(lines) == 0 {
		lines = append(lines, logTimeStyle.Render("  Waiting for logs..."))
	}

	// Pad to exact line count for stable layout
	for len(lines) < nLines {
		lines = append(lines, "")
	}

	return lines
}

// Render returns the complete log pane content as exactly lp.height lines.
// Line truncation/padding to fit the border is handled by manualBox.
func (lp *LogPane) Render() string {
	filterBar := lp.RenderFilterBar()
	separator := strings.Repeat("─", lp.width)
	logLines := lp.RenderLogs()

	all := make([]string, 0, lp.height)
	all = append(all, filterBar)
	all = append(all, separator)
	all = append(all, logLines...)

	if len(all) > lp.height {
		all = all[:lp.height]
	}
	for len(all) < lp.height {
		all = append(all, "")
	}

	return strings.Join(all, "\n")
}
