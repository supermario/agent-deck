package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withClaudeConfig points the config loader at an isolated HOME and writes cfg
// there, mirroring the recipe the UI tests use. Returns that HOME so a test can
// build the paths it expects the resolver to produce.
func withClaudeConfig(t *testing.T, build func(cfg *UserConfig, home string)) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)
	if err := os.MkdirAll(filepath.Join(home, ".agent-deck"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	cfg := &UserConfig{}
	build(cfg, home)
	if err := SaveUserConfig(cfg); err != nil {
		t.Fatalf("SaveUserConfig: %v", err)
	}
	ClearUserConfigCache()
	return home
}

// A session on the ordinary ~/.claude gets no label, which is what keeps the
// badge off the common row.
func TestNonDefaultClaudeConfigLabel_DefaultDirIsUnlabelled(t *testing.T) {
	withClaudeConfig(t, func(cfg *UserConfig, home string) {})

	for _, group := range []string{"", "work", "my-sessions"} {
		if label := NonDefaultClaudeConfigLabel("", group, "session"); label != "" {
			t.Errorf("group %q: got label %q, want none for the default config dir", group, label)
		}
	}
}

// The case the badge exists for: a group override, which carries no stored
// account slot at all, must still be labelled.
func TestNonDefaultClaudeConfigLabel_GroupOverride(t *testing.T) {
	home := withClaudeConfig(t, func(cfg *UserConfig, home string) {
		cfg.Groups = map[string]GroupSettings{
			"work": {Claude: GroupClaudeSettings{ConfigDir: filepath.Join(home, ".claude-work")}},
		}
	})
	_ = home

	if got, want := NonDefaultClaudeConfigLabel("", "work", "atlas"), "work"; got != want {
		t.Errorf("group override label = %q, want %q", got, want)
	}
	if got := NonDefaultClaudeConfigLabel("", "personal", "atlas"); got != "" {
		t.Errorf("unconfigured group label = %q, want none", got)
	}
}

// A stored slot resolves through [profiles.<name>.claude] and must produce the
// same label as the group override pointing at the same dir, so two sessions on
// one account never read as two different accounts.
func TestNonDefaultClaudeConfigLabel_StoredSlotMatchesGroupOverride(t *testing.T) {
	withClaudeConfig(t, func(cfg *UserConfig, home string) {
		dir := filepath.Join(home, ".claude-work")
		cfg.Profiles = map[string]ProfileSettings{
			"claude-work": {Claude: ProfileClaudeSettings{ConfigDir: dir}},
		}
		cfg.Groups = map[string]GroupSettings{
			"work": {Claude: GroupClaudeSettings{ConfigDir: dir}},
		}
	})

	slot := NonDefaultClaudeConfigLabel("claude-work", "my-sessions", "pinned")
	group := NonDefaultClaudeConfigLabel("", "work", "inherited")
	if slot != "work" || group != "work" {
		t.Errorf("slot=%q group=%q, want both %q", slot, group, "work")
	}
}

// A conductor block is a third way to land on another dir, and it resolves off
// the title. Pinning it keeps the title argument honest.
func TestNonDefaultClaudeConfigLabel_ConductorBlock(t *testing.T) {
	withClaudeConfig(t, func(cfg *UserConfig, home string) {
		cfg.Conductors = map[string]ConductorOverrides{
			"gsd": {Claude: ConductorClaudeSettings{ConfigDir: filepath.Join(home, ".claude-team")}},
		}
	})

	if got, want := NonDefaultClaudeConfigLabel("", "my-sessions", "conductor-gsd"), "team"; got != want {
		t.Errorf("conductor label = %q, want %q", got, want)
	}
	if got := NonDefaultClaudeConfigLabel("", "my-sessions", "gsd"); got != "" {
		t.Errorf("non-conductor title label = %q, want none", got)
	}
}

// Configuring the default dir explicitly is still the default dir. The label
// must compare resolved paths, not the resolver's source string, or a user with
// a [claude] config_dir = "~/.claude" line would get a badge on every row.
func TestNonDefaultClaudeConfigLabel_ExplicitlyConfiguredDefaultIsStillDefault(t *testing.T) {
	withClaudeConfig(t, func(cfg *UserConfig, home string) {
		cfg.Claude.ConfigDir = filepath.Join(home, ".claude")
	})

	_, source := ResolvedClaudeConfigDirFor("", "my-sessions", "s")
	if source == "default" {
		t.Fatalf("fixture did not take effect: source = %q, want a configured source", source)
	}
	if label := NonDefaultClaudeConfigLabel("", "my-sessions", "s"); label != "" {
		t.Errorf("label = %q, want none: the configured dir IS the default", label)
	}
}

// A config dir is user-supplied text that reaches a terminal through a row.
func TestNonDefaultClaudeConfigLabel_StripsControlCharacters(t *testing.T) {
	withClaudeConfig(t, func(cfg *UserConfig, home string) {
		cfg.Groups = map[string]GroupSettings{
			"work": {Claude: GroupClaudeSettings{ConfigDir: filepath.Join(home, ".claude-\x1b]0;bad\a")}},
		}
	})

	label := NonDefaultClaudeConfigLabel("", "work", "s")
	if label == "" {
		t.Fatal("expected a label for a non-default dir")
	}
	for _, bad := range []string{"\x1b", "\a", "\r", "\n"} {
		if strings.Contains(label, bad) {
			t.Errorf("label %q still carries %q", label, bad)
		}
	}
}

// The values-based entry point exists so the TUI never has to read GroupPath off
// a live Instance. It is only safe to use if it resolves identically.
func TestResolvedClaudeConfigDirFor_AgreesWithInstanceResolution(t *testing.T) {
	withClaudeConfig(t, func(cfg *UserConfig, home string) {
		cfg.Profiles = map[string]ProfileSettings{
			"claude-work": {Claude: ProfileClaudeSettings{ConfigDir: filepath.Join(home, ".claude-work")}},
		}
		cfg.Groups = map[string]GroupSettings{
			"work": {Claude: GroupClaudeSettings{ConfigDir: filepath.Join(home, ".claude-group")}},
		}
		cfg.Conductors = map[string]ConductorOverrides{
			"gsd": {Claude: ConductorClaudeSettings{ConfigDir: filepath.Join(home, ".claude-team")}},
		}
	})

	for _, tc := range []struct{ account, group, title string }{
		{"", "", ""},
		{"", "work", "atlas"},
		{"", "my-sessions", "conductor-gsd"},
		{"claude-work", "work", "pinned"},
		{"claude-work", "my-sessions", "conductor-gsd"},
		{"missing-slot", "work", "atlas"},
	} {
		wantPath, wantSource := GetClaudeConfigDirSourceForInstance(&Instance{
			Account: tc.account, GroupPath: tc.group, Title: tc.title,
		})
		gotPath, gotSource := ResolvedClaudeConfigDirFor(tc.account, tc.group, tc.title)
		if gotPath != wantPath || gotSource != wantSource {
			t.Errorf("account=%q group=%q title=%q: got (%q, %q), want (%q, %q)",
				tc.account, tc.group, tc.title, gotPath, gotSource, wantPath, wantSource)
		}
	}
}
