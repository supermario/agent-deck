package web

// In-memory cache of the latest voice summary generated per session, so the
// phone can fetch a ready summary (from the turn-end watcher or a prior manual
// read) instead of paying for a fresh OpenAI + TTS generation when it navigates
// to a session after missing the push. Keyed by the turn it was generated for,
// so a stale summary from a superseded turn is never served.

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

type voiceLatestEntry struct {
	Summary   string
	AudioFile string
	TurnKey   string // voiceTurnKey of the assistant turn it summarises
}

var (
	voiceLatestMu sync.Mutex
	voiceLatest   = map[string]voiceLatestEntry{}
)

// voiceTurnKey identifies a specific assistant turn (session + final text), so
// the watcher and /voice/latest agree on whether a cached summary is current.
func voiceTurnKey(claudeSessionID, text string) string {
	sum := sha256.Sum256([]byte(claudeSessionID + ":" + text))
	return hex.EncodeToString(sum[:8])
}

func storeVoiceLatest(sessionID, summary, audioFile, turnKey string) {
	voiceLatestMu.Lock()
	defer voiceLatestMu.Unlock()
	voiceLatest[sessionID] = voiceLatestEntry{Summary: summary, AudioFile: audioFile, TurnKey: turnKey}
}

func getVoiceLatest(sessionID string) (voiceLatestEntry, bool) {
	voiceLatestMu.Lock()
	defer voiceLatestMu.Unlock()
	e, ok := voiceLatest[sessionID]
	return e, ok
}
