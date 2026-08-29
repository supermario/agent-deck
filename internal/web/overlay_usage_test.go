package web

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadUsageIncludesCodexAndKeepsClaudeAccounts(t *testing.T) {
	dir := t.TempDir()
	oldDir := overlayUsageDir
	overlayUsageDir = dir
	t.Cleanup(func() { overlayUsageDir = oldDir })

	now := time.Now().Unix()
	for _, file := range []struct {
		name string
		body string
	}{
		{"claude-usage-personal.json", fmt.Sprintf(`{"account":"personal","seven_day":{"used_percentage":11,"resets_at":100},"captured_at":%d}`, now)},
		{"claude-usage-work.json", fmt.Sprintf(`{"account":"work","five_hour":{"used_percentage":22,"resets_at":200},"seven_day":{"used_percentage":33,"resets_at":300},"captured_at":%d}`, now)},
		{"codex-usage.json", fmt.Sprintf(`{"account":"codex","seven_day":{"used_percentage":44,"resets_at":400},"captured_at":%d}`, now)},
	} {
		if err := os.WriteFile(filepath.Join(dir, file.name), []byte(file.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got := readUsage()
	if len(got) != 3 {
		t.Fatalf("readUsage() returned %d accounts, want 3: %#v", len(got), got)
	}
	if got["codex"].SevenDayPct != 44 || got["codex"].FiveHourPct != nil {
		t.Fatalf("Codex usage = %#v, want weekly-only 44%%", got["codex"])
	}
	if got["personal"].SevenDayPct != 11 || got["work"].SevenDayPct != 33 {
		t.Fatalf("Claude usage was not preserved: %#v", got)
	}
}

func TestMapToolNameKeepsCodexForTheOverlay(t *testing.T) {
	if got := mapToolName("codex"); got != "codex" {
		t.Fatalf("mapToolName(codex) = %q, want codex", got)
	}
	if got := mapToolName("claude"); got != "claude-code" {
		t.Fatalf("mapToolName(claude) = %q, want claude-code", got)
	}
}
