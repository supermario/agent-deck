package ui

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// Editing config.toml changes which dir a session resolves to, so a settings
// save has to re-resolve labels and refresh the already-published rows. Without
// this the badge would only catch up on the next list rebuild.
func TestConfigChangeRefreshesPublishedLabels(t *testing.T) {
	home := isolatedConfigHome(t)
	writeClaudeDirsConfig(t, home, nil, nil)

	inst := claudeSession("work-session", "atlas-checker", "work")
	h := homeWithSessions(t, 240, inst)
	require.Equal(t, accountPresentation{}, h.getSessionRenderState(inst).accountDisplay,
		"precondition: no group override yet, so no badge")

	// The user adds [groups."work".claude] config_dir and saves settings.
	writeClaudeDirsConfig(t, home, map[string]string{"work": ".claude-work"}, nil)
	h.refreshAccountLabelsAfterConfigChange()

	require.Equal(t, "work", h.getSessionRenderState(inst).accountDisplay.label,
		"the published snapshot must pick up the new dir")
	require.Contains(t, renderRow(t, h, inst, false), "[work]")
	require.Contains(t, h.renderSessionInfoCard(inst, 240, 40), "Claude config:")

	// Removing it again takes the badge away.
	writeClaudeDirsConfig(t, home, nil, nil)
	h.refreshAccountLabelsAfterConfigChange()
	require.Equal(t, accountPresentation{}, h.getSessionRenderState(inst).accountDisplay)
	require.NotContains(t, renderRow(t, h, inst, false), "[work]")
}

// A save that changes no label must not rebuild the snapshot: active renderers
// hold it, and churning it costs a copy of every row for nothing.
func TestConfigChangeWithoutLabelChangeKeepsSnapshot(t *testing.T) {
	home := isolatedConfigHome(t)
	writeClaudeDirsConfig(t, home, map[string]string{"work": ".claude-work"}, nil)

	inst := claudeSession("work-session", "atlas-checker", "work")
	h := homeWithSessions(t, 240, inst)
	before := h.getSessionRenderSnapshot()
	require.Equal(t, "work", before[inst.ID].accountDisplay.label)

	h.refreshAccountLabelsAfterConfigChange()

	require.Equal(t, reflect.ValueOf(before).Pointer(), reflect.ValueOf(h.getSessionRenderSnapshot()).Pointer(),
		"saving settings with no label change must not rebuild the snapshot")
}

// A settings save before any session data exists must not invent rows.
func TestConfigChangeBeforeSnapshot(t *testing.T) {
	home := isolatedConfigHome(t)
	writeClaudeDirsConfig(t, home, map[string]string{"work": ".claude-work"}, nil)

	// A bare Home, not NewHome: this pins the pre-session path, and NewHome
	// publishes an empty snapshot of its own.
	h := &Home{}
	h.refreshAccountLabelsAfterConfigChange()
	require.Nil(t, h.getSessionRenderSnapshot(), "do not create session data during a settings change")
}

// A background refresh that started before a config change still publishes with
// the labels current at publication, not the ones it began with.
func TestPublicationUsesCurrentLabels(t *testing.T) {
	home := isolatedConfigHome(t)
	writeClaudeDirsConfig(t, home, map[string]string{"work": ".claude-work"}, nil)

	inst := claudeSession("work-session", "atlas-checker", "work")
	h := homeWithSessions(t, 240, inst)

	// In-flight refresh result, carrying a stale presentation.
	pending := map[string]sessionRenderState{
		inst.ID: {title: "fresh title", accountDisplay: newAccountPresentation("stale")},
	}
	h.publishSessionRenderSnapshot(pending)

	state := h.getSessionRenderState(inst)
	require.Equal(t, "fresh title", state.title, "the refresh's own data must survive")
	require.Equal(t, "work", state.accountDisplay.label, "but the label comes from the current resolution")
}

// A slot arriving from outside this process (another agent-deck writing
// state.db, which is what `session switch-account` does) must move the badge on
// the next refresh: that is how the operator sees the switch actually took.
func TestAccountChangeFromStorageMovesBadge(t *testing.T) {
	home := isolatedConfigHome(t)
	writeClaudeDirsConfig(t, home, nil, map[string]string{"claude-work": ".claude-work"})

	inst := claudeSession("switcher", "mos-rec", "my-sessions")
	h := homeWithSessions(t, 240, inst)
	require.Equal(t, accountPresentation{}, h.getSessionRenderState(inst).accountDisplay,
		"precondition: no slot, default dir, no badge")

	// What a reload hands back once the slot is set. Sequential mutation; no
	// concurrent writer, so a direct assignment is the honest fixture here.
	inst.Account = "claude-work"
	h.refreshAccountLabels()
	h.refreshSessionRenderSnapshot(h.instances)

	require.Equal(t, "work", h.getSessionRenderState(inst).accountDisplay.label)
	require.Contains(t, renderRow(t, h, inst, false), "[work]")
	require.Contains(t, h.renderSessionInfoCard(inst, 240, 40), "Claude config:")
}
