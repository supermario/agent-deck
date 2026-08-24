package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/logging"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

type overlaySession struct {
	SessionID  string `json:"session_id"`
	Tool       string `json:"tool"`
	CWD        string `json:"cwd"`
	IdleSecs   int    `json:"idle_seconds"`
	TTLSecs    int    `json:"ttl_seconds"`
	Summary    string `json:"summary"`
	OpenCmd    string `json:"open_command"`
	IsActive   bool   `json:"is_active"`
	NeedsInput bool   `json:"needs_input"`
	ShellCount int    `json:"shell_count,omitempty"`
	AgentCount int    `json:"agent_count,omitempty"`
	// Global attention rank, 1 = highest, 0 = unranked. Drives both the
	// overlay's display order and the next-session hotkey's pick order.
	Priority int `json:"priority,omitempty"`
	// GitHub PRs mentioned in this session's transcript, first-seen order.
	// Rendered as clickable #NNNN chips so a session links back to its PRs.
	PRs []prRef `json:"prs,omitempty"`
}

type overlayPayload struct {
	Source   string              `json:"source"`
	Sessions []overlaySession    `json:"sessions"`
	Usage    map[string]usageOut `json:"usage"`
}

// usageFile is the normalised JSON written by the Claude and Codex usage
// pollers. Both providers expose rolling quota windows, so the overlay does not
// need to know which account API produced a row.
type usageWindow struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at"`
}
type usageFile struct {
	Account    string       `json:"account"`
	FiveHour   *usageWindow `json:"five_hour"`
	SevenDay   usageWindow  `json:"seven_day"`
	CapturedAt int64        `json:"captured_at"`
}

// usageOut is the flattened shape the overlay consumes. five_hour is omitted
// for accounts without a rolling 5-hour cap (e.g. team accounts).
type usageOut struct {
	FiveHourPct      *float64 `json:"five_hour_pct,omitempty"`
	FiveHourResetsAt *int64   `json:"five_hour_resets_at,omitempty"`
	SevenDayPct      float64  `json:"seven_day_pct"`
	SevenDayResetsAt int64    `json:"seven_day_resets_at"`
	CapturedAt       int64    `json:"captured_at"`
}

var overlayUsageDir = "/tmp"

// readUsage loads the per-account usage files written by the local pollers.
// Accounts whose data is missing or older than 6h are omitted.
func readUsage() map[string]usageOut {
	out := map[string]usageOut{}
	now := time.Now().Unix()
	for _, source := range []struct {
		account  string
		filename string
	}{
		{account: "personal", filename: "claude-usage-personal.json"},
		{account: "work", filename: "claude-usage-work.json"},
		{account: "codex", filename: "codex-usage.json"},
	} {
		data, err := os.ReadFile(filepath.Join(overlayUsageDir, source.filename))
		if err != nil {
			continue
		}
		var uf usageFile
		if json.Unmarshal(data, &uf) != nil {
			continue
		}
		if uf.CapturedAt > 0 && now-uf.CapturedAt > 6*3600 {
			continue
		}
		o := usageOut{
			SevenDayPct:      uf.SevenDay.UsedPercentage,
			SevenDayResetsAt: uf.SevenDay.ResetsAt,
			CapturedAt:       uf.CapturedAt,
		}
		if uf.FiveHour != nil {
			pct := uf.FiveHour.UsedPercentage
			reset := uf.FiveHour.ResetsAt
			o.FiveHourPct = &pct
			o.FiveHourResetsAt = &reset
		}
		out[source.account] = o
	}
	// Always return a non-nil map so the client can clear stale usage
	// (the field is always present on the agent-deck push).
	return out
}

type overlayPusher struct {
	menuData   MenuDataLoader
	endpoint   string
	client     *http.Client
	mu         sync.Mutex
	lastHash   [32]byte
	lastStatus map[string]bool
	idleSince  map[string]time.Time
	// Incremental PR-scan position per session. Guarded by mu, like the maps
	// above: push() holds it across the whole scan loop.
	prCache map[string]*prScanState
}

func newOverlayPusher(menuData MenuDataLoader) *overlayPusher {
	return &overlayPusher{
		menuData:   menuData,
		endpoint:   "http://localhost:27015/push",
		lastStatus: make(map[string]bool),
		idleSince:  make(map[string]time.Time),
		client: &http.Client{
			Timeout: 2 * time.Second,
		},
	}
}

func (o *overlayPusher) push(ctx context.Context) {
	snapshot, err := o.menuData.LoadMenuSnapshot()
	if err != nil {
		overlayLog.Debug("overlay_snapshot_failed", slog.String("error", err.Error()))
		return
	}

	priorities := sessionPriorities()

	now := time.Now()
	var sessions []overlaySession
	// Serialize the whole scan: it reads and writes o.idleSince / o.lastStatus,
	// and triggerAsync fires push() in a fresh goroutine each trigger. Two
	// overlapping pushes writing these maps crash the process with a fatal
	// "concurrent map writes". Pushes are infrequent, so holding the lock
	// across the loop (which also does light transcript IO) is cheap.
	o.mu.Lock()
	for _, item := range snapshot.Items {
		if item.Type != MenuItemTypeSession || item.Session == nil {
			continue
		}
		s := item.Session
		tool := mapToolName(s.Tool)
		if tool == "" {
			continue
		}

		status := strings.ToLower(string(s.Status))
		if status != "running" && status != "waiting" && status != "idle" {
			continue
		}

		statusRunning := status == "running"

		// The overlay tracks the LLM prompt-cache TTL, which is anchored to
		// the last turn exchange — NOT to whether a process is alive.
		// agent-deck marks a session "running" whenever a background shell is
		// still up (e.g. a dev server launched via run_in_background), which
		// would wrongly keep the session spinning forever and never start the
		// cache countdown. Anchor to the last genuine turn recorded in this
		// session's own transcript (by ClaudeSessionID, not the newest file in
		// the cwd), read from the record timestamp rather than the file mtime —
		// mtime gets bumped by session-resume/housekeeping without a new turn,
		// which made idle sessions look recently active (see handlers_mobile.go).
		var lastTurn time.Time
		var prs []prRef
		if path, ok := sessionTranscriptPath(s.ProjectPath, s.ClaudeSessionID); ok {
			if ts, ok2 := lastTurnTimestamp(path); ok2 {
				lastTurn = ts
			}
			// Same transcript, scanned incrementally - see scanPRs. Excluded
			// sessions are skipped before the scan, so they cost no file I/O.
			if !prSessionExcluded(s.Title) {
				prs = o.scanPRs(s.ID, path)
			}
		}

		var isActive bool
		var idleSecs int
		if !lastTurn.IsZero() {
			since := now.Sub(lastTurn)
			// Only "active" (spinner) while the LLM is genuinely mid-turn:
			// agent-deck sees a spinner AND the transcript is still being
			// written. A stale transcript under a "running" status is the
			// background-shell case → fall through to the countdown.
			isActive = statusRunning && since < liveTurnWindow
			if isActive {
				delete(o.idleSince, s.ID)
			} else {
				o.idleSince[s.ID] = lastTurn
				idleSecs = int(since.Seconds())
			}
		} else {
			// No transcript located (non-Claude tool, or unusual path): fall
			// back to the prior status-transition heuristic.
			isActive = statusRunning
			wasActive := o.lastStatus[s.ID]
			if isActive {
				delete(o.idleSince, s.ID)
			} else if wasActive {
				o.idleSince[s.ID] = now
			} else if o.idleSince[s.ID].IsZero() {
				if !s.LastAccessedAt.IsZero() {
					o.idleSince[s.ID] = s.LastAccessedAt
				} else {
					o.idleSince[s.ID] = now
				}
			}
			if !isActive {
				if since, ok := o.idleSince[s.ID]; ok {
					idleSecs = int(now.Sub(since).Seconds())
				}
			}
		}
		o.lastStatus[s.ID] = isActive

		ttl := cacheTTL(tool)
		if !isActive && idleSecs > ttl+3600 {
			continue
		}

		shellCount, agentCount := claudePaneCounts(tool, s.TmuxSocketName, s.TmuxSession)

		sessions = append(sessions, overlaySession{
			Priority:   priorities[s.ID],
			PRs:        prs,
			SessionID:  s.ID,
			Tool:       tool,
			CWD:        s.ProjectPath,
			IdleSecs:   idleSecs,
			TTLSecs:    ttl,
			Summary:    s.Title,
			OpenCmd:    openCommand(s),
			IsActive:   isActive,
			NeedsInput: inputNeeded(s.ClaudeSessionID),
			ShellCount: shellCount,
			AgentCount: agentCount,
		})
	}
	o.mu.Unlock()

	payload := overlayPayload{Source: "agent-deck", Sessions: sessions, Usage: readUsage()}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}

	hash := sha256.Sum256(body)
	o.mu.Lock()
	changed := hash != o.lastHash
	o.lastHash = hash
	o.mu.Unlock()

	if changed {
		o.logDebug(sessions)
	}

	if !changed {
		return
	}

	req, err := http.NewRequestWithContext(ctx, "POST", o.endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		overlayLog.Debug("overlay_push_failed", slog.String("error", err.Error()))
		return
	}
	resp.Body.Close()
	overlayLog.Debug("overlay_push_ok", slog.Int("sessions", len(sessions)))
}

func mapToolName(tool string) string {
	switch strings.ToLower(tool) {
	case "claude":
		return "claude-code"
	case "windsurf":
		return "windsurf"
	case "gemini":
		return "gemini"
	case "codex":
		return "codex"
	default:
		return ""
	}
}

func cacheTTL(tool string) int {
	switch tool {
	case "windsurf":
		return 300
	default:
		return 3600
	}
}

// liveTurnWindow is how recently the Claude transcript must have been written
// for a "running" session to still count as a genuinely live turn (spinner)
// rather than a background-shell false-active. Comfortably longer than the
// gaps between transcript writes during a normal turn (streamed messages,
// tool calls), but short enough that an idle session with a lingering
// background process settles to the cache countdown.
const liveTurnWindow = 120 * time.Second

func openCommand(s *MenuSession) string {
	if s.ClaudeSessionID != "" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".claude/hooks/focus-claude-session") + " " + s.ClaudeSessionID
	}
	return ""
}

// inputNeeded reports whether Claude is blocked on the user mid-turn for this
// session. Claude's PreToolUse(AskUserQuestion) hook drops a marker file keyed
// by Claude session_id; PostToolUse removes it. A stale guard ignores markers
// older than 1h in case a clear was missed (session killed mid-question).
// shellCountRe matches the running-shell segment of the Claude footer status
// line, e.g. "· 2 shells ·" or "· 1 shell ·".
var shellCountRe = regexp.MustCompile(`·\s*(\d+)\s+shells?\b`)

// Claude renders a thread roster underneath the footer whenever subagents are
// live: one "⏺ main" row for the main thread, then one row per agent, e.g.
//
//	⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents
//	⏺ main
//	◯ general-purpose  Verifying IOG cache narinfo hits   36m 53s · ↓ 177.2k tokens
//
// The bullets are distinct codepoints — U+23FA for main, U+25EF for an agent —
// which is what makes this safe to match on.
var (
	agentRowRe = regexp.MustCompile(`^◯\s+\S`)
	mainRowRe  = regexp.MustCompile(`^⏺\s+main\b`)
)

// claudePaneCounts reads the running background shells and running subagents for
// a session from one pane capture. Returns zeroes for non-Claude tools, when
// there's no tmux session, or on any capture error.
//
// Both come from the same capture deliberately: this runs per session on every
// overlay push, and a second capture would double the tmux round-trips for a
// number we already have on screen.
func claudePaneCounts(tool, socketName, tmuxName string) (shells, agents int) {
	// `tool` is the mapped overlay name (mapToolName): Claude Code is "claude-code".
	if tool != "claude-code" || tmuxName == "" {
		return 0, 0
	}
	pane, err := tmux.CaptureVisibleBySocket(socketName, tmuxName)
	if err != nil {
		return 0, 0
	}
	return parseShellCountFromPane(pane), parseAgentCountFromPane(pane)
}

// parseShellCountFromPane reads the shell count from ONLY the footer status line.
// Scanning the whole pane would false-match transcript text that happens to
// contain "· N shells" (a session discussing shells, command output, or the
// agent's own quoted footer format).
//
// The footer is the last non-empty line EXCEPT when subagents are running, which
// pushes a thread roster below it. Skipping those rows is what keeps the shell
// badge working on a session that has both shells and agents.
func parseShellCountFromPane(pane string) int {
	lines := strings.Split(pane, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || agentRowRe.MatchString(line) || mainRowRe.MatchString(line) {
			continue
		}
		// First non-roster, non-empty line from the bottom IS the footer. Parse
		// it and stop — never fall through into the transcript above.
		if m := shellCountRe.FindStringSubmatch(line); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n
			}
		}
		return 0
	}
	return 0
}

// parseAgentCountFromPane counts the running subagents in the roster block at
// the very bottom of the pane.
//
// Bounded the same way the shell parse is: walk up from the last line and stop
// at the first line that is not part of the roster, so transcript text can never
// leak in — a session whose conversation happens to quote a roster row is only a
// match if that text is the last thing on screen, which the footer prevents.
func parseAgentCountFromPane(pane string) int {
	lines := strings.Split(pane, "\n")
	count := 0
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		switch {
		case line == "":
			continue
		case agentRowRe.MatchString(line):
			count++
		case mainRowRe.MatchString(line):
			// The main thread is not an agent, but it is part of the roster, so
			// keep walking past it.
			continue
		default:
			return count
		}
	}
	return count
}

func inputNeeded(claudeSessionID string) bool {
	if claudeSessionID == "" {
		return false
	}
	info, err := os.Stat(filepath.Join("/tmp/claude-input-needed", claudeSessionID))
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < time.Hour
}

var overlayLog = logging.ForComponent(logging.CompWeb)

func (o *overlayPusher) logDebug(sessions []overlaySession) {
	f, err := os.OpenFile("/tmp/agent-deck-overlay.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	ts := time.Now().Format("15:04:05")
	for _, s := range sessions {
		active := "idle"
		if s.IsActive {
			active = "ACTIVE"
		}
		input := ""
		if s.NeedsInput {
			input = " INPUT-NEEDED"
		}
		prs := ""
		if len(s.PRs) > 0 {
			nums := make([]string, 0, len(s.PRs))
			for _, p := range s.PRs {
				nums = append(nums, "#"+strconv.Itoa(p.Number))
			}
			prs = " prs=" + strings.Join(nums, ",")
		}
		fmt.Fprintf(f, "%s %-6s %-30s idle=%ds sh=%d ag=%d pri=%d%s%s\n",
			ts, active, s.Summary, s.IdleSecs, s.ShellCount, s.AgentCount, s.Priority, input, prs)
	}
	fmt.Fprintf(f, "%s --- pushed %d sessions ---\n", ts, len(sessions))
}

func (o *overlayPusher) triggerAsync(ctx context.Context) {
	go func() {
		pushCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		o.push(pushCtx)
	}()
}

// resetHash forces the next push to send regardless of content.
func (o *overlayPusher) resetHash() {
	o.mu.Lock()
	o.lastHash = [32]byte{}
	o.mu.Unlock()
}

// startPeriodicPush runs a background goroutine that pushes every interval.
// This ensures countdown idle_seconds stay reasonably fresh even without
// status transitions. Stops when ctx is cancelled.
func (o *overlayPusher) startPeriodicPush(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				o.resetHash()
				o.push(ctx)
			}
		}
	}()
}

// StartOverlayPusher launches a standalone overlay pusher that polls session
// status and pushes to DotfilesBar. Runs in background goroutines, returns
// immediately. Cancel ctx to stop.
func StartOverlayPusher(ctx context.Context, profile string) {
	menuData := NewSessionDataService(profile)
	pusher := newOverlayPusher(menuData)
	pusher.startPeriodicPush(ctx, 5*time.Second)
}
