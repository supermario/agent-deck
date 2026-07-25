package web

import (
	"os"
	"testing"
)

// TestSendVoicePush_Manual sends a REAL push to every registered device. Skipped
// unless APNS_MANUAL_TEST=1, so it never fires in normal test runs.
//
//	APNS_MANUAL_TEST=1 go test ./internal/web -run TestSendVoicePush_Manual -v
func TestSendVoicePush_Manual(t *testing.T) {
	if os.Getenv("APNS_MANUAL_TEST") == "" {
		t.Skip("set APNS_MANUAL_TEST=1 to send a real push")
	}
	cfg, err := loadAPNsConfig()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	envOr := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	n, err := sendAPNsPush(cfg,
		envOr("PUSH_TITLE", "atlas-heroku-aws"),
		envOr("PUSH_BODY", "Test voice summary from agent-deck."),
		map[string]any{
			"kind":       "voice_summary",
			"session_id": envOr("PUSH_SESSION", "test-session"),
			"audio_file": envOr("PUSH_AUDIO", "test.wav"),
			"summary":    envOr("PUSH_BODY", "Test voice summary from agent-deck."),
		})
	t.Logf("sent to %d device(s), err=%v", n, err)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
}
