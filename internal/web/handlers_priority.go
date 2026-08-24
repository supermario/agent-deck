package web

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// sessionPriorities returns the global attention rank per instance id, 1 =
// highest. Missing/0 means unranked.
//
// Read straight from the DB rather than carried on Instance: priority is set
// from outside the process (the overlay's drag order) and read by every consumer
// on every poll, so a shared snapshot would just be a staleness bug waiting to
// happen. One indexed read of a handful of rows is cheaper than that risk.
//
// Never fails a request: an unavailable DB yields an empty map, which renders as
// "nothing ranked" rather than an error page.
func sessionPriorities() map[string]int {
	db := statedb.GetGlobal()
	if db == nil {
		return map[string]int{}
	}
	p, err := db.ReadSessionPriorities()
	if err != nil {
		overlayLog.Warn("session_priorities_read_failed", slog.String("error", err.Error()))
		return map[string]int{}
	}
	return p
}

type priorityRequest struct {
	Order []string `json:"order"`
}

// handleSessionPriority sets the global priority order.
//
// The client sends the WHOLE ordering, not a single move: that's what a
// drag-and-drop knows, and applying it as one transaction means there's never an
// instant where two sessions share a rank. Anything omitted is unranked, so
// dragging a session out of the list actually drops it rather than leaving a
// stale rank that outranks something you just ordered.
func (s *Server) handleSessionPriority(w http.ResponseWriter, r *http.Request) {
	if handledPreflight(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"priorities": sessionPriorities()})

	case http.MethodPost, http.MethodPut:
		var req priorityRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeMobileError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		db := statedb.GetGlobal()
		if db == nil {
			writeMobileError(w, http.StatusServiceUnavailable, "state database unavailable")
			return
		}
		if err := db.WriteSessionPriorities(req.Order); err != nil {
			overlayLog.Warn("session_priorities_write_failed", slog.String("error", err.Error()))
			writeMobileError(w, http.StatusInternalServerError, "failed to save priority order")
			return
		}
		// Push the new order to the overlay immediately rather than waiting for
		// the next 5s tick — the drag that caused this is still under the user's
		// finger, and a visible lag reads as the drag not having taken.
		if s.overlay != nil {
			s.overlay.resetHash()
			s.overlay.triggerAsync(s.baseCtx)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ranked": len(req.Order)})

	case http.MethodDelete:
		// POST deliberately preserves ranks it wasn't told about, so it can no
		// longer express "forget everything". This can.
		db := statedb.GetGlobal()
		if db == nil {
			writeMobileError(w, http.StatusServiceUnavailable, "state database unavailable")
			return
		}
		if err := db.ClearSessionPriorities(); err != nil {
			writeMobileError(w, http.StatusInternalServerError, "failed to clear priority order")
			return
		}
		if s.overlay != nil {
			s.overlay.resetHash()
			s.overlay.triggerAsync(s.baseCtx)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": true})

	default:
		writeMobileError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
