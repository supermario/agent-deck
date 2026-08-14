package statedb

import (
	"testing"
	"time"
)

// Reproduces the real case: a conversation started in one session ("kindle-view")
// and later resumed in another. Both rows ended up claiming the same Claude id,
// so the dead one went on mirroring the live one's transcript and idle time.
func TestReleaseClaudeSessionBindingFromOthers(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()

	for _, id := range []string{"born-here", "lives-here", "unrelated"} {
		if err := db.SaveInstance(&InstanceRow{
			ID: id, Title: id, ProjectPath: "/tmp", GroupPath: "g",
		}); err != nil {
			t.Fatalf("SaveInstance(%s): %v", id, err)
		}
	}

	const shared = "2c4096aa-1c90-4534-8a85-458e31c66e50"
	const other = "ffffffff-0000-0000-0000-000000000000"

	// Both claim the same conversation; a third claims its own.
	for _, b := range []struct{ id, sid string }{
		{"born-here", shared}, {"lives-here", shared}, {"unrelated", other},
	} {
		if err := db.WriteClaudeSessionBinding(b.id, b.sid, now); err != nil {
			t.Fatalf("WriteClaudeSessionBinding(%s): %v", b.id, err)
		}
	}

	released, err := db.ReleaseClaudeSessionBindingFromOthers("lives-here", shared)
	if err != nil {
		t.Fatalf("ReleaseClaudeSessionBindingFromOthers: %v", err)
	}
	if released != 1 {
		t.Fatalf("released = %d, want 1 (only born-here)", released)
	}

	got := readBindings(t, db)
	if got["lives-here"] != shared {
		t.Errorf("winner lost its binding: %q", got["lives-here"])
	}
	if got["born-here"] != "" {
		t.Errorf("stale claimant not released: %q", got["born-here"])
	}
	if got["unrelated"] != other {
		t.Errorf("unrelated session disturbed: %q", got["unrelated"])
	}

	// Idempotent, and a no-op once nothing else claims it.
	released, err = db.ReleaseClaudeSessionBindingFromOthers("lives-here", shared)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if released != 0 {
		t.Errorf("second call released %d, want 0", released)
	}

	// An empty session id must never match every unbound row.
	if released, err = db.ReleaseClaudeSessionBindingFromOthers("lives-here", ""); err != nil {
		t.Fatalf("empty session id: %v", err)
	} else if released != 0 {
		t.Errorf("empty session id released %d rows, want 0", released)
	}
	if readBindings(t, db)["unrelated"] != other {
		t.Error("empty session id clobbered an unrelated binding")
	}
}

func readBindings(t *testing.T, db *StateDB) map[string]string {
	t.Helper()
	rows, err := db.db.Query(
		`SELECT id, COALESCE(json_extract(tool_data, '$.claude_session_id'), '') FROM instances`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var id, sid string
		if err := rows.Scan(&id, &sid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[id] = sid
	}
	return out
}
