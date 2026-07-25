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
	n, err := sendAPNsPush(cfg,
		"atlas-heroku-aws",
		"Test voice summary from agent-deck — if you see this, push + custom payload delivery works.",
		map[string]any{
			"kind":       "voice_summary",
			"session_id": "test-session",
			"audio_file": "test.wav",
		})
	t.Logf("sent to %d device(s), err=%v", n, err)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
}
