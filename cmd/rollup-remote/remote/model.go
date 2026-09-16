package remote

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Model is the Bubble Tea model for the TUI.
type Model struct {
	config        *Config
	servers       []*Server
	selectedIdx   int
	width, height int
	spinner       spinner.Model
	connecting    bool
	logs          []string
	showHelp      bool
}

// Messages
type (
	connectDoneMsg struct {
		idx int
		err error
	}
	refreshDoneMsg struct {
		idx int
		err error
	}
)

// NewModel creates a new TUI model.
func NewModel(cfg *Config) Model {
	servers := make([]*Server, len(cfg.Servers))
	for i := range cfg.Servers {
		servers[i] = NewServer(&cfg.Servers[i])
	}

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	return Model{
		config:     cfg,
		servers:    servers,
		spinner:    s,
		connecting: true,
	}
}

// Init initializes the model.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spinner.Tick}

	// Connect to all servers
	for i := range m.servers {
		idx := i
		cmds = append(cmds, func() tea.Msg {
			err := m.servers[idx].Connect()
			return connectDoneMsg{idx: idx, err: err}
		})
	}

	return tea.Batch(cmds...)
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			// Disconnect all servers
			for _, s := range m.servers {
				s.Disconnect()
			}
			return m, tea.Quit
		case "?":
			m.showHelp = !m.showHelp
		case "j", "down":
			if m.selectedIdx < len(m.servers)-1 {
				m.selectedIdx++
			}
		case "k", "up":
			if m.selectedIdx > 0 {
				m.selectedIdx--
			}
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			idx := int(msg.String()[0] - '1')
			if idx < len(m.servers) {
				m.selectedIdx = idx
			}
		case "r":
			// Refresh selected server
			if m.selectedIdx < len(m.servers) {
				idx := m.selectedIdx
				return m, func() tea.Msg {
					err := m.servers[idx].RefreshStatus()
					return refreshDoneMsg{idx: idx, err: err}
				}
			}
		case "R":
			// Refresh all servers
			var cmds []tea.Cmd
			for i := range m.servers {
				idx := i
				cmds = append(cmds, func() tea.Msg {
					err := m.servers[idx].RefreshStatus()
					return refreshDoneMsg{idx: idx, err: err}
				})
			}
			return m, tea.Batch(cmds...)
		case "s":
			// Start services on selected server
			if m.selectedIdx < len(m.servers) {
				s := m.servers[m.selectedIdx]
				for _, svc := range s.Config.Services {
					if err := s.StartService(svc); err != nil {
						m.logs = append(m.logs, fmt.Sprintf("[%s] start error: %v", s.Config.Name, err))
					} else {
						m.logs = append(m.logs, fmt.Sprintf("[%s] started %s", s.Config.Name, svc))
					}
				}
			}
		case "t":
			// Stop services on selected server
			if m.selectedIdx < len(m.servers) {
				s := m.servers[m.selectedIdx]
				for _, svc := range s.Config.Services {
					if err := s.StopService(svc); err != nil {
						m.logs = append(m.logs, fmt.Sprintf("[%s] stop error: %v", s.Config.Name, err))
					} else {
						m.logs = append(m.logs, fmt.Sprintf("[%s] stopped %s", s.Config.Name, svc))
					}
				}
			}
		case "l":
			// Show logs from selected server
			if m.selectedIdx < len(m.servers) {
				s := m.servers[m.selectedIdx]
				logOutput, err := s.TailLog(20)
				if err != nil {
					m.logs = append(m.logs, fmt.Sprintf("[%s] log error: %v", s.Config.Name, err))
				} else {
					m.logs = append(m.logs, fmt.Sprintf("--- Logs from %s ---", s.Config.Name))
					m.logs = append(m.logs, strings.Split(logOutput, "\n")...)
				}
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case connectDoneMsg:
		if msg.err != nil {
			m.logs = append(m.logs, fmt.Sprintf("[%s] connect error: %v", m.servers[msg.idx].Config.Name, msg.err))
		} else {
			m.logs = append(m.logs, fmt.Sprintf("[%s] connected", m.servers[msg.idx].Config.Name))
			// Refresh status after connecting
			idx := msg.idx
			return m, func() tea.Msg {
				err := m.servers[idx].RefreshStatus()
				return refreshDoneMsg{idx: idx, err: err}
			}
		}
		// Check if all connections are done
		allDone := true
		for _, s := range m.servers {
			if s.Status == StatusConnecting {
				allDone = false
				break
			}
		}
		if allDone {
			m.connecting = false
		}

	case refreshDoneMsg:
		if msg.err != nil {
			m.logs = append(m.logs, fmt.Sprintf("[%s] refresh error: %v", m.servers[msg.idx].Config.Name, msg.err))
		}

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

// View renders the TUI.
func (m Model) View() string {
	if m.showHelp {
		return m.helpView()
	}

	var b strings.Builder

	// Header
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("205")).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		Width(m.width)

	b.WriteString(headerStyle.Render("rollup-remote") + "\n\n")

	// Servers list
	b.WriteString("─ Servers ─\n")
	for i, s := range m.servers {
		selected := i == m.selectedIdx
		line := m.renderServerLine(i, s, selected)
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")

	// Selected server details
	if m.selectedIdx < len(m.servers) {
		s := m.servers[m.selectedIdx]
		b.WriteString(fmt.Sprintf("─ %s Details ─\n", s.Config.Name))
		b.WriteString(m.renderServerDetails(s))
		b.WriteString("\n")
	}

	// Logs
	b.WriteString("─ Logs ─\n")
	logLines := m.logs
	if len(logLines) > 5 {
		logLines = logLines[len(logLines)-5:]
	}
	for _, line := range logLines {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")

	// Footer
	footerStyle := lipgloss.NewStyle().Faint(true)
	b.WriteString(footerStyle.Render("[s]tart  s[t]op  [r]efresh  [l]ogs  [?]help  [q]uit"))

	return b.String()
}

func (m Model) renderServerLine(idx int, s *Server, selected bool) string {
	style := lipgloss.NewStyle()
	if selected {
		style = style.Bold(true).Foreground(lipgloss.Color("205"))
	}

	var statusColor string
	switch s.Status {
	case StatusConnected:
		statusColor = "42" // green
	case StatusConnecting:
		statusColor = "226" // yellow
	case StatusError:
		statusColor = "196" // red
	default:
		statusColor = "245" // gray
	}

	statusStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(statusColor))
	symbol := statusStyle.Render(s.StatusSymbol())

	prefix := "  "
	if selected {
		prefix = "> "
	}

	return fmt.Sprintf("%s[%d] %s %s %-12s  tunnels: %s  services: %s",
		prefix,
		idx+1,
		style.Render(fmt.Sprintf("%-12s", s.Config.Name)),
		symbol,
		s.Status.String(),
		s.TunnelSummary(),
		s.ServiceSummary(),
	)
}

func (m Model) renderServerDetails(s *Server) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("  Host: %s\n", s.Config.Host))
	b.WriteString(fmt.Sprintf("  Role: %s\n", s.Config.Role))

	if s.Status == StatusConnected {
		b.WriteString("\n  Tunnels:\n")
		for _, t := range s.Tunnels {
			status := "○"
			if t.Running {
				status = "●"
			}
			b.WriteString(fmt.Sprintf("    %s %s\n", status, t.Spec))
		}

		b.WriteString("\n  Services:\n")
		for _, svc := range s.Services {
			status := "○"
			if svc.Running {
				status = "●"
			}
			b.WriteString(fmt.Sprintf("    %s %s", status, svc.Name))
			if svc.Running && svc.Windows > 0 {
				b.WriteString(fmt.Sprintf(" (%d windows)", svc.Windows))
			}
			b.WriteString("\n")
		}
	} else if s.Error != "" {
		b.WriteString(fmt.Sprintf("\n  Error: %s\n", s.Error))
	}

	return b.String()
}

func (m Model) helpView() string {
	help := `
  rollup-remote - TUI Control Panel

  Navigation:
    j/↓         Move down
    k/↑         Move up
    1-9         Select server by number
    ?           Toggle this help

  Actions:
    s           Start services on selected server
    t           Stop services on selected server
    r           Refresh selected server status
    R           Refresh all servers
    l           Show logs from selected server

  General:
    q/Ctrl+c    Quit

  Press any key to return...
`
	return help
}
