package web

// Live transcript streaming for the BentoLife phone app.
//
// GET (WebSocket) /api/mobile/session/{id}/stream upgrades to a WebSocket and
// pushes structured Claude turn events (the same StreamEvent schema the CLI's
// `session send --stream` emits) as text frames, live, as they are written to
// the session's transcript. The phone opens this after fetching the initial
// transcript so it sees new assistant text / tool calls within ~100ms instead
// of waiting on the 1.5s REST poll.
//
// Like the rest of /api/mobile/*, this is token-less and permissive on origin:
// the deployment is a loopback bind fronted by `tailscale serve`, and the phone
// loads from a file:// origin (Origin: null), which the default same-Host WS
// check would reject.

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/gorilla/websocket"
)

// mobileWSUpgrader accepts any origin. Safe here because the surface is
// tailnet-only and read-only (it only streams transcript events; input still
// goes through the CSRF-exempt POST /send).
var mobileWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// wsLineWriter adapts a WebSocket connection to an io.Writer that StreamTranscript
// can emit JSONL into: it buffers partial writes and flushes each complete line
// as one text frame.
type wsLineWriter struct {
	conn *websocket.Conn
	mu   sync.Mutex
	buf  []byte
}

func (w *wsLineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := w.buf[:i]
		w.buf = w.buf[i+1:]
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		frame := make([]byte, len(line))
		copy(frame, line)
		if err := w.conn.WriteMessage(websocket.TextMessage, frame); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// ---- GET (WS) /api/mobile/session/{id}/stream ----

func (s *Server) handleMobileStream(w http.ResponseWriter, r *http.Request) {
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

	conn, err := mobileWSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Reader goroutine: the phone doesn't send anything on this socket, but we
	// must drain it to receive close/pong frames and notice the client leaving.
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				cancel()
				return
			}
		}
	}()

	// Keepalive pings (WriteControl is safe concurrently with WriteMessage).
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			}
		}
	}()

	lw := &wsLineWriter{conn: conn}

	// Live tail across turns: StreamTranscript returns at each end_turn; we
	// restart it (streaming only records newer than "now") to pick up the next
	// turn, until the client disconnects. IdleTimeout is set very high so quiet
	// gaps between turns don't end the stream.
	cfg := session.StreamConfig{IdleTimeout: 24 * time.Hour}
	for ctx.Err() == nil {
		inst.RefreshLiveSessionIDs()
		path := inst.GetJSONLPath()
		if path == "" {
			path = latestTranscriptOnDisk(inst)
		}
		if path == "" {
			if sleepCtx(ctx, 500*time.Millisecond) {
				return
			}
			continue
		}

		sentAt := time.Now()
		_ = session.StreamTranscript(ctx, path, inst.ID, sentAt, lw, cfg)
		if ctx.Err() != nil {
			return
		}
		// end_turn / timeout: brief pause, then resume tailing for the next turn.
		if sleepCtx(ctx, 250*time.Millisecond) {
			return
		}
	}
}

// sleepCtx sleeps for d or until ctx is done; returns true if ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}
