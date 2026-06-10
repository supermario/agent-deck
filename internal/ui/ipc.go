package ui

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// IPCSelectMsg is sent via p.Send() when an IPC client requests session selection.
type IPCSelectMsg struct {
	// Target is the session ID or title to select/attach.
	Target string
}

// IPCServer listens on a Unix socket for external commands (e.g., session select)
// and forwards them to the running bubbletea Program via p.Send().
type IPCServer struct {
	socketPath string
	listener   net.Listener
	program    *tea.Program
	home       *Home
	mu         sync.Mutex
	closed     bool
}

// IPCSocketPath returns the deterministic socket path for the given profile.
// The socket lives at ~/.agent-deck/tui-<profile>.sock.
func IPCSocketPath(profile string) string {
	if profile == "" {
		profile = session.DefaultProfile
	}
	dir, err := session.GetAgentDeckDir()
	if err != nil {
		// Fallback to a reasonable default
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".agent-deck")
	}
	return filepath.Join(dir, fmt.Sprintf("tui-%s.sock", profile))
}

// NewIPCServer creates a new IPC server bound to the given profile's socket path.
// The server does not start listening until Start() is called.
func NewIPCServer(profile string, program *tea.Program, home *Home) *IPCServer {
	return &IPCServer{
		socketPath: IPCSocketPath(profile),
		program:    program,
		home:       home,
	}
}

// Start begins listening on the Unix socket. It removes any stale socket file
// from a previous run, creates the socket, and spawns a goroutine to accept
// connections. Returns an error if the socket cannot be created.
func (s *IPCServer) Start() error {
	// Remove stale socket from a previous crash
	_ = os.Remove(s.socketPath)

	// Ensure the parent directory exists
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0o700); err != nil {
		return fmt.Errorf("ipc: failed to create socket directory: %w", err)
	}

	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("ipc: failed to listen on %s: %w", s.socketPath, err)
	}
	s.listener = ln

	go s.acceptLoop()

	uiLog.Info("ipc_server_started", slog.String("socket", s.socketPath))
	return nil
}

// Stop closes the listener and removes the socket file.
func (s *IPCServer) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.listener != nil {
		_ = s.listener.Close()
	}
	_ = os.Remove(s.socketPath)
	uiLog.Info("ipc_server_stopped")
}

func (s *IPCServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return // Normal shutdown
			}
			uiLog.Warn("ipc_accept_error", slog.String("error", err.Error()))
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *IPCServer) handleConn(conn net.Conn) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}
	line := strings.TrimSpace(scanner.Text())
	if line == "" {
		return
	}

	// Protocol: "select <target>" where target is a session ID or title.
	if strings.HasPrefix(line, "select ") {
		target := strings.TrimSpace(strings.TrimPrefix(line, "select "))
		if target == "" {
			_, _ = fmt.Fprintln(conn, "error: empty target")
			return
		}

		isAtt := s.home.isAttaching.Load()
		tmuxName := s.home.getAttachedTmuxName()
		tmuxSocket := s.home.getAttachedTmuxSocket()
		uiLog.Info("ipc_select_state",
			slog.Bool("isAttaching", isAtt),
			slog.String("tmuxName", tmuxName),
			slog.String("tmuxSocket", tmuxSocket))

		if isAtt {
			s.forceDetachCurrent()
			go func() {
				time.Sleep(200 * time.Millisecond)
				s.program.Send(IPCSelectMsg{Target: target})
			}()
		} else {
			s.program.Send(IPCSelectMsg{Target: target})
		}

		_, _ = fmt.Fprintln(conn, "ok")
		uiLog.Info("ipc_select_received", slog.String("target", target))
		return
	}

	_, _ = fmt.Fprintf(conn, "error: unknown command: %s\n", line)
}

// forceDetachCurrent detaches the currently-attached tmux session by reading
// the attached tmux session name from the Home model and running
// `tmux detach-client -s <name>`.
func (s *IPCServer) forceDetachCurrent() {
	tmuxName := s.home.getAttachedTmuxName()
	if tmuxName == "" {
		return
	}

	// Determine socket name for the tmux command
	socketName := s.home.getAttachedTmuxSocket()

	var args []string
	if socketName != "" {
		args = append(args, "-L", socketName)
	}
	args = append(args, "detach-client", "-s", tmuxName)

	// #nosec G204 -- "tmux" is a fixed binary; args are internal.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", args...)
	if err := cmd.Run(); err != nil {
		uiLog.Warn("ipc_force_detach_failed",
			slog.String("session", tmuxName),
			slog.String("error", err.Error()))
	} else {
		uiLog.Info("ipc_force_detached", slog.String("session", tmuxName))
	}
}
