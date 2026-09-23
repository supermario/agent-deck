package web

import (
	"testing"
	"time"
)

const remoteSnapshotJSON = `[
  {"session_id":"aaa-1","tool":"claude-code","cwd":"/work/one","idle_seconds":12,
   "ttl_seconds":3600,"summary":"atlas-item-4076","open_command":"x","is_active":true,
   "shell_count":2,"agent_count":1,"host":"WEBJET-FN6WQJ99FF"}
]`

// Rows arrive labelled with the remote's OWN hostname. The controller has to
// relabel them with the name it knows that machine as, because that name is
// what the badge shows and what click routing resolves against.
func TestParseRemoteOverlaySessions_RelabelsHost(t *testing.T) {
	sessions, err := parseRemoteOverlaySessions([]byte(remoteSnapshotJSON), "wbtmbp")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if sessions[0].Host != "wbtmbp" {
		t.Errorf("Host = %q, want the controller's name for the machine", sessions[0].Host)
	}
	// The rest of the row must survive intact: these are the fields only the
	// remote machine can compute.
	if sessions[0].ShellCount != 2 || sessions[0].AgentCount != 1 {
		t.Errorf("pane-derived counts lost: shells=%d agents=%d", sessions[0].ShellCount, sessions[0].AgentCount)
	}
	if sessions[0].Summary != "atlas-item-4076" || !sessions[0].IsActive {
		t.Errorf("unexpected row: %+v", sessions[0])
	}
}

// A remote running a deck too old for the command answers with usage text, and
// a login shell can prepend banners. Neither is an error worth logging every
// 15s; the machine just contributes nothing until it is updated.
func TestParseRemoteOverlaySessions_NonArrayOutputIsEmpty(t *testing.T) {
	for _, out := range []string{
		"Usage: agent-deck <command>",
		"agent-deck: heads-up - your tmux has an unfixed control-mode NULL deref",
	} {
		sessions, err := parseRemoteOverlaySessions([]byte(out), "wbtmbp")
		if err != nil {
			t.Errorf("output %q: unexpected error %v", out, err)
		}
		if sessions != nil {
			t.Errorf("output %q: want no rows, got %d", out, len(sessions))
		}
	}
}

// Nothing at all is different from "no sessions", and must be reported.
//
// The transport can answer successfully with an empty body: the remote agent's
// in-flight request sharing did exactly that for this verb. Treating that as an
// empty fleet hid a whole machine behind a silent success, with no error
// anywhere to explain the missing rows.
func TestParseRemoteOverlaySessions_EmptyOutputIsAnError(t *testing.T) {
	for _, out := range []string{"", "   \n"} {
		if _, err := parseRemoteOverlaySessions([]byte(out), "wbtmbp"); err == nil {
			t.Errorf("output %q: expected an error, got none", out)
		}
	}
}

// Truncated or corrupt JSON must be reported, not silently treated as an empty
// fleet: an empty fleet and a broken fetch look identical on the overlay.
func TestParseRemoteOverlaySessions_MalformedIsAnError(t *testing.T) {
	if _, err := parseRemoteOverlaySessions([]byte(`[{"session_id":"a"`), "wbtmbp"); err == nil {
		t.Error("expected an error for truncated JSON")
	}
}

func TestShouldRefresh(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		entry remoteOverlayEntry
		want  bool
	}{
		{"never fetched", remoteOverlayEntry{}, true},
		{"just tried", remoteOverlayEntry{lastTry: now.Add(-time.Second)}, false},
		{"due", remoteOverlayEntry{lastTry: now.Add(-remoteOverlayInterval - time.Second)}, true},
		// A hung host must not accumulate goroutines: one fetch at a time.
		{"in flight", remoteOverlayEntry{inFlight: true, lastTry: now.Add(-time.Hour)}, false},
	}
	for _, c := range cases {
		if got := shouldRefresh(&c.entry, now); got != c.want {
			t.Errorf("%s: shouldRefresh = %v, want %v", c.name, got, c.want)
		}
	}
}

// A machine off Wi-Fi for a moment should keep its rows; one genuinely gone
// should not linger forever pretending to be there.
func TestUsableSessions_StalenessWindow(t *testing.T) {
	now := time.Now()
	rows := []overlaySession{{SessionID: "a"}}

	if got := usableSessions(&remoteOverlayEntry{}, now); got != nil {
		t.Error("a remote that never answered must contribute nothing")
	}
	fresh := remoteOverlayEntry{sessions: rows, fetchedAt: now.Add(-time.Minute)}
	if len(usableSessions(&fresh, now)) != 1 {
		t.Error("a brief outage must not blank the machine off the overlay")
	}
	stale := remoteOverlayEntry{sessions: rows, fetchedAt: now.Add(-remoteOverlayMaxStale - time.Minute)}
	if usableSessions(&stale, now) != nil {
		t.Error("rows past the staleness window must be dropped")
	}
}
