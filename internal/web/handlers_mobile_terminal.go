package web

// Live "frontier" view for the BentoLife phone app.
//
// GET (WebSocket) /api/mobile/session/{id}/terminal streams the session's
// VISIBLE tmux pane content as plain-text snapshots, polled a few times a
// second. This is the real-time leading edge: the thinking spinner, the tool
// currently running, and text as Claude emits it - everything the desktop pane
// shows - before it settles into the structured transcript blocks the phone
// already gets over /api/mobile/session/{id}/stream.
//
// Why capture-pane rather than a `tmux attach` PTY (what the web terminal
// uses): attaching spawns a real tmux client, which can (a) inject keystrokes
// on an accidental touch and (b) participate in window-size arbitration and
// reshape the user's live DESKTOP session. capture-pane creates NO client - it
// is a pure read of the pane's current display - so it cannot disturb the
// running session in any way. It also sidesteps the width-mismatch problem of
// rendering a desktop-sized terminal grid on a phone: we send the already-
// rendered pane text and show its tail.
//
// Like the rest of /api/mobile/*, this is token-less and permissive on origin:
// a loopback bind fronted by `tailscale serve`, loaded from a file:// origin.

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// mobileTerminalPollInterval is how often we re-read the pane. Fast enough that
// the thinking spinner and streaming text feel live, slow enough that a forked
// `tmux capture-pane` a few times a second is negligible.
const mobileTerminalPollInterval = 200 * time.Millisecond

// mobileTerminalTailLines caps how many pane lines we forward - the "limited
// number of rows" of the frontier. Enough to show the current activity plus a
// little context, without shipping a whole 50-row desktop pane to the phone.
const mobileTerminalTailLines = 20

// ---- GET (WS) /api/mobile/session/{id}/terminal ----

func (s *Server) handleMobileTerminal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMobileError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	id := r.PathValue("id")

	// Resolve the tmux session/socket from the menu snapshot (same source the
	// web terminal uses), so the capture targets exactly this session's pane.
	snapshot, err := s.menuData.LoadMenuSnapshot()
	if err != nil {
		writeMobileError(w, http.StatusInternalServerError, "failed to load session")
		return
	}
	menuSession, found := snapshotSessionByID(snapshot, id)
	if !found {
		writeMobileError(w, http.StatusNotFound, "session not found")
		return
	}
	tmuxSession := menuSession.TmuxSession
	tmuxSocket := menuSession.TmuxSocketName

	conn, err := mobileWSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Reader goroutine: the phone sends nothing, but draining lets us notice
	// the client leaving (read error → cancel).
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				cancel()
				return
			}
		}
	}()

	if tmuxSession == "" {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("(no live terminal for this session)"))
		<-ctx.Done()
		return
	}

	ticker := time.NewTicker(mobileTerminalPollInterval)
	defer ticker.Stop()

	var lastHash [32]byte
	var lastPing time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		frame, ok := capturePaneTail(tmuxSession, tmuxSocket, mobileTerminalTailLines)
		if !ok {
			// Pane gone (session ended/rolled): tell the client once and keep
			// the socket open so it can fall back to the settled transcript.
			frame = "(terminal unavailable)"
		}

		h := sha256.Sum256([]byte(frame))
		if h != lastHash {
			lastHash = h
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
				return
			}
			continue
		}

		// Unchanged pane: send an occasional ping so idle sessions keep the
		// socket alive through the tailscale proxy without spamming frames.
		if time.Since(lastPing) > 25*time.Second {
			lastPing = time.Now()
			_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
		}
	}
}

// capturePaneTail reads the session's visible pane and returns its last n
// non-trailing-blank lines as plain text. Returns ok=false if the pane can't
// be read (session gone). No tmux client is created - this is a pure display
// read that cannot affect the running session's size or input.
func capturePaneTail(tmuxSession, tmuxSocket string, n int) (string, bool) {
	out, err := tmuxCommand(tmuxSocket, "capture-pane", "-p", "-t", tmuxSession).Output()
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(out), "\n")

	// Drop trailing blank lines (the pane is a fixed height, usually padded).
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	lines = lines[:end]

	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), true
}
