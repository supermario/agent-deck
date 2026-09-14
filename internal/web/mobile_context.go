package web

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// Model and context-fullness readout for the phone ("Opus 5 1M 46%"), derived
// from the transcript so it works for a session that died two weeks ago, not
// only one with a live pane to scrape.

const (
	contextWindowStandard int64 = 200_000
	contextWindowOneM     int64 = 1_000_000

	// Claude writes model "<synthetic>" for locally generated assistant records
	// (interrupts, errors). They carry no real usage, so taking one as "the
	// latest call" would report an empty context.
	syntheticModel = "<synthetic>"
)

// transcriptMeta is what the transcript says about the latest real model call.
type transcriptMeta struct {
	// ModelID is the raw id of the last main-chain assistant call, e.g.
	// "claude-opus-5". It never carries the [1m] suffix, even for a 1M session.
	ModelID string
	// ContextTokens is input + cache_creation + cache_read for that call. Output
	// is deliberately excluded: this matches Claude's own statusline
	// used_percentage, checked against nine live panes to the rounded percent.
	ContextTokens int64
	// OneMSeen is set when a cost-state record keyed a model with the [1m]
	// suffix. Sufficient evidence of a 1M window, but not necessary: sessions
	// with plain keys have been observed running well past 200K.
	OneMSeen bool
}

// observe folds one complete transcript line into m. Later assistant calls
// replace earlier ones; OneMSeen is sticky.
func (m *transcriptMeta) observe(line []byte) {
	// Cheap reject before a second JSON decode per line: only two record types
	// matter here, and most of a transcript is neither.
	if !bytes.Contains(line, []byte(`"assistant"`)) && !bytes.Contains(line, []byte(`"cost-state"`)) {
		return
	}
	var rec struct {
		Type        string                     `json:"type"`
		IsSidechain bool                       `json:"isSidechain"`
		ModelUsage  map[string]json.RawMessage `json:"modelUsage"`
		Message     struct {
			Model string `json:"model"`
			Usage *struct {
				InputTokens              int64 `json:"input_tokens"`
				CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return
	}
	switch rec.Type {
	case "cost-state":
		for key := range rec.ModelUsage {
			if hasOneMSuffix(key) {
				m.OneMSeen = true
			}
		}
	case "assistant":
		// Sidechains are subagents with their own separate context.
		if rec.IsSidechain || rec.Message.Usage == nil {
			return
		}
		model := strings.TrimSpace(rec.Message.Model)
		if model == "" || model == syntheticModel {
			return
		}
		u := rec.Message.Usage
		total := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
		if total == 0 {
			return
		}
		m.ModelID = model
		m.ContextTokens = total
	}
}

func hasOneMSuffix(model string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(model)), "[1m]")
}

var modelDateSuffix = regexp.MustCompile(`-\d{8}$`)

// modelTokens splits a model id or alias into its family word(s) and version
// numbers: "claude-haiku-4-5-20251001" -> ("haiku", ["4","5"]),
// "claude-3-5-sonnet" -> ("sonnet", ["3","5"]), "opus[1m]" -> ("opus", nil).
func modelTokens(id string) (family string, version []string) {
	id = strings.ToLower(strings.TrimSpace(id))
	if i := strings.IndexByte(id, '['); i >= 0 {
		id = id[:i]
	}
	id = strings.TrimPrefix(id, "claude-")
	id = modelDateSuffix.ReplaceAllString(id, "")
	for _, part := range strings.Split(id, "-") {
		switch {
		case part == "":
		case strings.Trim(part, "0123456789") == "":
			version = append(version, part)
		case family == "":
			family = part
		default:
			family += " " + part
		}
	}
	return family, version
}

// modelDisplayName turns a model id into the short name people say:
// "claude-opus-5" -> "Opus 5", "claude-fable-5-1" -> "Fable 5.1".
func modelDisplayName(id string) string {
	if strings.TrimSpace(id) == syntheticModel {
		return ""
	}
	family, version := modelTokens(id)
	if family == "" {
		return strings.Join(version, ".")
	}
	name := strings.ToUpper(family[:1]) + family[1:]
	if len(version) > 0 {
		name += " " + strings.Join(version, ".")
	}
	return name
}

// resolveContextWindow picks 1M or 200K. The transcript's model id never says
// which, so this weighs the evidence available:
//
//   - a context already past 200K cannot be a 200K window;
//   - a cost-state record keyed "[1m]" is direct evidence;
//   - the config dir's settings model ("opus[1m]") applies only when it names
//     the same family as the model actually used, since /model or a --model
//     launch flag can put a session on something else entirely.
//
// Otherwise 200K. The known gap: a 1M session launched with an explicit
// non-[1m] --model while settings say [1m] reads as 1M until proven otherwise.
func resolveContextWindow(meta transcriptMeta, settingsModel string) int64 {
	if meta.ContextTokens > contextWindowStandard || meta.OneMSeen {
		return contextWindowOneM
	}
	if hasOneMSuffix(settingsModel) {
		settingsFamily, _ := modelTokens(settingsModel)
		usedFamily, _ := modelTokens(meta.ModelID)
		if settingsFamily != "" && settingsFamily == usedFamily {
			return contextWindowOneM
		}
	}
	return contextWindowStandard
}

// claudeSettingsModel reads the "model" key from the instance's Claude
// settings.json. Unlike .claude.json, settings.json does live inside the config
// dir (~/.claude/settings.json for the default).
func claudeSettingsModel(inst *session.Instance) string {
	if inst == nil || !session.IsClaudeCompatible(inst.Tool) {
		return ""
	}
	dir := session.GetClaudeConfigDirForInstance(inst)
	if dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return ""
	}
	var settings struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return ""
	}
	return settings.Model
}
