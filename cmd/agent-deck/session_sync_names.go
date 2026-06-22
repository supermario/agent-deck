package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// syncClaudeDisplayNames sends /rename to Claude sessions whose display
// name doesn't match the agent-deck title.
func syncClaudeDisplayNames(out *CLIOutput, instances []*session.Instance) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	claudeDir := filepath.Join(home, ".claude")

	var claudeInstances []*session.Instance
	for _, inst := range instances {
		if session.IsClaudeCompatible(inst.Tool) {
			claudeInstances = append(claudeInstances, inst)
		}
	}
	if len(claudeInstances) == 0 {
		return
	}

	// Wait for session metadata files to be written by new Claude processes
	time.Sleep(3 * time.Second)

	renamed := 0
	for _, inst := range claudeInstances {
		if inst.ClaudeSessionID == "" {
			continue
		}
		currentName := findClaudeSessionName(claudeDir, inst.ClaudeSessionID)
		if currentName == inst.Title {
			continue
		}

		tmuxSess := inst.GetTmuxSession()
		if tmuxSess == nil || !tmuxSess.Exists() {
			continue
		}

		if _, err := executeSend(tmuxSess, inst.Tool, "/rename "+inst.Title, true, noWaitSendTuning()); err != nil {
			if !out.jsonMode && !out.quietMode {
				fmt.Fprintf(os.Stderr, "  Failed to rename %s: %v\n", inst.Title, err)
			}
			continue
		}
		renamed++
		if !out.jsonMode && !out.quietMode {
			fmt.Printf("  Renamed: %s\n", inst.Title)
		}
	}

	if renamed > 0 && !out.jsonMode && !out.quietMode {
		fmt.Printf("Set display name for %d sessions\n", renamed)
	}
}
