package web

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModelDisplayName(t *testing.T) {
	cases := map[string]string{
		"claude-opus-5":              "Opus 5",
		"claude-opus-5[1m]":          "Opus 5",
		"claude-fable-5-1":           "Fable 5.1",
		"claude-opus-4-8":            "Opus 4.8",
		"claude-haiku-4-5-20251001":  "Haiku 4.5",
		"claude-3-5-sonnet-20241022": "Sonnet 3.5",
		"<synthetic>":                "",
		"":                           "",
	}
	for id, want := range cases {
		if got := modelDisplayName(id); got != want {
			t.Errorf("modelDisplayName(%q) = %q, want %q", id, got, want)
		}
	}
}

const (
	lineAssistantA            = `{"type":"assistant","isSidechain":false,"message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":2,"cache_creation_input_tokens":1000,"cache_read_input_tokens":100000,"output_tokens":900}}}`
	lineAssistantB            = `{"type":"assistant","isSidechain":false,"message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":5,"cache_creation_input_tokens":200,"cache_read_input_tokens":150000,"output_tokens":10}}}`
	lineSidechain             = `{"type":"assistant","isSidechain":true,"message":{"role":"assistant","model":"claude-haiku-4-5-20251001","content":[],"usage":{"input_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":900000,"output_tokens":1}}}`
	lineSynthetic             = `{"type":"assistant","isSidechain":false,"message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"Interrupted"}],"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}`
	lineCostOneM              = `{"type":"cost-state","modelUsage":{"claude-opus-5[1m]":{"inputTokens":2},"claude-haiku-4-5-20251001":{"inputTokens":1}}}`
	lineUserMentionsAssistant = `{"type":"user","message":{"role":"user","content":"the \"assistant\" said so"}}`
)

func TestTranscriptMetaObserve(t *testing.T) {
	var m transcriptMeta
	for _, l := range []string{lineAssistantA, lineAssistantB, lineSidechain, lineSynthetic, lineUserMentionsAssistant} {
		m.observe([]byte(l))
	}
	// Output tokens excluded, to match Claude's statusline used_percentage.
	if m.ContextTokens != 5+200+150000 {
		t.Errorf("ContextTokens = %d, want the LAST real main-chain call (150205)", m.ContextTokens)
	}
	if m.ModelID != "claude-opus-5" {
		t.Errorf("ModelID = %q: a subagent or synthetic record must not replace the real model", m.ModelID)
	}
	if m.OneMSeen {
		t.Error("OneMSeen set without any cost-state record")
	}
	m.observe([]byte(lineCostOneM))
	if !m.OneMSeen {
		t.Error("a [1m] cost-state key must set OneMSeen")
	}
}

func TestResolveContextWindow(t *testing.T) {
	small := transcriptMeta{ModelID: "claude-opus-5", ContextTokens: 50_000}
	cases := []struct {
		name     string
		meta     transcriptMeta
		settings string
		want     int64
	}{
		{"past 200K cannot be a 200K window", transcriptMeta{ModelID: "claude-opus-5", ContextTokens: 624_000}, "", contextWindowOneM},
		{"cost-state [1m] evidence", transcriptMeta{ModelID: "claude-opus-5", ContextTokens: 50_000, OneMSeen: true}, "", contextWindowOneM},
		{"settings [1m] for the family in use", small, "opus[1m]", contextWindowOneM},
		{"settings [1m] for a different family", transcriptMeta{ModelID: "claude-fable-5-1", ContextTokens: 50_000}, "opus[1m]", contextWindowStandard},
		{"settings without [1m]", small, "opus", contextWindowStandard},
		{"no evidence at all", small, "", contextWindowStandard},
	}
	for _, c := range cases {
		if got := resolveContextWindow(c.meta, c.settings); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

// Polling a live session appends to the transcript; the latest usage must come
// through the incremental cache path, not only a cold full parse.
func TestCachedTranscriptMetaFollowsAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(lineAssistantA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		transcriptCacheMu.Lock()
		delete(transcriptCache, path)
		transcriptCacheMu.Unlock()
	})

	if _, meta := cachedTranscript(path, "claude"); meta.ContextTokens != 2+1000+100000 {
		t.Fatalf("cold parse: ContextTokens = %d", meta.ContextTokens)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(lineCostOneM + "\n" + lineAssistantB + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	turns, meta := cachedTranscript(path, "claude")
	if meta.ContextTokens != 5+200+150000 {
		t.Errorf("after append: ContextTokens = %d, want 150205", meta.ContextTokens)
	}
	if !meta.OneMSeen {
		t.Error("after append: OneMSeen lost through the incremental path")
	}
	if len(turns) != 2 {
		t.Errorf("turns = %d, want 2", len(turns))
	}
}
