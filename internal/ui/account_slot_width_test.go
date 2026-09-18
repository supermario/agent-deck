package ui

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/stretchr/testify/require"
)

// The badge shares the row's title budget, so every label length has to stay
// inside the terminal width at every width, selected or not.
func TestConfigDirLabelWidthMatrix(t *testing.T) {
	labels := []string{
		"",
		"work",
		strings.Repeat(".claude-日本é\U0001f680", 20),
	}
	for _, label := range labels {
		for _, width := range []int{24, 40, 72, 120} {
			for _, auto := range []bool{false, true} {
				t.Run(fmt.Sprintf("%.24q/%d/auto=%v", label, width, auto), func(t *testing.T) {
					h := NewHome()
					h.width, h.height = width, 40
					inst := &session.Instance{
						ID:     "width",
						Title:  strings.Repeat("Title日本é", 8),
						Tool:   "claude",
						Status: session.StatusIdle,
					}
					state := sessionRenderState{
						status:         session.StatusIdle,
						tool:           "claude",
						title:          inst.Title,
						accountDisplay: newAccountPresentation(label),
						autoName:       auto,
						paneTitle:      "pane subtitle",
						autoNameDesc:   "description",
					}
					for _, selected := range []bool{false, true} {
						var b strings.Builder
						h.renderSessionItem(&b, session.Item{
							Type: session.ItemTypeSession, Session: inst, Level: 1, Path: "work", IsLastInGroup: true,
						}, selected, map[string]sessionRenderState{inst.ID: state}, width)
						line := strings.TrimSuffix(b.String(), "\n")
						require.True(t, utf8.ValidString(line))
						require.LessOrEqual(t, cellWidth(line), width, "badge must not push the row past the terminal")
						require.NotContains(t, line, "\n")
						require.NotContains(t, line, "\r")
						if label == "" {
							require.NotContains(t, line, "[work")
						}
					}
				})
			}
		}
	}
}

// A label too long for the row is truncated with its delimiters intact, so a
// shortened dir still reads as a badge rather than as stray text.
func TestConfigDirLabelTruncationKeepsDelimiters(t *testing.T) {
	p := newAccountPresentation("work-with-a-very-long-name")
	badge, width := p.fit(12)
	require.True(t, strings.HasPrefix(badge, " ["), "badge = %q", badge)
	require.True(t, strings.HasSuffix(badge, "]"), "badge = %q", badge)
	require.Contains(t, badge, "…", "a truncated label must show it was cut")
	require.LessOrEqual(t, width, 12)
	require.Equal(t, width, cellWidth(badge))
}
