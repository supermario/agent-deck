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
}

type overlayPayload struct {
	Source   string              `json:"source"`
	Sessions []overlaySession    `json:"sessions"`
	Usage    map[string]usageOut `json:"usage"`
}

// usageFile mirrors the JSON the Claude statusline writes per account to
// /tmp/claude-usage-<account>.json.
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

// readUsage loads the per-account usage files written by the statusline.
// Accounts whose data is missing or older than 6h are omitted.
func readUsage() map[string]usageOut {
	out := map[string]usageOut{}
	now := time.Now().Unix()
	for _, acct := range []string{"personal", "work"} {
		data, err := os.ReadFile("/tmp/claude-usage-" + acct + ".json")
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
		out[acct] = o
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
		if path, ok := sessionTranscriptPath(s.ProjectPath, s.ClaudeSessionID); ok {
			if ts, ok2 := lastTurnTimestamp(path); ok2 {
				lastTurn = ts
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

		sessions = append(sessions, overlaySession{
			SessionID:  s.ID,
			Tool:       tool,
			CWD:        s.ProjectPath,
			IdleSecs:   idleSecs,
			TTLSecs:    ttl,
			Summary:    s.Title,
			OpenCmd:    openCommand(s),
			IsActive:   isActive,
			NeedsInput: inputNeeded(s.ClaudeSessionID),
			ShellCount: claudeShellCount(tool, s.TmuxSocketName, s.TmuxSession),
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

// claudeShellCount reads the count of running background shells from the Claude
// footer by capturing the session's pane. Returns 0 for non-Claude tools, when
// there's no tmux session, on any capture error, or when the footer shows no
// shell segment (0 shells).
func claudeShellCount(tool, socketName, tmuxName string) int {
	// `tool` is the mapped overlay name (mapToolName): Claude Code is "claude-code".
	if tool != "claude-code" || tmuxName == "" {
		return 0
	}
	pane, err := tmux.CaptureVisibleBySocket(socketName, tmuxName)
	if err != nil {
		return 0
	}
	return parseShellCountFromPane(pane)
}

// parseShellCountFromPane reads the shell count from ONLY the footer status line
// — the last non-empty line of the captured pane. Scanning the whole pane would
// false-match transcript text that happens to contain "· N shells" (a session
// discussing shells, command output, or the agent's own quoted footer format).
func parseShellCountFromPane(pane string) int {
	lines := strings.Split(pane, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		// First non-empty line from the bottom IS the footer. Parse it and stop
		// — never fall through into the transcript above.
		if m := shellCountRe.FindStringSubmatch(line); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n
			}
		}
		return 0
	}
	return 0
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
		fmt.Fprintf(f, "%s %-6s %-30s idle=%ds sh=%d%s\n", ts, active, s.Summary, s.IdleSecs, s.ShellCount, input)
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

