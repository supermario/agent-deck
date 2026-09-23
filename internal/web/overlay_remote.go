package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// Pulling other machines' decks into this machine's overlay.
//
// Each remote runs its own agent-deck, unaware it is being watched: the
// controller runs `agent-deck overlay-snapshot --json` there over SSH, which is
// read-only and emits exactly the rows that deck would push to its own overlay.
// Fetching the finished rows rather than re-deriving them from `list --json`
// matters because several fields can only be computed on the machine itself:
// the pane-derived shell and agent counts, the transcript-derived countdown, and
// its own attention priorities.

const (
	// How often to refresh one remote. Slower than the 5s local push: these
	// cost an SSH round trip, and a countdown that is a few seconds stale reads
	// the same to a person.
	remoteOverlayInterval = 15 * time.Second
	// Bound one fetch so a hung host cannot pile up goroutines.
	remoteOverlayTimeout = 20 * time.Second
	// Keep showing a remote's last known rows this long after it stops
	// answering. A machine that drops off Wi-Fi for a moment should not blink
	// its whole fleet off the overlay; one that is genuinely gone should not
	// linger forever.
	remoteOverlayMaxStale = 5 * time.Minute
	// How idle a remote session may be and still be pulled. Wider than the
	// local default on purpose: a session on another machine is one you cannot
	// see in your own deck at all, so hiding it after two hours defeats the
	// point. Matches the overlay client's own retention window.
	remoteOverlayMaxIdle = 24 * time.Hour
)

type remoteOverlayEntry struct {
	sessions  []overlaySession
	fetchedAt time.Time // last SUCCESSFUL fetch
	inFlight  bool
	lastTry   time.Time
	lastErr   string // last reported failure, so it is logged on change only
}

type remoteOverlayCache struct {
	mu      sync.Mutex
	entries map[string]*remoteOverlayEntry
}

func (c *remoteOverlayCache) entry(name string) *remoteOverlayEntry {
	if c.entries == nil {
		c.entries = map[string]*remoteOverlayEntry{}
	}
	if c.entries[name] == nil {
		c.entries[name] = &remoteOverlayEntry{}
	}
	return c.entries[name]
}

// remoteOverlaySessions returns the cached rows for every configured remote and
// kicks off refreshes that are due.
//
// Deliberately never blocks on SSH: the push loop runs every 5s and a slow host
// would otherwise delay the whole overlay, including this machine's own rows.
// A remote that has never answered simply contributes nothing.
func (o *overlayPusher) remoteOverlaySessions(ctx context.Context) []overlaySession {
	config, err := session.LoadUserConfig()
	if err != nil || config == nil || len(config.Remotes) == 0 {
		return nil
	}

	now := time.Now()
	var out []overlaySession

	o.remotes.mu.Lock()
	defer o.remotes.mu.Unlock()
	for name, rc := range config.Remotes {
		e := o.remotes.entry(name)
		if shouldRefresh(e, now) {
			e.inFlight = true
			e.lastTry = now
			go o.fetchRemoteOverlay(ctx, name, rc)
		}
		out = append(out, usableSessions(e, now)...)
	}
	return out
}

func (o *overlayPusher) fetchRemoteOverlay(ctx context.Context, name string, rc session.RemoteConfig) {
	fetchCtx, cancel := context.WithTimeout(ctx, remoteOverlayTimeout)
	defer cancel()

	sessions, err := fetchRemoteOverlaySessions(fetchCtx, name, rc)

	o.remotes.mu.Lock()
	defer o.remotes.mu.Unlock()
	e := o.remotes.entry(name)
	e.inFlight = false
	if err != nil {
		overlayLog.Debug("overlay_remote_fetch_failed",
			slog.String("remote", name), slog.String("error", err.Error()))
		// Also say so where a person will actually see it. The structured log
		// is off on most installs, and a remote that silently contributes
		// nothing is indistinguishable from one with no sessions. Printed only
		// when the failure CHANGES, so a host that is simply down does not
		// write a line every 15 seconds forever.
		if msg := err.Error(); msg != e.lastErr {
			e.lastErr = msg
			fmt.Fprintf(os.Stderr, "overlay: remote %s unreachable: %s\n", name, msg)
		}
		return
	}
	if e.lastErr != "" {
		fmt.Fprintf(os.Stderr, "overlay: remote %s recovered\n", name)
		e.lastErr = ""
	}
	e.sessions = sessions
	e.fetchedAt = time.Now()
}

// fetchRemoteOverlaySessions runs the snapshot command on one remote and tags
// every row with that remote's name.
func fetchRemoteOverlaySessions(ctx context.Context, name string, rc session.RemoteConfig) ([]overlaySession, error) {
	runner := session.NewSSHRunner(name, rc)
	out, err := runner.Run(ctx, "overlay-snapshot", "--json", "--max-idle", remoteOverlayMaxIdle.String())
	if err != nil {
		return nil, err
	}
	return parseRemoteOverlaySessions(out, name)
}

// parseRemoteOverlaySessions decodes one remote's snapshot and relabels every
// row with the name this controller knows that machine as.
func parseRemoteOverlaySessions(out []byte, name string) ([]overlaySession, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		// Reported rather than swallowed. An empty reply used to read as "that
		// machine has no sessions", which is indistinguishable from a transport
		// that answered successfully with nothing: exactly the failure that hid
		// a whole machine's fleet behind a silent success.
		return nil, fmt.Errorf("empty snapshot from %s", name)
	}
	if trimmed[0] != '[' {
		// A remote too old for the command prints usage, and a login shell can
		// prepend banners. Not worth an error: the machine simply contributes
		// no rows until it is updated.
		return nil, nil
	}
	var sessions []overlaySession
	if err := json.Unmarshal([]byte(trimmed), &sessions); err != nil {
		return nil, fmt.Errorf("parse overlay snapshot from %s: %w", name, err)
	}
	for i := range sessions {
		// The remote labels rows with ITS own hostname; the controller keys
		// them by the name it knows the machine as, which is what the badge and
		// the click routing use.
		sessions[i].Host = name
	}
	return sessions, nil
}

// shouldRefresh reports whether this remote is due for a fetch.
func shouldRefresh(e *remoteOverlayEntry, now time.Time) bool {
	return !e.inFlight && now.Sub(e.lastTry) >= remoteOverlayInterval
}

// usableSessions returns the rows worth showing for a remote: the last good
// fetch, until it goes stale enough that showing it would be a lie.
func usableSessions(e *remoteOverlayEntry, now time.Time) []overlaySession {
	if e.fetchedAt.IsZero() || now.Sub(e.fetchedAt) > remoteOverlayMaxStale {
		return nil
	}
	return e.sessions
}

// localHostLabel is this machine's short hostname, lowercased.
func localHostLabel() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "local"
	}
	if i := strings.IndexByte(name, '.'); i > 0 {
		name = name[:i]
	}
	return strings.ToLower(name)
}

// BuildOverlaySnapshotJSON returns this machine's overlay rows as JSON, for the
// overlay-snapshot command. Local rows only: a controller pulling this must not
// receive rows this deck pulled from somewhere else, or a pair of machines
// pointing at each other would echo their fleets back and forth.
func BuildOverlaySnapshotJSON(profile string, maxIdle time.Duration) ([]byte, error) {
	pusher := newOverlayPusher(NewSessionDataService(profile))
	pusher.maxIdle = maxIdle
	sessions, ok := pusher.buildLocalSessions()
	if !ok {
		return nil, fmt.Errorf("could not load the session snapshot")
	}
	host := localHostLabel()
	for i := range sessions {
		sessions[i].Host = host
	}
	if sessions == nil {
		sessions = []overlaySession{}
	}
	pusher.stats.Emitted = len(sessions)
	return json.MarshalIndent(sessions, "", "  ")
}

// BuildOverlayFleetJSON returns this machine's rows plus every configured
// remote's: the same set the overlay receives once the pusher has warmed up.
//
// Fetches remotes synchronously, unlike the push loop, because someone running
// this by hand wants the answer now rather than a cache that fills in later. A
// remote that fails is reported on stderr and skipped, so one unreachable
// machine still leaves a usable listing.
func BuildOverlayFleetJSON(profile string, maxIdle time.Duration) ([]byte, error) {
	pusher := newOverlayPusher(NewSessionDataService(profile))
	pusher.maxIdle = maxIdle
	sessions, ok := pusher.buildLocalSessions()
	if !ok {
		return nil, fmt.Errorf("could not load the session snapshot")
	}
	host := localHostLabel()
	for i := range sessions {
		sessions[i].Host = host
	}

	if config, err := session.LoadUserConfig(); err == nil && config != nil {
		for name, rc := range config.Remotes {
			ctx, cancel := context.WithTimeout(context.Background(), remoteOverlayTimeout)
			rows, ferr := fetchRemoteOverlaySessions(ctx, name, rc)
			cancel()
			if ferr != nil {
				fmt.Fprintf(os.Stderr, "remote %s: %v\n", name, ferr)
				continue
			}
			sessions = append(sessions, rows...)
		}
	}

	if sessions == nil {
		sessions = []overlaySession{}
	}
	return json.MarshalIndent(sessions, "", "  ")
}

// ExplainOverlaySnapshot describes what the last snapshot build kept and
// dropped. A machine that contributes no rows is otherwise silent, and the
// cause differs every time: no sessions at all, a tool the overlay does not
// render, a status that is not live, or a fleet that has simply been idle
// longer than the overlay shows.
func ExplainOverlaySnapshot(profile string, maxIdle time.Duration) string {
	pusher := newOverlayPusher(NewSessionDataService(profile))
	pusher.maxIdle = maxIdle
	sessions, ok := pusher.buildLocalSessions()
	if !ok {
		return "could not load the session snapshot (storage unreadable for this profile)"
	}
	st := pusher.stats
	return fmt.Sprintf(
		"sessions in snapshot: %d; dropped: tool-not-rendered=%d status-not-live=%d idle-too-long=%d; emitted: %d",
		st.Items, st.NoTool, st.StatusFiltered, st.IdleFiltered, len(sessions))
}
