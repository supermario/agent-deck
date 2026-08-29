package web

import "testing"

func TestParseCodexTurnLine(t *testing.T) {
	turn, ok := parseCodexTurnLine([]byte(`{"timestamp":"2026-08-23T12:00:00.123Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Codex reply"}]}}`))
	if !ok {
		t.Fatal("expected Codex assistant message to be parsed")
	}
	if turn.Role != "assistant" || turn.Text != "Codex reply" || turn.TsMs == 0 {
		t.Fatalf("unexpected turn: %+v", turn)
	}
}

func TestParseCodexTurnLineSkipsDeveloperMessages(t *testing.T) {
	_, ok := parseCodexTurnLine([]byte(`{"timestamp":"2026-08-23T12:00:00Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"private instruction"}]}}`))
	if ok {
		t.Fatal("developer message must not be shown in the mobile transcript")
	}
}
