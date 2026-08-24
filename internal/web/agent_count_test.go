package web

import "testing"

// The roster Claude renders under the footer while subagents are live. Captured
// verbatim from the lamdera-martin session.
const rosterPane = `
  I'll verify its claims by building and running the binary myself.
✻ Waiting for 1 background agent to finish
                                                            434548 tokens
────────────────────────────────────────────────────────────────────────
❯ ok let me know when it's done
────────────────────────────────────────────────────────────────────────
  Opus 5 (1M context) | ctx: 43% | $273.12                            /rc
  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents
  ⏺ main
  ◯ general-purpose  Verifying IOG cache narinfo hits   36m 53s · ↓ 177.2k tokens
`

func TestParseAgentCountFromPane(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want int
	}{
		{"one running agent", rosterPane, 1},
		{
			"several agents",
			"  ⏵⏵ bypass permissions on · ← for agents\n  ⏺ main\n" +
				"  ◯ general-purpose  a  1s · ↓ 1k tokens\n" +
				"  ◯ Explore  b  2s · ↓ 2k tokens\n" +
				"  ◯ Plan  c  3s · ↓ 3k tokens\n",
			3,
		},
		{
			"no roster at all",
			"  Opus 5 | ctx: 48%\n  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents\n",
			0,
		},
		{"empty pane", "", 0},
		{
			// The bounded walk is the whole point: a conversation quoting a
			// roster row must not be counted, because the footer sits below it.
			"roster-looking text in the transcript",
			"  ◯ general-purpose  quoted in chat  1s · ↓ 1k tokens\n" +
				"  I was explaining the roster format above.\n" +
				"  ⏵⏵ bypass permissions on · ← for agents\n",
			0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseAgentCountFromPane(c.pane); got != c.want {
				t.Errorf("parseAgentCountFromPane = %d, want %d", got, c.want)
			}
		})
	}
}

// A roster pushes the footer up off the last line. Before agents were parsed the
// shell count read the roster row instead and silently returned 0.
func TestParseShellCountSkipsRoster(t *testing.T) {
	pane := "  ⏵⏵ bypass permissions on (shift+tab to cycle) · 2 shells · ← for agents\n" +
		"  ⏺ main\n" +
		"  ◯ general-purpose  doing a thing  10s · ↓ 5k tokens\n"

	if got := parseShellCountFromPane(pane); got != 2 {
		t.Errorf("shell count with roster present = %d, want 2", got)
	}
	if got := parseAgentCountFromPane(pane); got != 1 {
		t.Errorf("agent count = %d, want 1", got)
	}

	// Still correct with no roster.
	plain := "  ⏵⏵ bypass permissions on · 3 shells · ← for agents\n"
	if got := parseShellCountFromPane(plain); got != 3 {
		t.Errorf("shell count without roster = %d, want 3", got)
	}
}
