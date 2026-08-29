package ui

import (
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// The "by last active" view (GroupViewRecentFlat) sorts on Instance.lastTurnAt,
// an in-memory cache that is never persisted. A storage reload replaces every
// *Instance with a freshly-loaded one, which used to drop the cache for the
// whole fleet: the sort silently fell back to the badge formula, the list
// re-ordered, and it only snapped back on the next 2s tick. Attaching to a
// session writes to storage, so this fired on every ctrl+q back to the list.
func TestCarryLastTurnAtSurvivesReload(t *testing.T) {
	now := time.Now()

	prev := []*session.Instance{
		session.NewInstanceWithGroupAndTool("older", "/tmp/a", "g", "claude"),
		session.NewInstanceWithGroupAndTool("newer", "/tmp/b", "g", "claude"),
	}
	prev[0].ID = "older"
	prev[1].ID = "newer"
	prev[0].SetLastTurnAt(now.Add(-2 * time.Hour))
	prev[1].SetLastTurnAt(now.Add(-5 * time.Minute))

	// A reload hands back brand-new instances with the same IDs and a cold cache.
	next := []*session.Instance{
		session.NewInstanceWithGroupAndTool("older", "/tmp/a", "g", "claude"),
		session.NewInstanceWithGroupAndTool("newer", "/tmp/b", "g", "claude"),
	}
	next[0].ID = "older"
	next[1].ID = "newer"

	for i, inst := range next {
		if !inst.GetLastTurnAt().IsZero() {
			t.Fatalf("precondition: next[%d] should start with a cold cache", i)
		}
	}

	carryLastTurnAt(prev, next)

	if got := next[0].GetLastTurnAt(); !got.Equal(prev[0].GetLastTurnAt()) {
		t.Errorf("older: last-turn not carried across reload: got %v want %v", got, prev[0].GetLastTurnAt())
	}
	if got := next[1].GetLastTurnAt(); !got.Equal(prev[1].GetLastTurnAt()) {
		t.Errorf("newer: last-turn not carried across reload: got %v want %v", got, prev[1].GetLastTurnAt())
	}
	// The ordering the view depends on must be preserved, not just the values.
	if !next[1].GetLastTurnAt().After(next[0].GetLastTurnAt()) {
		t.Error("recency order lost across reload: 'newer' no longer sorts above 'older'")
	}
}

// A session absent from the previous fleet (created while we were attached) must
// simply stay cold rather than inherit anything.
func TestCarryLastTurnAtIgnoresUnknownAndZero(t *testing.T) {
	prev := []*session.Instance{
		session.NewInstanceWithGroupAndTool("known", "/tmp/a", "g", "claude"),
		session.NewInstanceWithGroupAndTool("nevercached", "/tmp/c", "g", "claude"),
	}
	prev[0].ID = "known"
	prev[1].ID = "nevercached" // deliberately left zero
	prev[0].SetLastTurnAt(time.Now().Add(-time.Minute))

	next := []*session.Instance{
		session.NewInstanceWithGroupAndTool("known", "/tmp/a", "g", "claude"),
		session.NewInstanceWithGroupAndTool("brandnew", "/tmp/d", "g", "claude"),
		session.NewInstanceWithGroupAndTool("nevercached", "/tmp/c", "g", "claude"),
	}
	next[0].ID = "known"
	next[1].ID = "brandnew"
	next[2].ID = "nevercached"

	carryLastTurnAt(prev, next)

	if next[0].GetLastTurnAt().IsZero() {
		t.Error("known session should have kept its cached timestamp")
	}
	if !next[1].GetLastTurnAt().IsZero() {
		t.Error("session unknown to the previous fleet must stay cold")
	}
	if !next[2].GetLastTurnAt().IsZero() {
		t.Error("a zero cached value must not be carried")
	}
}

// Guards the empty-slice early return (first load has no previous fleet).
func TestCarryLastTurnAtEmptyInputs(t *testing.T) {
	inst := session.NewInstanceWithGroupAndTool("x", "/tmp/x", "g", "claude")
	inst.ID = "x"
	carryLastTurnAt(nil, []*session.Instance{inst})
	carryLastTurnAt([]*session.Instance{inst}, nil)
	if !inst.GetLastTurnAt().IsZero() {
		t.Error("no-op calls must not mutate anything")
	}
}
