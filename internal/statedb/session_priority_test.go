package statedb

import "testing"

func TestSessionPriorities(t *testing.T) {
	db := newTestDB(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := db.SaveInstance(&InstanceRow{
			ID: id, Title: id, ProjectPath: "/tmp", GroupPath: "g",
		}); err != nil {
			t.Fatalf("SaveInstance(%s): %v", id, err)
		}
	}

	// Nothing ranked to begin with.
	if p, err := db.ReadSessionPriorities(); err != nil || len(p) != 0 {
		t.Fatalf("initial priorities = %v, %v; want empty", p, err)
	}

	if err := db.WriteSessionPriorities([]string{"c", "a", "b"}); err != nil {
		t.Fatalf("WriteSessionPriorities: %v", err)
	}
	got, err := db.ReadSessionPriorities()
	if err != nil {
		t.Fatalf("ReadSessionPriorities: %v", err)
	}
	for id, want := range map[string]int{"c": 1, "a": 2, "b": 3} {
		if got[id] != want {
			t.Errorf("priority[%s] = %d, want %d", id, got[id], want)
		}
	}
	if _, ranked := got["d"]; ranked {
		t.Errorf("d was never ordered but has priority %d", got["d"])
	}

	// Submitting a subset ranks that subset first and pushes the rest below,
	// keeping their relative order. The overlay can only ever submit the
	// sessions it can currently see, so anything else must survive untouched —
	// clearing them is what let a newly-opened session wipe the ordering.
	if err := db.WriteSessionPriorities([]string{"b", "c"}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	got, err = db.ReadSessionPriorities()
	if err != nil {
		t.Fatalf("ReadSessionPriorities after reorder: %v", err)
	}
	if got["b"] != 1 || got["c"] != 2 {
		t.Errorf("after reorder b=%d c=%d, want 1 and 2", got["b"], got["c"])
	}
	if got["a"] != 3 {
		t.Errorf("a was not submitted; want it carried to rank 3, got %d", got["a"])
	}
	if _, ranked := got["d"]; ranked {
		t.Errorf("d was never ranked but now has %d", got["d"])
	}

	// Ranks must stay a dense 1..N with no duplicates, or the sort is ambiguous.
	seen := map[int]string{}
	for id, r := range got {
		if prev, dup := seen[r]; dup {
			t.Errorf("rank %d shared by %s and %s", r, prev, id)
		}
		seen[r] = id
	}
	for i := 1; i <= len(got); i++ {
		if _, ok := seen[i]; !ok {
			t.Errorf("rank %d missing; ranks must be dense 1..%d", i, len(got))
		}
	}

	// Carried sessions keep their relative order when pushed down.
	if err := db.WriteSessionPriorities([]string{"d"}); err != nil {
		t.Fatalf("submit d: %v", err)
	}
	got, _ = db.ReadSessionPriorities()
	if got["d"] != 1 || got["b"] != 2 || got["c"] != 3 || got["a"] != 4 {
		t.Errorf("carry order wrong: d=%d b=%d c=%d a=%d, want 1,2,3,4",
			got["d"], got["b"], got["c"], got["a"])
	}

	// Explicit clear is its own operation now.
	if err := db.ClearSessionPriorities(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if p, _ := db.ReadSessionPriorities(); len(p) != 0 {
		t.Errorf("after clear = %v, want empty", p)
	}
}
