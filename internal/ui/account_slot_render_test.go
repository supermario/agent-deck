package ui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// A config dir is user-supplied text that reaches a terminal through a session
// row, so the label has to survive odd names without emitting escapes, breaking
// the single-line contract, or disturbing the title.
func TestConfigDirLabelRendersSafely(t *testing.T) {
	for _, tc := range []struct {
		name, dir, wantLabel string
	}{
		{"plain", ".claude-work", "work"},
		{"wide runes", ".claude-\u65e5\u672c", "\u65e5\u672c"},
		{"combining and emoji", ".claude-e\u0301\U0001f680", "e\u0301\U0001f680"},
		{"escape sequence", ".claude-\x1b]0;injected\a", "]0;injected"},
		{"newline and bidi", ".claude-\r\n\u202ework", "work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolatedConfigHome(t)
			writeClaudeDirsConfig(t, home, map[string]string{"work": tc.dir}, nil)

			inst := claudeSession("render", "same-title", "work")
			h := homeWithSessions(t, 240, inst)

			require.Equal(t, tc.wantLabel, h.getSessionRenderState(inst).accountDisplay.label)
			for _, selected := range []bool{false, true} {
				row := renderRow(t, h, inst, selected)
				require.Contains(t, row, "["+tc.wantLabel+"]", "resolved dir must be visible on the row")
				require.Contains(t, row, "same-title")
				require.True(t, utf8.ValidString(row))
				for _, bad := range []string{"\x1b]0;injected", "\a", "\r"} {
					require.NotContains(t, row, bad)
				}
			}
			card := h.renderSessionInfoCard(inst, 240, 40)
			require.Contains(t, card, "Claude config:")
			require.Contains(t, card, tc.wantLabel)
			require.NotContains(t, card, "\x1b]0;injected")
		})
	}
}

// The snapshot is authoritative for the row: a label resolved earlier keeps
// rendering until labels are refreshed again, so a mid-frame config read can
// never change what a row says.
func TestConfigDirLabelSnapshotIsAuthoritative(t *testing.T) {
	home := isolatedConfigHome(t)
	writeClaudeDirsConfig(t, home, map[string]string{"work": ".claude-work"}, nil)

	inst := claudeSession("auth", "title", "work")
	h := homeWithSessions(t, 240, inst)
	require.Equal(t, "work", h.getSessionRenderState(inst).accountDisplay.label)

	// Config now says something else, but nothing has asked for a refresh.
	writeClaudeDirsConfig(t, home, map[string]string{"work": ".claude-other"}, nil)
	require.Equal(t, "work", h.getSessionRenderState(inst).accountDisplay.label,
		"published rows must not shift under a config read")

	h.refreshAccountLabels()
	h.refreshSessionRenderSnapshot(h.instances)
	require.Equal(t, "other", h.getSessionRenderState(inst).accountDisplay.label)
	require.Contains(t, renderRow(t, h, inst, false), "[other]")
}

// Recency and title rendering are unaffected by the badge.
func TestConfigDirLabelKeepsTitleAndRecency(t *testing.T) {
	home := isolatedConfigHome(t)
	writeClaudeDirsConfig(t, home, map[string]string{"work": ".claude-work"}, nil)

	inst := claudeSession("recency", "existing-title", "work")
	h := homeWithSessions(t, 240, inst)
	h.showSessionTimestamps = true

	row := renderRow(t, h, inst, false)
	require.Contains(t, row, "existing-title")
	require.Contains(t, row, "[work]")
	require.Equal(t, 1, strings.Count(row, "\n"))
}
