package remote

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/user"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// SSHClient wraps an SSH connection to a remote server.
type SSHClient struct {
	config *ServerConfig
	client *ssh.Client
}

// NewSSHClient creates a new SSH client for the given server config.
func NewSSHClient(cfg *ServerConfig) *SSHClient {
	return &SSHClient{config: cfg}
}

// Connect establishes an SSH connection using the SSH agent.
func (c *SSHClient) Connect() error {
	socket := os.Getenv("SSH_AUTH_SOCK")
	if socket == "" {
		return fmt.Errorf("SSH_AUTH_SOCK not set - is ssh-agent running?")
	}

	conn, err := net.Dial("unix", socket)
	if err != nil {
		return fmt.Errorf("connect to SSH agent: %w", err)
	}

	agentClient := agent.NewClient(conn)

	username := c.config.User
	if username == "" {
		u, err := user.Current()
		if err != nil {
			return fmt.Errorf("get current user: %w", err)
		}
		username = u.Username
	}

	host := c.config.Host
	if !strings.Contains(host, ":") {
		host = host + ":22"
	}

	sshConfig := &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeysCallback(agentClient.Signers),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // TODO: use known_hosts
	}

	client, err := ssh.Dial("tcp", host, sshConfig)
	if err != nil {
		return fmt.Errorf("SSH dial %s: %w", host, err)
	}

	c.client = client
	return nil
}

// Close closes the SSH connection.
func (c *SSHClient) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

// Run executes a command and returns the combined output.
func (c *SSHClient) Run(cmd string) (string, error) {
	if c.client == nil {
		return "", fmt.Errorf("not connected")
	}

	session, err := c.client.NewSession()
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	defer session.Close()

	var buf bytes.Buffer
	session.Stdout = &buf
	session.Stderr = &buf

	if err := session.Run(cmd); err != nil {
		return buf.String(), fmt.Errorf("run command: %w (output: %s)", err, buf.String())
	}

	return strings.TrimSpace(buf.String()), nil
}

// RunIgnoreError executes a command and returns output, ignoring errors.
func (c *SSHClient) RunIgnoreError(cmd string) string {
	output, _ := c.Run(cmd)
	return output
}

// IsConnected returns true if the SSH connection is established.
func (c *SSHClient) IsConnected() bool {
	return c.client != nil
}
