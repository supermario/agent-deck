package ui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/stretchr/testify/require"
)

// RemoteSessionInfo carries no account or group, so a remote row cannot know
// which config dir the far side runs on. A local session sharing its ID must not
// lend the remote row its badge: the remote name collides by ID only, and
// claiming the local answer for it would be a fabricated statement about
// another machine.
func TestRemoteRowNeverBorrowsLocalConfigLabel(t *testing.T) {
	home := isolatedConfigHome(t)
	writeClaudeDirsConfig(t, home, map[string]string{"work": ".claude-work"}, nil)

	for _, width := range []int{40, 120} {
		for _, selected := range []bool{false, true} {
			local := claudeSession("same-id", "local", "work")
			h := homeWithSessions(t, width, local)
			require.Equal(t, "work", h.getSessionRenderState(local).accountDisplay.label,
				"precondition: the local row does carry a badge")

			remote := session.RemoteSessionInfo{
				ID: "same-id", Title: "remote日本", Tool: "claude", Status: "idle", RemoteName: "dev",
			}
			var b strings.Builder
			h.renderRemoteSessionItem(&b, session.Item{
				Type: session.ItemTypeRemoteSession, RemoteSession: &remote, RemoteName: "dev",
			}, selected)
			row := strings.TrimSuffix(b.String(), "\n")

			require.Contains(t, row, "remote日本")
			require.NotContains(t, row, "[work]", "the remote row must not borrow the local config dir")
			require.NotContains(t, row, "[work")
			require.True(t, utf8.ValidString(row))
			require.LessOrEqual(t, cellWidth(row), width)
			require.Equal(t, 1, strings.Count(b.String(), "\n"))
		}
	}
}
