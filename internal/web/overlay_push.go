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
	"strings"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/logging"
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

		isActive := status == "running"
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
		o.lastStatus[s.ID] = isActive

		idleSecs := 0
		if !isActive {
			if since, ok := o.idleSince[s.ID]; ok {
				idleSecs = int(now.Sub(since).Seconds())
			}
		}

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
		})
	}

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
		fmt.Fprintf(f, "%s %-6s %-30s idle=%ds%s\n", ts, active, s.Summary, s.IdleSecs, input)
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

