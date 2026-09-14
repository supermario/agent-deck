package session

import (
	"os/exec"
	"testing"
	"time"
)

// The delivery contract behind "just send this message": a dead session comes
// back, and a live one is left strictly alone.
//
// Uses the shell tool rather than claude so the test spawns a plain shell — the
// behaviour under test is tmux revival, not anything agent-specific, and this
// keeps the test from launching a real agent.
func TestEnsureRunningRevivesThenLeavesAlone(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	if testing.Short() {
		t.Skip("spawns a real tmux session; skipped in short mode")
	}

	inst := NewInstanceWithTool("ensure-running-revive", t.TempDir(), "shell")
	inst.ID = "ensure-running-revive"
	t.Cleanup(func() { _ = inst.Kill() })

	if inst.Exists() {
		t.Fatal("precondition: a never-started session must not already exist")
	}

	// 1. Dead -> revived. This is the case that used to be a 409 on the phone
	//    and an "is not running" exit on the CLI.
	restarted, err := inst.EnsureRunning(15 * time.Second)
	if err != nil {
		t.Fatalf("EnsureRunning on a dead session: %v", err)
	}
	if !restarted {
		t.Error("a dead session should report that it was restarted")
	}
	if !inst.Exists() {
		t.Fatal("the session should be running after EnsureRunning")
	}

	// 2. Already live -> untouched. Restarting a running session would
	//    interrupt whatever the agent is mid-way through, which is worse than
	//    the problem this solves.
	name := inst.GetTmuxSession().Name
	restarted, err = inst.EnsureRunning(15 * time.Second)
	if err != nil {
		t.Fatalf("EnsureRunning on a live session: %v", err)
	}
	if restarted {
		t.Error("a live session must not be restarted")
	}
	if got := inst.GetTmuxSession().Name; got != name {
		t.Errorf("live session was replaced: had %q, now %q", name, got)
	}
	if !inst.Exists() {
		t.Error("the session should still be running")
	}
}
