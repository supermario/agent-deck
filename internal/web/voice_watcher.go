package web

// Turn-end voice pipeline: watches every Claude session and, when one hands back
// to the user with a NEW final turn, generates a spoken summary (OpenAI +
// OmniVoice) and sends an APNs push whose body is the summary and whose data
// deep-links the phone to that session and auto-plays the pre-generated audio.
//
// Global for now ("across the board" testing). TODO: gate on the laptop being
// clamshelled (dotfiles/scripts/clamstatus.sh) = on-the-go, so summaries are
// only spoken when the user is actually away from the desk.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/logging"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

var voiceLog = logging.ForComponent(logging.CompWeb)

// voiceAutoEnabled reports whether the turn-end pipeline runs. On by default;
// set AGENTDECK_VOICE_AUTO=0 to disable.
func voiceAutoEnabled() bool {
	return os.Getenv("AGENTDECK_VOICE_AUTO") != "0"
}

// startVoiceWatcher launches the background poller. Cancel ctx to stop.
func (s *Server) startVoiceWatcher(ctx context.Context) {
	go func() {
		seen := map[string]string{} // sessionID -> last handled turn key
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if voiceAutoEnabled() {
				s.voiceWatchTick(seen)
			}
		}
	}()
	voiceLog.Info("voice_watcher_started")
}

func (s *Server) voiceWatchTick(seen map[string]string) {
	snapshot, err := s.menuData.LoadMenuSnapshot()
	if err != nil {
		return
	}
	for _, item := range snapshot.Items {
		if item.Type != MenuItemTypeSession || item.Session == nil {
			continue
		}
		se := item.Session
		if !session.IsClaudeCompatible(se.Tool) {
			continue
		}
		// Fire only on a genuine hand-back: waiting (needs input) or idle
		// (acknowledged). Skip running (mid-turn) and error/dead.
		status := strings.ToLower(string(se.Status))
		if status != "waiting" && status != "idle" {
			continue
		}
		path, ok := sessionTranscriptPath(se.ProjectPath, se.ClaudeSessionID)
		if !ok {
			continue
		}
		text := lastAssistantText(cachedTranscriptTurns(path))
		if strings.TrimSpace(text) == "" {
			continue
		}
		sum := sha256.Sum256([]byte(se.ClaudeSessionID + ":" + text))
		key := hex.EncodeToString(sum[:8])

		prev, known := seen[se.ID]
		if !known {
			// First observation: seed without firing, so the watcher doesn't
			// blast a push for every already-idle session on startup.
			seen[se.ID] = key
			continue
		}
		if prev == key {
			continue
		}
		seen[se.ID] = key
		go s.deliverVoiceSummary(se.ID, se.Title, text)
	}
}

// deliverVoiceSummary generates the summary+audio for a completed turn and
// pushes it. Runs in its own goroutine (generation takes seconds).
func (s *Server) deliverVoiceSummary(sessionID, title, text string) {
	gen, err := requestVoiceGeneration(text)
	if err != nil {
		voiceLog.Warn("voice_generate_failed", slog.String("session", sessionID), slog.String("error", err.Error()))
		return
	}
	cfg, err := loadAPNsConfig()
	if err != nil {
		voiceLog.Warn("voice_apns_config_failed", slog.String("error", err.Error()))
		return
	}
	if strings.TrimSpace(title) == "" {
		title = "Claude"
	}
	n, err := sendAPNsPush(cfg, title, gen.Summary, map[string]any{
		"kind":       "voice_summary",
		"session_id": sessionID,
		"audio_file": gen.AudioFile,
	})
	if err != nil {
		voiceLog.Warn("voice_push_failed", slog.String("session", sessionID), slog.String("error", err.Error()))
		return
	}
	voiceLog.Info("voice_push_sent",
		slog.String("session", sessionID),
		slog.Int("devices", n),
		slog.String("audio", gen.AudioFile))
}
