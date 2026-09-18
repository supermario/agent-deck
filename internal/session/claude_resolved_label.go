package session

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// DefaultClaudeConfigDir is the config dir Claude uses with no override:
// ~/.claude. Returns "" when the home directory cannot be determined, which
// callers must read as "cannot tell", not as "is the default".
func DefaultClaudeConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// ResolvedClaudeConfigDirFor answers GetClaudeConfigDirSourceForInstance's
// question from values already in hand rather than from a live *Instance.
//
// The TUI needs this shape. Its row metadata is captured on a background
// goroutine, where Account and Title are read through Instance.mu accessors but
// GroupPath has none (its writers, MoveSessionToGroup among them, assign the
// field directly). Taking the three values as arguments keeps resolution off
// Instance entirely, so no caller has to read a field it cannot read safely.
//
// The throwaway Instance carries only what the resolver reads (Account,
// GroupPath, and Title for the conductor- prefix), is never shared, and is
// never locked. Priority is unchanged: account, conductor, group, env, profile,
// global, default.
func ResolvedClaudeConfigDirFor(account, groupPath, title string) (path, source string) {
	return resolveClaudeConfigDir(resolveOpts{
		inst:      &Instance{Account: account, GroupPath: groupPath, Title: title},
		groupPath: groupPath,
	})
}

// NonDefaultClaudeConfigLabel names the Claude config dir a session runs
// against, and is empty when that is the ordinary ~/.claude.
//
// This is the "which account is this?" question a multi-account user actually
// asks, and it is deliberately not the stored account slot: a session inherits
// its dir from [groups."<path>".claude] or a conductor block just as really as
// one pinned with `session switch-account`, and a row that reported only the
// stored slot would show nothing for it.
//
// The label comes from the directory rather than the profile that selected it:
// a group override names a path, not a slot, so the path is the one thing every
// route has in common and the user can verify. It is shortened by the naming
// convention those dirs follow, so ~/.claude-work reads as "work" on the row.
// Control and bidi characters are dropped: a config value must not be able to
// emit terminal escapes or visually reorder a row.
func NonDefaultClaudeConfigLabel(account, groupPath, title string) string {
	resolved, _ := ResolvedClaudeConfigDirFor(account, groupPath, title)
	resolved = strings.TrimSpace(resolved)
	if resolved == "" {
		return ""
	}
	if def := DefaultClaudeConfigDir(); def == "" || filepath.Clean(resolved) == filepath.Clean(def) {
		return ""
	}
	return shortClaudeDirLabel(sanitizeConfigDirLabel(filepath.Base(filepath.Clean(resolved))))
}

// sanitizeConfigDirLabel keeps the label printable on a single row. A config
// dir is user-supplied text that reaches a terminal, so controls (escape
// included), line breaks and bidi overrides are removed rather than escaped;
// the same characters upstream's claudeTitleName refuses in a session name.
func sanitizeConfigDirLabel(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '\u2028' || r == '\u2029' || unicode.Is(unicode.Bidi_Control, r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// shortClaudeDirLabel drops the prefix these directories conventionally carry,
// so ~/.claude-work reads as "work" instead of repeating "claude" on every row.
// A directory that does not follow the convention keeps its own name, and a
// bare ".claude" somewhere other than home keeps its too rather than vanishing.
func shortClaudeDirLabel(name string) string {
	for _, prefix := range []string{".claude-", "claude-"} {
		if trimmed := strings.TrimPrefix(name, prefix); trimmed != name && trimmed != "" {
			return trimmed
		}
	}
	return name
}
