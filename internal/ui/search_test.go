package ui

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestNewSearch(t *testing.T) {
	s := NewSearch()

	if s == nil {
		t.Fatal("NewSearch returned nil")
	}
	if s.IsVisible() {
		t.Error("Search should not be visible by default")
	}
	if s.cursor != 0 {
		t.Error("Cursor should start at 0")
	}
}

func TestSearchSetItems(t *testing.T) {
	s := NewSearch()
	items := []*session.Instance{
		{Title: "session-1", ProjectPath: "/tmp/1", Tool: "claude"},
		{Title: "session-2", ProjectPath: "/tmp/2", Tool: "shell"},
	}

	s.SetItems(items)

	if len(s.allItems) != 2 {
		t.Errorf("Expected 2 items, got %d", len(s.allItems))
	}
}

func TestSearchVisibility(t *testing.T) {
	s := NewSearch()

	s.Show()
	if !s.IsVisible() {
		t.Error("Search should be visible after Show()")
	}

	s.Hide()
	if s.IsVisible() {
		t.Error("Search should not be visible after Hide()")
	}
}

// Show() must open with an empty query: a stale query left over from the
// previous search forced the user to manually clear it every time.
func TestSearchShowClearsPreviousQuery(t *testing.T) {
	s := NewSearch()
	s.SetItems([]*session.Instance{
		{Title: "alpha", ProjectPath: "/tmp/a", Tool: "claude"},
		{Title: "beta", ProjectPath: "/tmp/b", Tool: "shell"},
	})

	// First search: type a query that filters down to one result, then close.
	s.Show()
	s.input.SetValue("alpha")
	s.updateResults()
	if len(s.results) != 1 {
		t.Fatalf("setup: expected 'alpha' to match 1 item, got %d", len(s.results))
	}
	s.Hide()

	// Reopening must start fresh, not resume "alpha".
	s.Show()
	if got := s.input.Value(); got != "" {
		t.Errorf("Show() must clear the previous query, got %q", got)
	}
	if len(s.results) != 2 {
		t.Errorf("Show() must reset results to all items, got %d want 2", len(s.results))
	}
	if s.cursor != 0 {
		t.Errorf("Show() must reset cursor, got %d", s.cursor)
	}
}

// Show() clears the query but must NOT drop a group scope set immediately
// before it by openInGroupSearch (only Hide() clears the scope).
func TestSearchShowPreservesGroupScope(t *testing.T) {
	s := NewSearch()
	s.SetScopedGroup("proj/api")
	s.SetItems([]*session.Instance{
		{Title: "in-scope", ProjectPath: "/tmp/a", Tool: "claude", GroupPath: "proj/api"},
		{Title: "out-of-scope", ProjectPath: "/tmp/b", Tool: "shell", GroupPath: "other"},
	})

	s.Show()

	if s.scopedGroup != "proj/api" {
		t.Errorf("Show() must preserve group scope, got %q", s.scopedGroup)
	}
	if len(s.results) != 1 || s.results[0].Title != "in-scope" {
		t.Errorf("cleared query must list only in-scope items, got %d results", len(s.results))
	}
}

func TestSearchSelected(t *testing.T) {
	s := NewSearch()
	items := []*session.Instance{
		{Title: "session-1"},
		{Title: "session-2"},
	}
	s.SetItems(items)
	s.Show()

	selected := s.Selected()
	if selected == nil {
		t.Fatal("Selected should not be nil when items exist")
	}
	if selected.Title != "session-1" {
		t.Errorf("Expected session-1, got %s", selected.Title)
	}
}

func TestSearchSetSize(t *testing.T) {
	s := NewSearch()
	s.SetSize(100, 50)

	if s.width != 100 {
		t.Errorf("Width = %d, want 100", s.width)
	}
	if s.height != 50 {
		t.Errorf("Height = %d, want 50", s.height)
	}
}

func TestSearchView(t *testing.T) {
	s := NewSearch()

	// Not visible - should return empty
	view := s.View()
	if view != "" {
		t.Error("View should be empty when not visible")
	}

	// Visible - should return content
	s.SetSize(80, 24)
	s.Show()
	view = s.View()
	if view == "" {
		t.Error("View should not be empty when visible")
	}
}
