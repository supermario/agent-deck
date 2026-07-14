package web

// Mobile API: a small, CORS-enabled, CSRF-exempt surface under /api/mobile/
// that powers the native BentoLife phone client (a structured-chat view over
// Tailscale). It reuses the existing session data service, Claude JSONL
// resolution, and tmux send primitives — it does not introduce a new data
// model.
//
// These endpoints are intentionally token-less and same-origin-agnostic: the
// deployment is a loopback bind fronted by `tailscale serve`, reachable only on
// the tailnet. The phone app loads from a file:// origin, so permissive CORS +
// a CSRF exemption (see csrf.go) are required for it to call these at all.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/send"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

// ---- wire types (field names are consumed verbatim by the Elm decoders) ----

type mobileSession struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Group    string `json:"group"`
	Tool     string `json:"tool"`
	Status   string `json:"status"`
	Substate string `json:"substate"`
	// Prompt-cache countdown inputs, mirroring the DotfilesBar overlay
	// (see overlay_push.go). The phone's "Recent" tab sorts by IdleSeconds
	// and renders a local M:SS countdown of (TTLSeconds - IdleSeconds).
	IdleSeconds int  `json:"idleSeconds"`
	TTLSeconds  int  `json:"ttlSeconds"`
	IsActive    bool `json:"isActive"`
	NeedsInput  bool `json:"needsInput"`
}

type mobileSessionsResponse struct {
	Sessions []mobileSession `json:"sessions"`
}

type mobileToolRef struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

type mobileTurn struct {
	Role  string          `json:"role"`
	Text  string          `json:"text"`
	TS    string          `json:"ts"`
	TsMs  int64           `json:"tsMs"` // last-record unix millis; 0 if unparseable
	Tools []mobileToolRef `json:"tools"`
}

type mobileTranscriptResponse struct {
	ID     string       `json:"id"`
	Title  string       `json:"title"`
	Status string       `json:"status"`
	Turns  []mobileTurn `json:"turns"` // when afterIndex is given, only turns after it
	Total  int          `json:"total"` // total turn count on the server (for incremental sync)
}

// ---- shared helpers ----

func mobileCORS(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type")
}

// handledPreflight writes CORS headers and, for an OPTIONS preflight, a 204 and
// returns true so the caller can stop. Every mobile handler calls this first.
func handledPreflight(w http.ResponseWriter, r *http.Request) bool {
	mobileCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func writeMobileError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": message})
}

// loadMobileInstance loads the session registry for the server's profile and
// returns the instance with the given id (nil, nil if not found). Tmux sessions
// are lazily reconnected by LoadWithGroups, so Exists()/GetTmuxSession() work on
// the returned instance.
func (s *Server) loadMobileInstance(id string) (*session.Instance, error) {
	profile := session.GetEffectiveProfile(s.cfg.Profile)
	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = storage.Close() }()

	instances, _, err := storage.LoadWithGroups()
	if err != nil {
		return nil, err
	}
	for _, inst := range instances {
		if inst.ID == id {
			return inst, nil
		}
	}
	return nil, nil
}

// ---- GET /api/mobile/sessions ----

func (s *Server) handleMobileSessions(w http.ResponseWriter, r *http.Request) {
	if handledPreflight(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeMobileError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	snapshot, err := s.menuData.LoadMenuSnapshot()
	if err != nil {
		writeMobileError(w, http.StatusInternalServerError, "failed to load sessions")
		return
	}
	refreshSnapshotHookStatuses(snapshot, s.hookStatusLoader)

	now := time.Now()
	out := mobileSessionsResponse{Sessions: []mobileSession{}}
	for _, item := range snapshot.Items {
		if item.Session == nil {
			continue
		}
		se := item.Session
		idle, ttl, active, needsInput := mobileSessionActivity(se, now)
		out.Sessions = append(out.Sessions, mobileSession{
			ID:          se.ID,
			Title:       se.Title,
			Group:       se.GroupPath,
			Tool:        se.Tool,
			Status:      string(se.Status),
			Substate:    se.Substate,
			IdleSeconds: idle,
			TTLSeconds:  ttl,
			IsActive:    active,
			NeedsInput:  needsInput,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// mobileSessionActivity derives the prompt-cache countdown inputs for a session:
// idleSeconds (since the last turn exchange), ttlSeconds (the cache TTL for the
// tool), isActive (genuinely mid-turn), and needsInput.
//
// Unlike the DotfilesBar overlay's lastTurnAt (which takes the newest .jsonl in
// the working directory), this keys off THIS session's own Claude transcript,
// identified by its ClaudeSessionID. That distinction matters on the phone,
// which lists every session: many agent-deck sessions share one working
// directory (e.g. several sessions all opened in ~/dev/projects/bento-life), and
// "newest file in the cwd" would collapse them all to the same activity time.
//
// When no per-session transcript can be located (non-Claude tool, never ran, or
// a rolled session-id whose file is gone) idleSeconds is a large sentinel and
// ttlSeconds is 0, so the session sorts to the bottom of a most-recent-first
// list and shows no countdown.
func mobileSessionActivity(s *MenuSession, now time.Time) (idleSeconds, ttlSeconds int, isActive, needsInput bool) {
	ttlSeconds = cacheTTL(mapToolName(s.Tool))
	needsInput = inputNeeded(s.ClaudeSessionID)
	statusRunning := strings.ToLower(string(s.Status)) == "running"

	if path, ok := sessionTranscriptPath(s.ProjectPath, s.ClaudeSessionID); ok {
		if lastTurn, ok2 := lastTurnTimestamp(path); ok2 {
			since := now.Sub(lastTurn)
			isActive = statusRunning && since < liveTurnWindow
			if !isActive {
				idleSeconds = int(since.Seconds())
			}
			return
		}
	}

	// No per-session transcript located: a running session counts as active;
	// otherwise fall back to last-accessed time, or mark it unknown.
	if statusRunning {
		isActive = true
		return
	}
	if !s.LastAccessedAt.IsZero() {
		idleSeconds = int(now.Sub(s.LastAccessedAt).Seconds())
		return
	}
	idleSeconds = 1 << 30
	ttlSeconds = 0
	return
}

// sessionTranscriptPath resolves a specific session's Claude transcript file,
// <config>/projects/<cwd-slug>/<sessionID>.jsonl, checking the personal
// (~/.claude) and work (~/.claude-work) config dirs and returning the newest
// existing match. Keying by sessionID - rather than globbing the cwd - is what
// lets sessions sharing a working directory resolve to distinct files.
func sessionTranscriptPath(cwd, claudeSessionID string) (string, bool) {
	if cwd == "" || claudeSessionID == "" {
		return "", false
	}
	resolved := cwd
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		resolved = r
	}
	dirName := session.ConvertToClaudeDirName(resolved)
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	var newestPath string
	var newestMod time.Time
	for _, cfg := range []string{".claude", ".claude-work"} {
		f := filepath.Join(home, cfg, "projects", dirName, claudeSessionID+".jsonl")
		if fi, err := os.Stat(f); err == nil && (newestPath == "" || fi.ModTime().After(newestMod)) {
			newestPath = f
			newestMod = fi.ModTime()
		}
	}
	return newestPath, newestPath != ""
}

// lastTurnTimestamp returns the timestamp of the last genuine user/assistant
// message record in a Claude transcript. This is the true "last turn ended"
// time - unlike the file mtime, which gets bumped when the file is re-touched
// (session resume, housekeeping) without any new turn, making an old idle
// session look recently active. Reads only the file's tail for efficiency.
func lastTurnTimestamp(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return time.Time{}, false
	}
	const tailBytes int64 = 1 << 20 // 1MB tail: comfortably covers the last several turns
	if fi.Size() > tailBytes {
		if _, err := f.Seek(fi.Size()-tailBytes, io.SeekStart); err != nil {
			return time.Time{}, false
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return time.Time{}, false
	}

	lines := bytes.Split(data, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var hdr struct {
			Type        string `json:"type"`
			Timestamp   string `json:"timestamp"`
			IsSidechain bool   `json:"isSidechain"`
			Message     struct {
				Role string `json:"role"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &hdr) != nil || hdr.IsSidechain || hdr.Timestamp == "" {
			continue
		}
		role := hdr.Message.Role
		if role == "" {
			role = hdr.Type
		}
		if role != "user" && role != "assistant" {
			continue
		}
		if ts, err := time.Parse(time.RFC3339Nano, hdr.Timestamp); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

// ---- GET /api/mobile/session/{id}/transcript ----

func (s *Server) handleMobileTranscript(w http.ResponseWriter, r *http.Request) {
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

	// Pull the freshest Claude session UUID from tmux (it rolls after /clear or
	// compaction) so we resolve the currently-active transcript file.
	inst.RefreshLiveSessionIDs()

	// afterIndex lets the phone fetch only turns newer than what it already has,
	// so watching a live conversation transfers a few KB instead of the whole
	// transcript each poll. total is always the full count so the client can
	// detect a reset (session /clear-roll) when total < its own count.
	afterIndex := 0
	if v := r.URL.Query().Get("afterIndex"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n >= 0 {
			afterIndex = n
		}
	}

	resp := mobileTranscriptResponse{
		ID:     inst.ID,
		Title:  inst.Title,
		Status: string(inst.Status),
		Turns:  []mobileTurn{},
	}

	path := inst.GetJSONLPath()
	if path == "" {
		path = latestTranscriptOnDisk(inst)
	}
	if path != "" {
		all := cachedTranscriptTurns(path)
		resp.Total = len(all)
		if afterIndex > 0 {
			if afterIndex <= len(all) {
				resp.Turns = all[afterIndex:]
			}
			// afterIndex > len(all): the client is ahead (roll); leave Turns
			// empty - it will full-refetch on seeing total < its count.
		} else {
			resp.Turns = all
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// latestTranscriptOnDisk is the fallback when the stored UUID doesn't resolve to
// a file: pick the newest .jsonl in the session's Claude project directory.
func latestTranscriptOnDisk(inst *session.Instance) string {
	if !session.IsClaudeCompatible(inst.Tool) {
		return ""
	}
	configDir := session.GetClaudeConfigDirForInstance(inst)
	resolved := inst.ProjectPath
	if r, err := filepath.EvalSymlinks(inst.ProjectPath); err == nil {
		resolved = r
	}
	dir := filepath.Join(configDir, "projects", session.ConvertToClaudeDirName(resolved))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var newest string
	var newestMod time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestMod) {
			newest = filepath.Join(dir, e.Name())
			newestMod = info.ModTime()
		}
	}
	return newest
}

// transcriptCache memoizes parsed turns per file so re-fetches (polling a live
// conversation) don't re-parse the whole JSONL. Claude appends whole lines to a
// given session file and never rewrites it, so we cache the parsed turns plus
// the byte offset of the last complete line and, on the next read, parse only
// the appended bytes. A rolled session resolves to a different path (fresh
// cache entry); a truncated file (size < offset) triggers a full re-parse.
type transcriptCacheEntry struct {
	offset int64
	turns  []mobileTurn
}

var (
	transcriptCacheMu sync.Mutex
	transcriptCache   = map[string]*transcriptCacheEntry{}
)

func cachedTranscriptTurns(path string) []mobileTurn {
	transcriptCacheMu.Lock()
	defer transcriptCacheMu.Unlock()

	fi, err := os.Stat(path)
	if err != nil {
		turns, _ := parseTranscriptFrom(path, 0)
		return turns
	}

	entry := transcriptCache[path]
	if entry == nil || fi.Size() < entry.offset {
		turns, off := parseTranscriptFrom(path, 0)
		transcriptCache[path] = &transcriptCacheEntry{offset: off, turns: turns}
		return turns
	}
	if fi.Size() > entry.offset {
		newTurns, off := parseTranscriptFrom(path, entry.offset)
		if len(newTurns) > 0 {
			entry.turns = append(entry.turns, newTurns...)
		}
		entry.offset = off
	}
	return entry.turns
}

// parseTranscriptFrom reads a Claude JSONL transcript starting at byte offset
// start, flattening genuine user/assistant records into chat turns. It only
// consumes complete lines (ending in '\n'); the returned offset is the position
// after the last complete line, so a partially-written trailing record is
// re-read next time rather than parsed truncated.
func parseTranscriptFrom(path string, start int64) ([]mobileTurn, int64) {
	turns := []mobileTurn{}

	f, err := os.Open(path)
	if err != nil {
		return turns, start
	}
	defer func() { _ = f.Close() }()

	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return turns, start
		}
	}

	r := bufio.NewReaderSize(f, 1024*1024)
	offset := start
	for {
		line, err := r.ReadBytes('\n')
		if err == nil {
			// Complete line (delimiter included).
			offset += int64(len(line))
			if turn, ok := parseTurnLine(line); ok {
				turns = append(turns, turn)
			}
			continue
		}
		// err != nil: ReadBytes returned the trailing bytes without a delimiter
		// (partial last record) - leave the offset before it and stop.
		break
	}
	return turns, offset
}

// parseTurnLine parses one JSONL record into a chat turn. ok is false for
// records that aren't displayed turns (sidechain, tool_result-only user records,
// Claude Code plumbing, empty assistant records).
func parseTurnLine(line []byte) (mobileTurn, bool) {
	if len(strings.TrimSpace(string(line))) == 0 {
		return mobileTurn{}, false
	}
	var hdr struct {
		Type        string          `json:"type"`
		Timestamp   string          `json:"timestamp"`
		IsSidechain bool            `json:"isSidechain"`
		Message     json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(line, &hdr); err != nil {
		return mobileTurn{}, false
	}
	if hdr.IsSidechain || len(hdr.Message) == 0 {
		return mobileTurn{}, false
	}
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(hdr.Message, &msg); err != nil {
		return mobileTurn{}, false
	}
	role := msg.Role
	if role == "" {
		role = hdr.Type
	}

	tsMs := parseTsMs(hdr.Timestamp)

	switch role {
	case "assistant":
		text, tools := extractAssistantContent(msg.Content)
		if text != "" || len(tools) > 0 {
			return mobileTurn{Role: "assistant", Text: text, TS: hdr.Timestamp, TsMs: tsMs, Tools: tools}, true
		}
	case "user":
		text := extractUserText(msg.Content)
		if strings.TrimSpace(text) != "" && !isNoiseUserText(text) {
			return mobileTurn{Role: "user", Text: text, TS: hdr.Timestamp, TsMs: tsMs, Tools: []mobileToolRef{}}, true
		}
	}
	return mobileTurn{}, false
}

// parseTsMs converts an RFC3339 record timestamp to unix milliseconds, or 0 if
// it can't be parsed (so the phone can just skip the "… ago" label).
func parseTsMs(ts string) int64 {
	if ts == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t.UnixMilli()
	}
	return 0
}

// extractAssistantContent pulls concatenated text and tool_use references out of
// an assistant message's content (which is either a bare string or an array of
// typed blocks).
func extractAssistantContent(content json.RawMessage) (string, []mobileToolRef) {
	tools := []mobileToolRef{}

	var str string
	if err := json.Unmarshal(content, &str); err == nil {
		return str, tools
	}

	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return "", tools
	}

	var sb strings.Builder
	for _, blk := range blocks {
		var btype string
		_ = json.Unmarshal(blk["type"], &btype)
		switch btype {
		case "text":
			var txt string
			_ = json.Unmarshal(blk["text"], &txt)
			appendLine(&sb, txt)
		case "tool_use":
			var name string
			_ = json.Unmarshal(blk["name"], &name)
			tools = append(tools, mobileToolRef{
				Name:    name,
				Summary: toolSummary(blk["input"]),
			})
		}
	}
	return sb.String(), tools
}

// extractUserText returns the human-authored text of a user message, or "" if
// the record carries only tool_result blocks (not a chat turn).
func extractUserText(content json.RawMessage) string {
	var str string
	if err := json.Unmarshal(content, &str); err == nil {
		return str
	}

	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}

	var sb strings.Builder
	for _, blk := range blocks {
		var btype string
		_ = json.Unmarshal(blk["type"], &btype)
		if btype == "text" {
			var txt string
			_ = json.Unmarshal(blk["text"], &txt)
			appendLine(&sb, txt)
		}
	}
	return sb.String()
}

// isNoiseUserText reports whether a "user" message is Claude Code plumbing
// (slash-command wrappers, local-command output, injected system reminders,
// interrupt markers) rather than something the human actually typed. These
// would otherwise clutter the phone view's user-message anchors.
func isNoiseUserText(text string) bool {
	t := strings.TrimSpace(text)
	prefixes := []string{
		"<command-",         // slash-command wrappers: <command-name>, <command-message>, <command-args>
		"<local-command-",   // local-command plumbing: -stdout, -stderr, -caveat
		"<system-reminder>",
		"<task-notification>", // background-agent completion notices injected as a user turn
		"Caveat: The messages below were generated by the user",
		"[Request interrupted",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

func appendLine(sb *strings.Builder, txt string) {
	if txt == "" {
		return
	}
	if sb.Len() > 0 {
		sb.WriteString("\n")
	}
	sb.WriteString(txt)
}

// toolSummary derives a short, human-friendly one-liner from a tool_use input
// object (basename of a path, a truncated command, etc.).
func toolSummary(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	for _, key := range []string{"file_path", "path", "command", "pattern", "url", "query", "description", "prompt"} {
		raw, ok := m[key]
		if !ok {
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			if key == "file_path" || key == "path" {
				v = filepath.Base(v)
			}
			return truncateSummary(v, 60)
		}
	}
	// Fallback: first usable string value (map order is non-deterministic, but
	// any short label beats none).
	for _, raw := range m {
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			return truncateSummary(v, 60)
		}
	}
	return ""
}

func truncateSummary(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---- POST /api/mobile/session/{id}/send ----

func (s *Server) handleMobileSend(w http.ResponseWriter, r *http.Request) {
	if handledPreflight(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeMobileError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeMobileError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeMobileError(w, http.StatusBadRequest, "text is required")
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
	if !inst.Exists() {
		writeMobileError(w, http.StatusConflict, "session is not running")
		return
	}
	tmuxSess := inst.GetTmuxSession()
	if tmuxSess == nil {
		writeMobileError(w, http.StatusConflict, "could not determine tmux session")
		return
	}

	// Fire the readiness wait + keystrokes in the background and ack the phone
	// immediately: WaitForAgentReady can block for seconds while the composer
	// mounts, and the phone already echoes the message optimistically, so there
	// is nothing to gain from making it wait on the HTTP response.
	go func() {
		_ = send.WaitForAgentReady(tmuxSess, inst.Tool, 8*time.Second, send.PromptGates{
			ClaudeComposer: session.IsClaudeCompatible(inst.Tool),
			CodexPrompt:    session.IsCodexCompatible(inst.Tool),
		})
		_ = tmuxSess.SendKeysAndEnter(text)
	}()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
