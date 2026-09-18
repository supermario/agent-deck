package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/stretchr/testify/require"
)

// The row badge names the Claude config dir a session runs against, and only
// when that dir is not the ordinary ~/.claude.
//
// Upstream #2122/#2238 badged the stored account slot, which labelled every
// session without one "inherited". On a machine whose work sessions inherit
// ~/.claude-work from [groups."work".claude], that told the user the opposite of
// what they needed: the two sessions pinned by `switch-account` were named, the
// twenty-three that really do run on the work account were called inherited, and
// the personal sessions carried the same inherited badge as the work ones.

// isolatedConfigHome points the config loader at a temp HOME. Call before
// NewHome so every later config read resolves inside the fixture.
func isolatedConfigHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".agent-deck"), 0o700))
	return home
}

// writeClaudeDirsConfig writes config.toml with the given group and profile
// Claude config dirs, each named relative to home. Re-callable within a test so
// a config change can be exercised.
func writeClaudeDirsConfig(t *testing.T, home string, groups, profiles map[string]string) {
	t.Helper()
	cfg := &session.UserConfig{}
	if len(groups) > 0 {
		cfg.Groups = make(map[string]session.GroupSettings, len(groups))
		for group, dir := range groups {
			cfg.Groups[group] = session.GroupSettings{
				Claude: session.GroupClaudeSettings{ConfigDir: filepath.Join(home, dir)},
			}
		}
	}
	if len(profiles) > 0 {
		cfg.Profiles = make(map[string]session.ProfileSettings, len(profiles))
		for profile, dir := range profiles {
			cfg.Profiles[profile] = session.ProfileSettings{
				Claude: session.ProfileClaudeSettings{ConfigDir: filepath.Join(home, dir)},
			}
		}
	}
	// WithIntent: a test moves between "group override configured" and "none",
	// and the plain save refuses to empty a populated [groups] section.
	require.NoError(t, session.SaveUserConfigWithIntent(cfg, true))
	session.ClearUserConfigCache()
}

// homeWithSessions builds a Home carrying these sessions, in the order
// production does it: the group tree first (refreshAccountLabels reads group
// paths from it), then labels, then the render snapshot that consumes them.
func homeWithSessions(t *testing.T, width int, instances ...*session.Instance) *Home {
	t.Helper()
	h := NewHome()
	h.width, h.height = width, 40
	h.instances = instances
	h.groupTree = session.NewGroupTree(instances)
	h.refreshAccountLabels()
	h.refreshSessionRenderSnapshot(instances)
	return h
}

func claudeSession(id, title, group string) *session.Instance {
	inst := session.NewInstanceWithGroupAndTool(title, "/tmp/"+id, group, "claude")
	inst.ID = id
	return inst
}

func renderRow(t *testing.T, h *Home, inst *session.Instance, selected bool) string {
	t.Helper()
	var b strings.Builder
	h.renderSessionItem(&b, session.Item{
		Type: session.ItemTypeSession, Session: inst, Level: 1, Path: inst.GroupPath, IsLastInGroup: true,
	}, selected, h.getSessionRenderSnapshot(), h.width)
	row := b.String()
	require.Equal(t, 1, strings.Count(row, "\n"), "a badge must never add a logical row")
	return row
}

// The common case: a session on ~/.claude carries no badge and costs no title
// width, which is the whole point of resolving rather than labelling "inherited".
func TestBadgeAbsentOnDefaultConfigDir(t *testing.T) {
	home := isolatedConfigHome(t)
	writeClaudeDirsConfig(t, home, map[string]string{"work": ".claude-work"}, nil)

	inst := claudeSession("personal", "personal-session", "my-sessions")
	h := homeWithSessions(t, 240, inst)

	require.Equal(t, accountPresentation{}, h.getSessionRenderState(inst).accountDisplay,
		"suppressed badge must be the zero value, not an empty-labelled one")
	for _, selected := range []bool{false, true} {
		row := renderRow(t, h, inst, selected)
		require.NotContains(t, row, "[work]")
		require.Contains(t, row, "personal-session", "suppressing the badge must not disturb the title")
	}
	card := h.renderSessionInfoCard(inst, 240, 40)
	require.NotContains(t, card, "Claude config:", "the card drops the line rather than printing an empty value")
}

// The case that motivated the change: the dir is inherited from the group, the
// session holds no stored slot, and it must still be identified.
func TestBadgeShownForGroupInheritedConfigDir(t *testing.T) {
	home := isolatedConfigHome(t)
	writeClaudeDirsConfig(t, home, map[string]string{"work": ".claude-work"}, nil)

	inst := claudeSession("atlas", "atlas-checker", "work")
	require.Empty(t, inst.GetAccountThreadSafe(), "precondition: no stored slot to read")
	h := homeWithSessions(t, 240, inst)

	for _, selected := range []bool{false, true} {
		require.Contains(t, renderRow(t, h, inst, selected), "[work]")
	}
	card := h.renderSessionInfoCard(inst, 240, 40)
	require.Contains(t, card, "Claude config:")
	require.Contains(t, card, "work")
}

// A stored slot reaches the same dir by another route and must read identically,
// so two sessions on one account never look like two different accounts.
func TestBadgeIdenticalForStoredSlotAndGroupOverride(t *testing.T) {
	home := isolatedConfigHome(t)
	writeClaudeDirsConfig(t, home,
		map[string]string{"work": ".claude-work"},
		map[string]string{"claude-work": ".claude-work"})

	inherited := claudeSession("inherited", "via-group", "work")
	pinned := claudeSession("pinned", "via-slot", "my-sessions")
	pinned.Account = "claude-work"
	h := homeWithSessions(t, 240, inherited, pinned)

	require.Contains(t, renderRow(t, h, inherited, false), "[work]")
	require.Contains(t, renderRow(t, h, pinned, false), "[work]")
	require.Equal(t,
		h.getSessionRenderState(inherited).accountDisplay,
		h.getSessionRenderState(pinned).accountDisplay,
		"same config dir must produce the same presentation whatever selected it")
}

// The badge is about the session's environment, not its tool. agent-deck
// exports the resolved CLAUDE_CONFIG_DIR into every session it starts, so a
// shell opened in a work group is on the work account as soon as anyone runs
// claude in it, and upstream's own SSH lifecycle test asserts the account is
// visible on exactly such a row.
func TestBadgeShownForAnyToolOnNonDefaultDir(t *testing.T) {
	home := isolatedConfigHome(t)
	writeClaudeDirsConfig(t, home, map[string]string{"work": ".claude-work"}, nil)

	for _, tool := range []string{"shell", "claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			inst := session.NewInstanceWithGroupAndTool("row-"+tool, "/tmp/"+tool, "work", tool)
			inst.ID = "id-" + tool
			h := homeWithSessions(t, 240, inst)

			require.Equal(t, "work", h.getSessionRenderState(inst).accountDisplay.label)
			require.Contains(t, renderRow(t, h, inst, false), "[work]")
		})
	}
}

// The presentation is a pure function of the label; pin it so a future refactor
// cannot reintroduce a zero-width badge that still reserves prefix width.
func TestAccountPresentationFromLabel(t *testing.T) {
	hidden := newAccountPresentation("")
	require.Equal(t, accountPresentation{}, hidden)
	badge, width := hidden.fit(0)
	require.Empty(t, badge)
	require.Zero(t, width)

	shown := newAccountPresentation("work")
	require.Equal(t, "work", shown.label)
	require.Equal(t, " [work]", shown.badge)
	badge, width = shown.fit(shown.width)
	require.Equal(t, " [work]", badge)
	require.Equal(t, shown.width, width)

	// Too narrow for any label keeps the row clean instead of emitting "[]".
	badge, width = shown.fit(3)
	require.Empty(t, badge)
	require.Zero(t, width)
}
