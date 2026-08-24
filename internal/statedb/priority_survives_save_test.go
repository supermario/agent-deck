package statedb

import "testing"

// priority is written by a targeted single-column UPDATE and is deliberately
// absent from SaveInstance's column list. With INSERT OR REPLACE that meant the
// row was deleted and re-inserted on every save, silently resetting the column
// to its default — so opening a session, or any status change, wiped the user's
// whole manual ordering.
func TestPrioritySurvivesSaveInstance(t *testing.T) {
	db := newTestDB(t)
	row := &InstanceRow{ID: "s1", Title: "one", ProjectPath: "/tmp", GroupPath: "g"}
	if err := db.SaveInstance(row); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	if err := db.WriteSessionPriorities([]string{"s1"}); err != nil {
		t.Fatalf("WriteSessionPriorities: %v", err)
	}

	// A save carrying no knowledge of priority — exactly what every existing
	// caller does — must not disturb it.
	row.Title = "one (renamed)"
	if err := db.SaveInstance(row); err != nil {
		t.Fatalf("second SaveInstance: %v", err)
	}
	got, err := db.ReadSessionPriorities()
	if err != nil {
		t.Fatalf("ReadSessionPriorities: %v", err)
	}
	if got["s1"] != 1 {
		t.Errorf("after SaveInstance priority = %d, want 1 (preserved)", got["s1"])
	}

	// The save itself must still have applied.
	rows, err := db.LoadInstances()
	if err != nil {
		t.Fatalf("LoadInstances: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.ID == "s1" {
			found = true
			if r.Title != "one (renamed)" {
				t.Errorf("title = %q, want the updated value", r.Title)
			}
		}
	}
	if !found {
		t.Error("row missing after save")
	}
}

// SaveInstances (the bulk path) has the same column list and the same hazard.
func TestPrioritySurvivesSaveInstances(t *testing.T) {
	db := newTestDB(t)
	rows := []*InstanceRow{
		{ID: "a", Title: "a", ProjectPath: "/tmp", GroupPath: "g"},
		{ID: "b", Title: "b", ProjectPath: "/tmp", GroupPath: "g"},
	}
	if err := db.SaveInstances(rows); err != nil {
		t.Fatalf("SaveInstances: %v", err)
	}
	if err := db.WriteSessionPriorities([]string{"b", "a"}); err != nil {
		t.Fatalf("WriteSessionPriorities: %v", err)
	}
	if err := db.SaveInstances(rows); err != nil {
		t.Fatalf("second SaveInstances: %v", err)
	}
	got, _ := db.ReadSessionPriorities()
	if got["b"] != 1 || got["a"] != 2 {
		t.Errorf("after SaveInstances b=%d a=%d, want 1 and 2 (preserved)", got["b"], got["a"])
	}
}
