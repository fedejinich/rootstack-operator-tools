package dashboard

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// LogLevel represents a parsed log level for filtering.
type LogLevel int

const (
	LevelTrace LogLevel = iota
	LevelDebug
	LevelInfo
	LevelWarn
	LevelError
	LevelCrit
	LevelUnknown
)

func (l LogLevel) String() string {
	switch l {
	case LevelTrace:
		return "TRACE"
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	case LevelCrit:
		return "CRIT"
	default:
		return "???"
	}
}

// LogContext identifies which rollup-node component emitted the log line.
type LogContext int

const (
	CtxGeneral LogContext = iota
	CtxExecution
	CtxConsensus
	CtxBatcher
	CtxProposer
)

var contextNames = [...]string{"GENERAL", "EXECUTION", "CONSENSUS", "BATCHER", "PROPOSER"}

func (c LogContext) String() string {
	if int(c) < len(contextNames) {
		return contextNames[c]
	}
	return "UNKNOWN"
}

func (c LogContext) Short() string {
	switch c {
	case CtxExecution:
		return "EXEC"
	case CtxConsensus:
		return "CONS"
	case CtxBatcher:
		return "BTCH"
	case CtxProposer:
		return "PROP"
	default:
		return "GEN"
	}
}

// LogEntry is a structured representation of a single log line from rollup-node.
type LogEntry struct {
	Time    time.Time
	Level   LogLevel
	Context LogContext
	Message string
	Raw     string
}

// contextPrefixes maps [PREFIX] strings emitted by the rollup-node's
// prefixHandler to LogContext values.
var contextPrefixes = map[string]LogContext{
	"[EXECUTION]": CtxExecution,
	"[CONSENSUS]": CtxConsensus,
	"[BATCHER]":   CtxBatcher,
	"[PROPOSER]":  CtxProposer,
}

// subsystemPrefixes maps short OP Stack logger subsystem names (rendered
// before the level by go-ethereum's TerminalHandler) to LogContext.
var subsystemPrefixes = map[string]LogContext{
	"BTCH": CtxBatcher,
	"PROP": CtxProposer,
}

// serviceValues maps service=<value> key-value pairs commonly found in
// batcher/proposer log lines to LogContext.
var serviceValues = map[string]LogContext{
	"service=batcher":           CtxBatcher,
	"service=batch-submitter":   CtxBatcher,
	"service=batcher-txmanager": CtxBatcher,
	"service=proposer":          CtxProposer,
}

// ansiRe strips ANSI escape sequences so string matching works on colored output.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z~]`)

// levelRe matches log level tokens in OP Stack terminal-formatted logs.
// Typical format: "BTCH INFO [03-06|15:04:05.123] [BATCHER] Some message key=value"
var levelRe = regexp.MustCompile(`\b(TRACE|TRC|DEBUG|DBG|INFO|INF|WARN|WRN|ERROR|ERR|CRIT)\b`)

func parseLogLevel(s string) LogLevel {
	switch strings.ToUpper(s) {
	case "TRACE", "TRC":
		return LevelTrace
	case "DEBUG", "DBG":
		return LevelDebug
	case "INFO", "INF":
		return LevelInfo
	case "WARN", "WRN":
		return LevelWarn
	case "ERROR", "ERR":
		return LevelError
	case "CRIT":
		return LevelCrit
	default:
		return LevelUnknown
	}
}

// ParseLogLine extracts level and context from a rollup-node log line.
// It strips ANSI escape codes before matching so that colored terminal
// output doesn't break context/level detection.
func ParseLogLine(line string) LogEntry {
	entry := LogEntry{
		Time:    time.Now(),
		Level:   LevelInfo,
		Context: CtxGeneral,
		Raw:     line,
	}

	clean := ansiRe.ReplaceAllString(line, "")

	// Extract level
	if m := levelRe.FindString(clean); m != "" {
		entry.Level = parseLogLevel(m)
	}

	// Detect context using three strategies (first match wins):
	// 1) [PREFIX] from rollup-node's prefixHandler
	for prefix, ctx := range contextPrefixes {
		if strings.Contains(clean, prefix) {
			entry.Context = ctx
			break
		}
	}

	// 2) OP Stack subsystem prefix at the start of the line (e.g. "BTCH INFO ...")
	if entry.Context == CtxGeneral {
		trimmed := strings.TrimSpace(clean)
		for prefix, ctx := range subsystemPrefixes {
			if strings.HasPrefix(trimmed, prefix+" ") || strings.HasPrefix(trimmed, prefix+"\t") {
				entry.Context = ctx
				break
			}
		}
	}

	// 3) service=<value> key-value pairs in the log attrs
	if entry.Context == CtxGeneral {
		for pattern, ctx := range serviceValues {
			if strings.Contains(clean, pattern) {
				entry.Context = ctx
				break
			}
		}
	}

	// Build the cleaned message (remove the context prefix for display)
	msg := line
	for prefix := range contextPrefixes {
		msg = strings.Replace(msg, prefix+" ", "", 1)
	}
	entry.Message = msg

	return entry
}

// NodeProcess manages the rollup-node subprocess.
type NodeProcess struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	running bool
	pid     int
	waited  chan struct{} // closed when cmd.Wait() returns

	binPath    string
	configPath string
	logCh      chan LogEntry
	logger     *slog.Logger
}

// NewNodeProcess creates a new subprocess manager.
func NewNodeProcess(binPath, configPath string, logCh chan LogEntry, logger *slog.Logger) *NodeProcess {
	return &NodeProcess{
		binPath:    binPath,
		configPath: configPath,
		logCh:      logCh,
		logger:     logger,
	}
}

// Start launches the rollup-node subprocess and begins streaming its output.
func (np *NodeProcess) Start(parentCtx context.Context) error {
	np.mu.Lock()
	defer np.mu.Unlock()

	if np.running {
		return fmt.Errorf("node already running")
	}

	// Don't use exec.CommandContext -- it sends SIGKILL on context cancel,
	// which prevents graceful shutdown. We manage signals ourselves.
	ctx, cancel := context.WithCancel(parentCtx)
	np.cancel = cancel

	args := []string{}
	if np.configPath != "" {
		args = append(args, "--config", np.configPath)
	}

	np.cmd = exec.Command(np.binPath, args...)
	np.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := np.cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := np.cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := np.cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start rollup-node: %w", err)
	}

	np.pid = np.cmd.Process.Pid
	np.running = true
	np.waited = make(chan struct{})

	go np.streamLogs(stdout)
	go np.streamLogs(stderr)

	// Wait for process exit in background. Does NOT hold the mutex.
	go func() {
		err := np.cmd.Wait()
		np.mu.Lock()
		np.running = false
		np.mu.Unlock()
		close(np.waited)
		if err != nil && ctx.Err() == nil {
			np.emitLog(LevelError, CtxGeneral, fmt.Sprintf("rollup-node exited: %v", err))
		} else if ctx.Err() == nil {
			np.emitLog(LevelInfo, CtxGeneral, "rollup-node exited normally")
		}
	}()

	np.emitLog(LevelInfo, CtxGeneral, fmt.Sprintf("rollup-node started (pid %d)", np.pid))
	return nil
}

// Stop gracefully stops the subprocess with SIGINT, then SIGKILL after timeout.
// Safe to call from any goroutine; does not deadlock.
//
// The rollup-node's stopConsensusLayer stops proposer, batcher, and op-node
// sequentially, each with a 10s timeout, so worst-case shutdown is ~30s.
// We allow 35s before resorting to SIGKILL.
func (np *NodeProcess) Stop() error {
	np.mu.Lock()
	if !np.running || np.cmd == nil || np.cmd.Process == nil {
		np.mu.Unlock()
		return nil
	}
	proc := np.cmd.Process
	waited := np.waited
	np.mu.Unlock()

	np.emitLog(LevelInfo, CtxGeneral, "stopping rollup-node (waiting up to 35s for graceful shutdown)...")

	// Send SIGINT to the subprocess's process group for graceful shutdown
	_ = syscall.Kill(-proc.Pid, syscall.SIGINT)

	// Wait for exit WITHOUT holding the mutex. 35s allows the rollup-node to
	// stop proposer (10s) + batcher (10s) + op-node (10s) + op-geth close.
	select {
	case <-waited:
		return nil
	case <-time.After(35 * time.Second):
		np.emitLog(LevelWarn, CtxGeneral, "rollup-node did not exit in 35s, sending SIGKILL")
		_ = proc.Kill()
		<-waited
		return nil
	}
}

// Restart stops and re-starts the subprocess.
func (np *NodeProcess) Restart(ctx context.Context) error {
	if err := np.Stop(); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	// Give the OS time to release all listening sockets (metrics, RPC, pprof)
	// before the new process tries to bind to the same ports.
	time.Sleep(2 * time.Second)
	return np.Start(ctx)
}

// IsRunning returns whether the subprocess is alive.
func (np *NodeProcess) IsRunning() bool {
	np.mu.Lock()
	defer np.mu.Unlock()
	return np.running
}

// PID returns the process ID, or 0 if not running.
func (np *NodeProcess) PID() int {
	np.mu.Lock()
	defer np.mu.Unlock()
	return np.pid
}

func (np *NodeProcess) streamLogs(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		entry := ParseLogLine(line)
		select {
		case np.logCh <- entry:
		default:
			// Drop if channel full to avoid blocking subprocess
		}
	}
}

func (np *NodeProcess) emitLog(level LogLevel, ctx LogContext, msg string) {
	entry := LogEntry{
		Time:    time.Now(),
		Level:   level,
		Context: ctx,
		Message: msg,
		Raw:     fmt.Sprintf("[DASHBOARD] %s", msg),
	}
	select {
	case np.logCh <- entry:
	default:
	}
}
