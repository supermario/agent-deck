package ui

// The row badge names the Claude config dir a session runs against, and only
// when that is not the ordinary ~/.claude.
//
// Upstream #2122/#2238 badged the stored account slot instead, labelling every
// session without one "inherited". On a machine whose work sessions inherit
// ~/.claude-work from [groups."work".claude] that reads backwards: the handful
// pinned with `switch-account` were named, the rest were called inherited, and
// the answer to "which of these is on the work account" was absent from the one
// place it matters. Resolving the dir answers it for both, and a session on the
// default dir needs no badge at all, so the common row stays clean.

const storedAccountPrefix = " ["

// Immutable presentation travels with the resolved label in the render
// snapshot. Width-independent for rows that share a label.
type accountPresentation struct {
	label string
	badge string
	width int
}

// newAccountPresentation builds the badge for an already-resolved label. An
// empty label is the zero value: no badge, no reserved width.
func newAccountPresentation(label string) accountPresentation {
	if label == "" {
		return accountPresentation{}
	}
	return accountPresentation{
		label: label,
		badge: storedAccountPrefix + label + "]",
		width: len(storedAccountPrefix) + cellWidth(label) + 1,
	}
}

// fit truncates to the row's remaining budget, keeping the delimiters visible
// so a shortened label still reads as a badge rather than stray text.
func (p accountPresentation) fit(budget int) (string, int) {
	if p.width <= budget {
		return p.badge, p.width
	}
	available := budget - len(storedAccountPrefix) - 1
	if available < 1 {
		return "", 0
	}
	label := cellTruncate(p.label, available, "…")
	return storedAccountPrefix + label + "]", len(storedAccountPrefix) + cellWidth(label) + 1
}
