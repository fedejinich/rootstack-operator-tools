package remote

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ServerStatus represents the connection status of a server.
type ServerStatus int

const (
	StatusDisconnected ServerStatus = iota
	StatusConnecting
	StatusConnected
	StatusError
)

func (s ServerStatus) String() string {
	switch s {
	case StatusDisconnected:
		return "DISCONNECTED"
	case StatusConnecting:
		return "CONNECTING"
	case StatusConnected:
		return "CONNECTED"
	case StatusError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// TunnelStatus represents the status of an SSH tunnel.
type TunnelStatus struct {
	Spec    string // e.g., "4444:rskj1:4444"
	Running bool
}

// ServiceStatus represents the status of a tmux service.
type ServiceStatus struct {
	Name    string
	Running bool
	Windows int
}

// Server represents a connected remote server with its status.
type Server struct {
	Config   *ServerConfig
	SSH      *SSHClient
	Status   ServerStatus
	Error    string
	Tunnels  []TunnelStatus
	Services []ServiceStatus
	CPU      float64
	Memory   uint64
}

// NewServer creates a new Server instance.
func NewServer(cfg *ServerConfig) *Server {
	return &Server{
		Config: cfg,
		SSH:    NewSSHClient(cfg),
		Status: StatusDisconnected,
	}
}

// Connect establishes the SSH connection.
func (s *Server) Connect() error {
	s.Status = StatusConnecting
	if err := s.SSH.Connect(); err != nil {
		s.Status = StatusError
		s.Error = err.Error()
		return err
	}
	s.Status = StatusConnected
	s.Error = ""
	return nil
}

// Disconnect closes the SSH connection.
func (s *Server) Disconnect() {
	if s.SSH != nil {
		s.SSH.Close()
	}
	s.Status = StatusDisconnected
}

// RefreshStatus updates the server's tunnel and service status.
func (s *Server) RefreshStatus() error {
	if s.Status != StatusConnected {
		return fmt.Errorf("not connected")
	}

	// Check tunnels
	s.Tunnels = make([]TunnelStatus, len(s.Config.Tunnels))
	autosshOutput := s.SSH.RunIgnoreError("pgrep -af autossh 2>/dev/null || true")
	for i, tunnel := range s.Config.Tunnels {
		s.Tunnels[i] = TunnelStatus{
			Spec:    tunnel,
			Running: strings.Contains(autosshOutput, extractPort(tunnel)),
		}
	}

	// Check tmux sessions
	s.Services = make([]ServiceStatus, len(s.Config.Services))
	tmuxOutput := s.SSH.RunIgnoreError("tmux list-sessions -F '#{session_name}:#{session_windows}' 2>/dev/null || true")
	for i, service := range s.Config.Services {
		s.Services[i] = ServiceStatus{
			Name:    service,
			Running: false,
		}
		for _, line := range strings.Split(tmuxOutput, "\n") {
			parts := strings.Split(line, ":")
			if len(parts) >= 2 && parts[0] == service {
				s.Services[i].Running = true
				s.Services[i].Windows, _ = strconv.Atoi(parts[1])
				break
			}
		}
	}

	// Get basic system stats
	cpuOutput := s.SSH.RunIgnoreError("top -bn1 | grep 'Cpu(s)' | awk '{print $2}' 2>/dev/null || echo 0")
	s.CPU, _ = strconv.ParseFloat(strings.TrimSuffix(cpuOutput, "%"), 64)

	return nil
}

// StartService starts a tmux session/service.
func (s *Server) StartService(service string) error {
	if s.Status != StatusConnected {
		return fmt.Errorf("not connected")
	}

	// Look for the launcher script based on role
	launcherPath := fmt.Sprintf("~/rollup/scripts/launchers/%s-launch.sh", s.Config.Role)
	_, err := s.SSH.Run(fmt.Sprintf("%s start", launcherPath))
	return err
}

// StopService stops a tmux session/service.
func (s *Server) StopService(service string) error {
	if s.Status != StatusConnected {
		return fmt.Errorf("not connected")
	}

	launcherPath := fmt.Sprintf("~/rollup/scripts/launchers/%s-launch.sh", s.Config.Role)
	_, err := s.SSH.Run(fmt.Sprintf("%s stop", launcherPath))
	return err
}

// TailLog returns recent log lines from the server.
func (s *Server) TailLog(lines int) (string, error) {
	if s.Status != StatusConnected {
		return "", fmt.Errorf("not connected")
	}

	if s.Config.LogPath == "" {
		return "", fmt.Errorf("no log_path configured for %s", s.Config.Name)
	}

	output, err := s.SSH.Run(fmt.Sprintf("tail -n %d %s 2>/dev/null || echo '(log file not found)'", lines, s.Config.LogPath))
	return output, err
}

// extractPort extracts the local port from a tunnel spec like "4444:host:4444"
func extractPort(spec string) string {
	parts := strings.Split(spec, ":")
	if len(parts) > 0 {
		return parts[0]
	}
	return spec
}

// StatusSymbol returns a symbol representing the server status.
func (s *Server) StatusSymbol() string {
	switch s.Status {
	case StatusConnected:
		return "●"
	case StatusConnecting:
		return "◐"
	case StatusError:
		return "✗"
	default:
		return "○"
	}
}

// TunnelSummary returns a summary of tunnel status.
func (s *Server) TunnelSummary() string {
	if len(s.Tunnels) == 0 {
		return "no tunnels"
	}
	running := 0
	for _, t := range s.Tunnels {
		if t.Running {
			running++
		}
	}
	return fmt.Sprintf("%d/%d", running, len(s.Tunnels))
}

// ServiceSummary returns a summary of service status.
func (s *Server) ServiceSummary() string {
	if len(s.Services) == 0 {
		return "no services"
	}
	running := 0
	for _, svc := range s.Services {
		if svc.Running {
			running++
		}
	}
	return fmt.Sprintf("%d/%d", running, len(s.Services))
}

// RunCommand executes an arbitrary command on the server.
func (s *Server) RunCommand(cmd string) (string, error) {
	if s.Status != StatusConnected {
		return "", fmt.Errorf("not connected")
	}
	return s.SSH.Run(cmd)
}

var uptimeRegex = regexp.MustCompile(`up\s+(\d+)\s*days?,?\s*(\d+):(\d+)`)

// GetUptime returns the server uptime.
func (s *Server) GetUptime() (time.Duration, error) {
	if s.Status != StatusConnected {
		return 0, fmt.Errorf("not connected")
	}

	output, err := s.SSH.Run("uptime")
	if err != nil {
		return 0, err
	}

	matches := uptimeRegex.FindStringSubmatch(output)
	if len(matches) < 4 {
		return 0, fmt.Errorf("could not parse uptime: %s", output)
	}

	days, _ := strconv.Atoi(matches[1])
	hours, _ := strconv.Atoi(matches[2])
	mins, _ := strconv.Atoi(matches[3])

	return time.Duration(days)*24*time.Hour +
		time.Duration(hours)*time.Hour +
		time.Duration(mins)*time.Minute, nil
}
