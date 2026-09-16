package remote

import (
	"fmt"
	"os"
)

// PrintStatus prints the status of all servers (non-interactive mode).
func PrintStatus(cfg *Config) error {
	fmt.Println("Connecting to servers...")

	for _, serverCfg := range cfg.Servers {
		s := NewServer(&serverCfg)
		if err := s.Connect(); err != nil {
			fmt.Printf("  %-12s %s ERROR: %v\n", serverCfg.Name, s.StatusSymbol(), err)
			continue
		}
		defer s.Disconnect()

		if err := s.RefreshStatus(); err != nil {
			fmt.Printf("  %-12s %s ERROR refreshing: %v\n", serverCfg.Name, s.StatusSymbol(), err)
			continue
		}

		fmt.Printf("  %-12s %s CONNECTED  tunnels: %s  services: %s\n",
			serverCfg.Name,
			s.StatusSymbol(),
			s.TunnelSummary(),
			s.ServiceSummary(),
		)

		// Print tunnel details
		for _, t := range s.Tunnels {
			status := "○"
			if t.Running {
				status = "●"
			}
			fmt.Printf("      tunnel: %s %s\n", status, t.Spec)
		}

		// Print service details
		for _, svc := range s.Services {
			status := "○"
			if svc.Running {
				status = "●"
			}
			fmt.Printf("      service: %s %s\n", status, svc.Name)
		}
	}

	return nil
}

// StartServer starts services on a specific server.
func StartServer(cfg *Config, name string) error {
	serverCfg, err := cfg.FindServer(name)
	if err != nil {
		return err
	}

	s := NewServer(serverCfg)
	if err := s.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer s.Disconnect()

	fmt.Printf("Starting services on %s...\n", name)
	for _, svc := range serverCfg.Services {
		if err := s.StartService(svc); err != nil {
			fmt.Printf("  %s: ERROR: %v\n", svc, err)
		} else {
			fmt.Printf("  %s: started\n", svc)
		}
	}

	return nil
}

// StopServer stops services on a specific server.
func StopServer(cfg *Config, name string) error {
	serverCfg, err := cfg.FindServer(name)
	if err != nil {
		return err
	}

	s := NewServer(serverCfg)
	if err := s.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer s.Disconnect()

	fmt.Printf("Stopping services on %s...\n", name)
	for _, svc := range serverCfg.Services {
		if err := s.StopService(svc); err != nil {
			fmt.Printf("  %s: ERROR: %v\n", svc, err)
		} else {
			fmt.Printf("  %s: stopped\n", svc)
		}
	}

	return nil
}

// StopAllServers stops services on all servers.
func StopAllServers(cfg *Config) error {
	for _, serverCfg := range cfg.Servers {
		if err := StopServer(cfg, serverCfg.Name); err != nil {
			fmt.Printf("Warning: %s: %v\n", serverCfg.Name, err)
		}
	}
	return nil
}

// TailLogs tails logs from servers.
func TailLogs(cfg *Config, serverName string, follow bool) error {
	var servers []*ServerConfig

	if serverName == "" {
		for i := range cfg.Servers {
			servers = append(servers, &cfg.Servers[i])
		}
	} else {
		s, err := cfg.FindServer(serverName)
		if err != nil {
			return err
		}
		servers = append(servers, s)
	}

	for _, serverCfg := range servers {
		s := NewServer(serverCfg)
		if err := s.Connect(); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] connect error: %v\n", serverCfg.Name, err)
			continue
		}

		logOutput, err := s.TailLog(50)
		s.Disconnect()

		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] log error: %v\n", serverCfg.Name, err)
			continue
		}

		fmt.Printf("=== %s ===\n", serverCfg.Name)
		fmt.Println(logOutput)
		fmt.Println()
	}

	if follow {
		fmt.Println("Note: --follow not yet implemented for non-interactive mode")
		fmt.Println("Use 'rollup-remote' (TUI mode) for live log streaming")
	}

	return nil
}

// ExecCommand executes a command on a specific server.
func ExecCommand(cfg *Config, serverName, command string) error {
	serverCfg, err := cfg.FindServer(serverName)
	if err != nil {
		return err
	}

	s := NewServer(serverCfg)
	if err := s.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer s.Disconnect()

	output, err := s.RunCommand(command)
	if err != nil {
		return fmt.Errorf("command failed: %w", err)
	}

	fmt.Println(output)
	return nil
}
