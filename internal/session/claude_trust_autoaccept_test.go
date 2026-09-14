package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readTrusted(t *testing.T, path, dir string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	projects, _ := cfg["projects"].(map[string]any)
	entry, _ := projects[dir].(map[string]any)
	return entry["hasTrustDialogAccepted"] == true
}

// With no explicit config dir, Claude reads ~/.claude.json in HOME — NOT
// ~/.claude/.claude.json. Writing to the latter is silent: the file appears,
// the prompt still blocks the session. This pins the default resolution.
func TestClaudeTrustFilePath_DefaultsToHomeNotDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	inst := NewInstanceWithTool("trust-default", t.TempDir(), "claude")
	got := claudeTrustFilePath(inst)

	if want := filepath.Join(home, ".claude.json"); got != want {
		t.Errorf("default trust file: got %q want %q", got, want)
	}
	if got == filepath.Join(home, ".claude", ".claude.json") {
		t.Error("resolved to the data dir, where Claude never looks")
	}
}

// EnsureClaudeFolderTrust must actually mark the session's working directory
// trusted, so the spawn that follows does not stop on the dialog.
func TestEnsureClaudeFolderTrust_MarksWorkingDirTrusted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()

	inst := NewInstanceWithTool("trust-write", work, "claude")
	if err := EnsureClaudeFolderTrust(inst); err != nil {
		t.Fatalf("EnsureClaudeFolderTrust: %v", err)
	}

	trustFile := filepath.Join(home, ".claude.json")
	if !readTrusted(t, trustFile, inst.EffectiveWorkingDir()) {
		t.Errorf("working dir %q not marked trusted in %s", inst.EffectiveWorkingDir(), trustFile)
	}
}

// Pre-seeding trust must never clobber the rest of a real ~/.claude.json —
// it holds MCP servers, onboarding state and every other project's entry.
func TestEnsureClaudeFolderTrust_PreservesExistingConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()

	trustFile := filepath.Join(home, ".claude.json")
	original := `{"numStartups":42,"projects":{"/other/project":{"hasTrustDialogAccepted":true}}}`
	if err := os.WriteFile(trustFile, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	inst := NewInstanceWithTool("trust-merge", work, "claude")
	if err := EnsureClaudeFolderTrust(inst); err != nil {
		t.Fatalf("EnsureClaudeFolderTrust: %v", err)
	}

	data, _ := os.ReadFile(trustFile)
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if cfg["numStartups"] != float64(42) {
		t.Errorf("top-level field lost: numStartups=%v", cfg["numStartups"])
	}
	if !readTrusted(t, trustFile, "/other/project") {
		t.Error("an unrelated project's trust entry was dropped")
	}
	if !readTrusted(t, trustFile, inst.EffectiveWorkingDir()) {
		t.Error("the new working dir was not trusted")
	}
}

// Non-Claude tools have no such prompt, so touching Claude's config for them
// would be writing to an unrelated tool's state.
func TestEnsureClaudeFolderTrust_SkipsNonClaudeTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	inst := NewInstanceWithTool("trust-shell", t.TempDir(), "shell")
	if err := EnsureClaudeFolderTrust(inst); err != nil {
		t.Fatalf("expected a silent no-op, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Error("a shell session must not create Claude's config file")
	}
}

// macOS resolves /var to /private/var, so a session started in /var/folders/...
// is reported by Claude under /private/var/folders/... Seeding only the
// unresolved spelling wrote an entry Claude never reads: the prompt still
// blocked the pane while the config looked correct. Verified against a real
// launch before this was fixed.
func TestEnsureClaudeFolderTrust_SeedsSymlinkResolvedPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	realDir := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	inst := NewInstanceWithTool("trust-symlink", linkDir, "claude")
	if err := EnsureClaudeFolderTrust(inst); err != nil {
		t.Fatalf("EnsureClaudeFolderTrust: %v", err)
	}

	trustFile := filepath.Join(home, ".claude.json")
	resolved, err := filepath.EvalSymlinks(linkDir)
	if err != nil {
		t.Fatal(err)
	}
	if !readTrusted(t, trustFile, resolved) {
		t.Errorf("resolved path %q not trusted — Claude would still prompt", resolved)
	}
	if !readTrusted(t, trustFile, inst.EffectiveWorkingDir()) {
		t.Errorf("unresolved path %q not trusted", inst.EffectiveWorkingDir())
	}
}
