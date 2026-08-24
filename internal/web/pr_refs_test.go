package web

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTranscript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestScanPRsFiltersAndDedupes(t *testing.T) {
	path := writeTranscript(t, `
{"text":"opened https://github.com/locomote/warehouse/pull/42 for review"}
{"text":"see also https://github.com/locomote/mid_office_system/pull/1234"}
{"text":"upstream fix in https://github.com/rails/rails/pull/999 (not mine)"}
{"text":"personal repo https://github.com/supermario/habitat/pull/500 (now excluded)"}
{"text":"mentioned https://github.com/locomote/warehouse/pull/42 again"}
{"text":"push output: https://github.com/locomote/cbt/pull/new/some-branch"}
`)

	o := newOverlayPusher(nil)
	got := o.scanPRs("sess-1", path)

	if len(got) != 2 {
		t.Fatalf("got %d refs, want 2: %+v", len(got), got)
	}
	if got[0].Number != 42 || got[0].Repo != "locomote/warehouse" {
		t.Errorf("first ref = %+v", got[0])
	}
	if got[1].Number != 1234 || got[1].Repo != "locomote/mid_office_system" {
		t.Errorf("second ref = %+v", got[1])
	}
	for _, r := range got {
		if r.Repo == "rails/rails" || r.Repo == "supermario/habitat" {
			t.Errorf("PR from a non-allowlisted owner leaked through: %s", r.Repo)
		}
		if r.Number == 0 {
			t.Errorf("/pull/new/<branch> matched as a PR: %+v", r)
		}
	}
}

// The scan is incremental: only bytes appended since last time are read. A PR
// URL landing across that boundary must still be found, which is what the
// overlap exists for.
func TestScanPRsIncrementalAcrossBoundary(t *testing.T) {
	path := writeTranscript(t, `{"text":"nothing yet"}`+"\n")
	o := newOverlayPusher(nil)

	if got := o.scanPRs("sess-2", path); len(got) != 0 {
		t.Fatalf("initial scan found %+v, want none", got)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString(`{"text":"now https://github.com/locomote/cbt/pull/7"}` + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = f.Close()

	got := o.scanPRs("sess-2", path)
	if len(got) != 1 || got[0].Number != 7 {
		t.Fatalf("after append got %+v, want PR 7", got)
	}

	// A scan with nothing appended must return the same refs, not lose them.
	if again := o.scanPRs("sess-2", path); len(again) != 1 || again[0].Number != 7 {
		t.Fatalf("no-op scan returned %+v, want PR 7 retained", again)
	}
}

// A truncated/replaced transcript must reset rather than keep a stale offset
// that would skip everything before it.
func TestScanPRsHandlesTruncation(t *testing.T) {
	path := writeTranscript(t, `{"text":"https://github.com/locomote/cbt/pull/11"}`+"\n")
	o := newOverlayPusher(nil)
	if got := o.scanPRs("sess-3", path); len(got) != 1 {
		t.Fatalf("first scan got %+v, want 1", got)
	}

	if err := os.WriteFile(path, []byte(`{"text":"https://github.com/locomote/cbt/pull/22"}`+"\n"), 0o600); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	got := o.scanPRs("sess-3", path)
	if len(got) != 1 || got[0].Number != 22 {
		t.Fatalf("after truncation got %+v, want only PR 22", got)
	}
}

// The launcher session dispatches work across every project, so its history
// accumulates PR links that say nothing about what it is for.
func TestPRSessionExclusion(t *testing.T) {
	for _, name := range []string{"launcher", "Launcher", "  launcher  "} {
		if !prSessionExcluded(name) {
			t.Errorf("prSessionExcluded(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"mos-rec", "launcher-fork", "", "lamdera-martin"} {
		if prSessionExcluded(name) {
			t.Errorf("prSessionExcluded(%q) = true, want false", name)
		}
	}
}
