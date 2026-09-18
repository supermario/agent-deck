package ui

import (
	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestStorageWatcherAccountOneRefreshComposition(t *testing.T) {
	h, storage, inst := newWatcherEffectsHome(t)
	w, err := NewStorageWatcher(storage.GetDB())
	require.NoError(t, err)
	defer w.Close()
	h.storageWatcher = w
	settleWatcherInitialLoad(t, w, storage.GetDB())
	old := h.sessionLoadCmd(nil, false)()
	writer, err := statedb.Open(storage.Path())
	require.NoError(t, err)
	defer writer.Close()
	_, err = writer.DB().Exec("UPDATE instances SET account=? WHERE id=?", "reviewer", inst.ID)
	require.NoError(t, err)
	w.checkAndNotify()
	requireWatcherSignal(t, w)
	_, batch := h.Update(storageChangedMsg{})
	commands := batch().(tea.BatchMsg)
	_, _ = h.Update(commands[0]())
	loaded := h.getInstanceByID(inst.ID)
	require.Equal(t, "reviewer", loaded.GetAccountThreadSafe())
	require.Equal(t, "reviewer", h.getSessionRenderState(loaded).account)
	var row strings.Builder
	h.renderSessionItem(&row, session.Item{Type: session.ItemTypeSession, Session: loaded, Level: 1, Path: "work", IsLastInGroup: true}, false, h.getSessionRenderSnapshot(), 240)
	// The badge names the resolved Claude config dir, not the stored slot. This
	// machine declares no [profiles.reviewer.claude] block, so "reviewer"
	// resolves to the default dir and the row must not invent a label for it.
	// The slot itself still has to survive the reload, which is the subject here.
	require.NotContains(t, row.String(), "[work")
	require.Equal(t, accountPresentation{}, h.getSessionRenderState(loaded).accountDisplay)
	require.NotContains(t, h.renderSessionInfoCard(loaded, 240, 40), "Claude config:")
	_, _ = h.Update(old)
	require.Equal(t, "reviewer", h.getSessionRenderState(h.getInstanceByID(inst.ID)).account, "late old load reverted account snapshot")
}
