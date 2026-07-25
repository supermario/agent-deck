package web

// Voice-summary endpoints for the BentoLife app's per-session "Voice mode".
//
//   GET /api/mobile/session/{id}/voice
//       Extract the session's last assistant turn, hand it to the local voice
//       server (OpenAI summary → OmniVoice TTS), and return {summary, audioFile}.
//   GET /api/mobile/session/{id}/voice/audio?f=<key>.wav
//       Proxy the generated wav from the voice server to the phone.
//
// agent-deck is the phone's tailscale gateway and the transcript authority; the
// voice server (engines/omnivoice/voice_server.py) is localhost-only and does
// the summary + TTS. The phone reaches the audio only through this proxy.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// voiceServerBase is the local voice_server.py (summary + OmniVoice TTS).
const voiceServerBase = "http://127.0.0.1:8123"

// Summary + TTS for a fresh turn can take tens of seconds (model generation);
// give the round-trip generous headroom. Cached repeats return immediately.
var voiceHTTPClient = &http.Client{Timeout: 180 * time.Second}

// voiceFileRe guards the proxied filename against path traversal: the voice
// server names files <16-hex>.wav.
var voiceFileRe = regexp.MustCompile(`^[A-Za-z0-9_-]+\.wav$`)

// ---- GET /api/mobile/session/{id}/voice ----

func (s *Server) handleMobileVoice(w http.ResponseWriter, r *http.Request) {
	if handledPreflight(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeMobileError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	id := r.PathValue("id")
	inst, err := s.loadMobileInstance(id)
	if err != nil {
		writeMobileError(w, http.StatusInternalServerError, "failed to load session")
		return
	}
	if inst == nil {
		writeMobileError(w, http.StatusNotFound, "session not found")
		return
	}
	inst.RefreshLiveSessionIDs()

	path := inst.GetJSONLPathForInstance()
	if path == "" {
		writeMobileError(w, http.StatusNotFound, "no transcript for this session")
		return
	}
	text := lastAssistantText(cachedTranscriptTurns(path))
	if strings.TrimSpace(text) == "" {
		writeMobileError(w, http.StatusNotFound, "no assistant turn to read yet")
		return
	}

	gen, err := requestVoiceGeneration(text)
	if err != nil {
		writeMobileError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"summary":   gen.Summary,
		"voice":     gen.Voice,
		"duration":  gen.Duration,
		"audioFile": gen.AudioFile,
	})
}

// voiceGenResult is what the voice server returns for one summary+TTS request.
type voiceGenResult struct {
	Summary   string
	AudioFile string // bare "<key>.wav"; the phone/watcher build the audio URL
	Duration  float64
	Voice     string
}

// requestVoiceGeneration asks the local voice server to summarise text (OpenAI)
// and synthesise it (OmniVoice), returning the summary + generated wav filename.
// Shared by the on-demand endpoint and the turn-end watcher.
func requestVoiceGeneration(text string) (*voiceGenResult, error) {
	reqBody, _ := json.Marshal(map[string]string{"text": text})
	resp, err := voiceHTTPClient.Post(voiceServerBase+"/api/session-voice", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("voice server unavailable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("voice server error: %s", strings.TrimSpace(string(body)))
	}
	var vr struct {
		Summary  string  `json:"summary"`
		URL      string  `json:"url"` // /generated/<key>.wav
		Duration float64 `json:"duration"`
		Voice    string  `json:"voice"`
	}
	if json.Unmarshal(body, &vr) != nil || vr.URL == "" {
		return nil, fmt.Errorf("bad voice server response")
	}
	file := vr.URL
	if i := strings.LastIndex(file, "/"); i >= 0 {
		file = file[i+1:]
	}
	return &voiceGenResult{Summary: vr.Summary, AudioFile: file, Duration: vr.Duration, Voice: vr.Voice}, nil
}

// ---- GET /api/mobile/session/{id}/voice/audio?f=<key>.wav ----

func (s *Server) handleMobileVoiceAudio(w http.ResponseWriter, r *http.Request) {
	if handledPreflight(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeMobileError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	f := r.URL.Query().Get("f")
	if !voiceFileRe.MatchString(f) {
		writeMobileError(w, http.StatusBadRequest, "invalid file")
		return
	}

	resp, err := voiceHTTPClient.Get(voiceServerBase + "/generated/" + f)
	if err != nil {
		writeMobileError(w, http.StatusBadGateway, "voice server unavailable")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		writeMobileError(w, http.StatusNotFound, "audio not found")
		return
	}

	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Accept-Ranges", "bytes")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, resp.Body)
}

// lastAssistantText returns the text of the most recent non-empty assistant
// turn, i.e. the end-of-turn content to summarize.
func lastAssistantText(turns []mobileTurn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "assistant" && strings.TrimSpace(turns[i].Text) != "" {
			return turns[i].Text
		}
	}
	return ""
}
